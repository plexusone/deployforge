package zoneharden

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"

	"github.com/plexusone/deployforge/zones"
)

func testZone() zones.Zone {
	return zones.Zone{
		Name: "example.com",
		Records: []zones.Record{
			{Name: "app", Type: "A", Values: []string{"192.0.2.1"}},
			{Name: "auth", Type: "A", Values: []string{"192.0.2.1"}},
		},
		Hardening: &zones.Hardening{AllowCountries: []string{"US"}, CountryHosts: []string{"app", "auth"}, BotFightModeOff: true, WWWRedirect: true,
			RateLimit: &zones.RateLimit{Path: "/login-token", WhenRecord: "auth"}, NoCachePrefixes: []string{"/api/"}},
	}
}

func TestDesired(t *testing.T) {
	want, err := Desired(testZone(), "managed-1")
	if err != nil {
		t.Fatal(err)
	}
	custom := want[PhaseCustom]
	if len(custom) != 3 || !strings.Contains(custom[2].Expression, `http.host in {"app.example.com" "auth.example.com"} and not ip.src.country in {"US"}`) {
		t.Errorf("custom rules: %+v", custom)
	}
	if !strings.Contains(custom[0].Expression, `starts_with(http.request.uri.path, "/.env")`) || !strings.Contains(custom[1].Expression, `"OPTIONS"`) {
		t.Errorf("paths and methods: %+v", custom)
	}
	if r := want[PhaseRate]; len(r) != 1 || r[0].Ratelimit["requests_per_period"] != 5 || r[0].Expression != `http.request.uri.path eq "/login-token"` {
		t.Errorf("rate limit: %+v", r)
	}
	if c := want[PhaseCache]; len(c) != 1 || !strings.Contains(c[0].Expression, `starts_with(http.request.uri.path, "/api/")`) || strings.Contains(c[0].Expression, "/bff/") {
		t.Errorf("cache rule: %+v", c)
	}
	if r := want[PhaseManaged]; len(r) != 1 || r[0].ActionParameters["id"] != "managed-1" {
		t.Errorf("managed: %+v", r)
	}

	// Without the record the rate limit waits for, there is no rate limit.
	z := testZone()
	z.Records = z.Records[:1]
	if want, err := Desired(z, ""); err != nil || want[PhaseRate] != nil {
		t.Errorf("a rate limit before its record: %+v, %v", want[PhaseRate], err)
	}
	// Without a rate limit or countries, neither rule is made.
	z.Hardening = &zones.Hardening{}
	want, err = Desired(z, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := want[PhaseRate]; ok || len(want[PhaseCustom]) != 2 || want[PhaseManaged] != nil || want[PhaseRedirect] != nil {
		t.Errorf("a plain zone: %+v", want)
	}
	if _, err := Desired(zones.Zone{Name: "x.com"}, ""); err == nil {
		t.Error("a zone without hardening was accepted")
	}
}

func TestMatchesDashboardRules(t *testing.T) {
	want := Rule{Action: "redirect", Expression: `http.host eq "www.example.com"`,
		ActionParameters: map[string]any{"from_value": map[string]any{"status_code": 301}}}
	dashboard := Rule{Action: "redirect", Expression: `(http.host eq "www.example.com")`,
		ActionParameters: map[string]any{"from_value": map[string]any{"status_code": float64(301), "preserve_query_string": true}}}
	if !Matches(want, dashboard) {
		t.Error("a dashboard rule with parentheses and more fields does not match")
	}
	off := false
	dashboard.Enabled = &off
	if Matches(want, dashboard) {
		t.Error("a disabled rule matches")
	}
	if normalize("(a) or (b)") != "(a) or (b)" {
		t.Error("parentheses that do not enclose the whole were dropped")
	}
}

func TestRunAppliesThenVerifies(t *testing.T) {
	f := newFakeCloudflare()
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := cf.NewClient(option.WithAPIToken("test"), option.WithBaseURL(srv.URL+"/"), option.WithMaxRetries(0))
	ctx := context.Background()
	zs := []zones.Zone{testZone()}
	// A rule made in the dashboard stays.
	f.phases[PhaseRedirect] = []Rule{{Ref: "dash", Description: "by hand", Action: "redirect", Expression: `(http.host eq "old.example.com")`}}

	var out bytes.Buffer
	res, err := Run(ctx, api, Plan, zs, &out)
	// 4 settings (ssl is already strict), Bot Fight Mode, 5 phases.
	if err != nil || res.Missing != 10 || f.writes != 0 {
		t.Fatalf("plan: %+v %v, %d writes\n%s", res, err, f.writes, out.String())
	}
	if _, err := Run(ctx, api, Verify, zs, &out); !errors.Is(err, ErrNotInPlace) {
		t.Errorf("verify before apply: %v", err)
	}

	out.Reset()
	if _, err := Run(ctx, api, Apply, zs, &out); err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	if f.settings["min_tls_version"] != "1.2" || f.fightMode {
		t.Errorf("settings: %v, fight mode %v", f.settings, f.fightMode)
	}
	if r := f.phases[PhaseRedirect]; len(r) != 2 || r[0].Ref != "dash" || r[1].Ref != RefPrefix+"www" {
		t.Errorf("redirects: %+v", r)
	}
	if !strings.Contains(out.String(), `kept     http_request_dynamic_redirect rule "by hand"`) {
		t.Errorf("the dashboard rule was not reported:\n%s", out.String())
	}

	// Again, nothing changes.
	writes := f.writes
	if res, err := Run(ctx, api, Verify, zs, &out); err != nil || res.Missing != 0 || f.writes != writes {
		t.Fatalf("verify: %+v %v", res, err)
	}

	// A rule of this tool's that no longer matches is replaced, not kept.
	f.phases[PhaseCustom][1].Expression = "true"
	if _, err := Run(ctx, api, Apply, zs, &out); err != nil {
		t.Fatal(err)
	}
	if r := f.phases[PhaseCustom]; len(r) != 3 || !strings.Contains(r[1].Expression, "http.request.method") {
		t.Errorf("custom after repair: %+v", r)
	}
}

// fakeCloudflare answers the calls a run makes, for one zone, z1.
type fakeCloudflare struct {
	mu        sync.Mutex
	settings  map[string]string
	fightMode bool
	phases    map[string][]Rule
	writes    int
}

func newFakeCloudflare() *fakeCloudflare {
	return &fakeCloudflare{
		settings: map[string]string{"ssl": "strict", "always_use_https": "off", "min_tls_version": "1.0", "rocket_loader": "on", "email_obfuscation": "on"},
		// Bot Fight Mode is on.
		fightMode: true,
		phases:    map[string][]Rule{},
	}
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result}); err != nil {
			panic(err)
		}
	}
	var body map[string]json.RawMessage
	if r.Method != http.MethodGet {
		f.writes++
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	p := r.URL.Path
	switch {
	case p == "/zones" && r.URL.Query().Get("name") == "example.com":
		reply([]map[string]string{{"id": "z1", "name": "example.com"}})
	case strings.HasPrefix(p, "/zones/z1/settings/"):
		id := strings.TrimPrefix(p, "/zones/z1/settings/")
		if r.Method == http.MethodPatch {
			var v string
			if err := json.Unmarshal(body["value"], &v); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.settings[id] = v
		}
		reply(map[string]any{"id": id, "value": f.settings[id]})
	case p == "/zones/z1/bot_management":
		if r.Method == http.MethodPut {
			if err := json.Unmarshal(body["fight_mode"], &f.fightMode); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		reply(map[string]any{"fight_mode": f.fightMode})
	case p == "/zones/z1/rulesets":
		reply([]map[string]string{{"id": "managed-1", "name": FreeManagedRuleset, "kind": "managed"}, {"id": "x", "name": "Other", "kind": "zone"}})
	case strings.HasPrefix(p, "/zones/z1/rulesets/phases/") && strings.HasSuffix(p, "/entrypoint"):
		phase := strings.TrimSuffix(strings.TrimPrefix(p, "/zones/z1/rulesets/phases/"), "/entrypoint")
		if r.Method == http.MethodPut {
			var rules []Rule
			if err := json.Unmarshal(body["rules"], &rules); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.phases[phase] = rules
		}
		rules, ok := f.phases[phase]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			if _, err := w.Write([]byte(`{"success":false,"errors":[{"code":10003,"message":"not found"}],"result":null}`)); err != nil {
				panic(err)
			}
			return
		}
		// Cloudflare adds fields of its own to each rule.
		out := make([]map[string]any, len(rules))
		for i, rl := range rules {
			b, err := json.Marshal(rl)
			if err != nil {
				panic(err)
			}
			if err := json.Unmarshal(b, &out[i]); err != nil {
				panic(err)
			}
			out[i]["id"], out[i]["version"] = "r", "1"
		}
		reply(map[string]any{"rules": out})
	default:
		http.NotFound(w, r)
	}
}
