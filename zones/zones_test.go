package zones

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/schema"
)

func committed(t *testing.T) *Document {
	t.Helper()
	d, err := Load("testdata/zones.json")
	if err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Load("../fleet/testdata/fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckApps(f); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTheSampleZonesAreValid(t *testing.T) {
	d := committed(t)
	if len(d.Zones) != 1 || d.Zones[0].Hardening.RateLimit.Limit() != 5 {
		t.Fatalf("zones %+v", d.Zones)
	}
}

func TestTheCommittedSchemaIsCurrent(t *testing.T) {
	got, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, schema.Zones()) {
		t.Fatal("schema/zones.schema.json is out of date; run go generate ./schema")
	}
}

func TestThePageListsTheRecords(t *testing.T) {
	page := committed(t).Page()
	for _, want := range []string{"example.com", "app.example.com", "deployforge zones harden"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
}

func doc(rs ...Record) *Document {
	return &Document{FormatVersion: FormatVersion, Zones: []Zone{{Name: "example.com", Registrar: "x", DNS: Cloudflare, Records: rs}}}
}

func TestValidateRejects(t *testing.T) {
	for name, r := range map[string]Record{
		"bad address":      {Name: "@", Type: "A", Values: []string{"nope"}, Purpose: App, Source: "s/prod"},
		"proxied MX":       {Name: "@", Type: "MX", Values: []string{"10 mx.example.com"}, Proxied: true, Purpose: Mail},
		"site no repo":     {Name: "@", Type: "A", Values: []string{"192.0.2.1"}, Purpose: StaticSite},
		"bad purpose":      {Name: "@", Type: "A", Values: []string{"192.0.2.1"}, Purpose: "other"},
		"two CNAMEs":       {Name: "www", Type: "CNAME", Values: []string{"a.example.com", "b.example.com"}, Purpose: Parked},
		"unproxied Worker": {Name: "@", Type: "WORKER", Values: []string{"www"}, Purpose: StaticSite, Source: "github.com/example/www"},
		"bad MX":           {Name: "@", Type: "MX", Values: []string{"mx.example.com"}, Purpose: Mail},
		"app not a stack":  {Name: "app", Type: "A", Values: []string{"192.0.2.1"}, Purpose: App, Source: "site"},
	} {
		if err := doc(r).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dup := Record{Name: "@", Type: "TXT", Values: []string{"a"}, Purpose: Mail}
	if err := doc(dup, dup).Validate(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a name and type listed twice: %v", err)
	}
}

func TestCheckAppsWantsEveryLiveHost(t *testing.T) {
	f, err := fleet.Load("../fleet/testdata/fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc().CheckApps(f); err == nil || !strings.Contains(err.Error(), "app.example.com serves example-app/prod") {
		t.Errorf("a live host with no record: %v", err)
	}
	app := Record{Name: "app", Type: "A", Values: []string{"192.0.2.1"}, Proxied: true, Purpose: App, Source: "example-app/prod"}
	auth := app
	auth.Name = "auth"
	if err := doc(app, auth).CheckApps(f); err != nil {
		t.Errorf("with its records: %v", err)
	}
	app.Source = "other/prod"
	if err := doc(app, auth).CheckApps(f); err == nil {
		t.Error("an app record naming no app was accepted")
	}
}

// fake answers from a table keyed by name and type.
func fake(answers map[string][]dnsmessage.Resource) Querier {
	return func(_ context.Context, _, name string, typ dnsmessage.Type) ([]dnsmessage.Resource, error) {
		return answers[name+" "+typ.String()], nil
	}
}

func a(ip string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Type: dnsmessage.TypeA}, Body: &dnsmessage.AResource{A: netip.MustParseAddr(ip).As4()}}
}

func mx(pref uint16, host string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Type: dnsmessage.TypeMX}, Body: &dnsmessage.MXResource{Pref: pref, MX: dnsmessage.MustNewName(host)}}
}

func txt(s string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Type: dnsmessage.TypeTXT}, Body: &dnsmessage.TXTResource{TXT: []string{s}}}
}

func TestVerify(t *testing.T) {
	d := doc(
		Record{Name: "@", Type: "A", Values: []string{"192.0.2.1"}, Proxied: true, Purpose: Redirect},
		Record{Name: "www", Type: "A", Values: []string{"192.0.2.1"}, Proxied: true, Purpose: Redirect},
		Record{Name: "app", Type: "A", Values: []string{"192.0.2.1"}, Proxied: true, Purpose: App, Source: "s/prod"},
		Record{Name: "old", Type: "A", Values: []string{"198.51.100.1", "198.51.100.2"}, Purpose: Parked},
		Record{Name: "@", Type: "MX", Values: []string{"0 ."}, Purpose: Mail},
		Record{Name: "new", Type: "A", Values: []string{"192.0.2.9"}, Purpose: App, Source: "s/prod", Status: Planned},
		Record{Name: "@", Type: "TXT", Values: []string{"v=spf1 -all"}, Purpose: Mail, Status: Planned},
		Record{Name: "site", Type: "WORKER", Values: []string{"example-www"}, Proxied: true, Purpose: StaticSite, Source: "github.com/example/www"},
	)
	v := &Verifier{
		Cloudflare: []netip.Prefix{netip.MustParsePrefix("104.16.0.0/13")},
		Query: fake(map[string][]dnsmessage.Resource{
			"example.com TypeA":      {a("104.21.0.1")},
			"www.example.com TypeA":  {a("192.0.2.1")}, // not proxied
			"old.example.com TypeA":  {a("198.51.100.1")},
			"example.com TypeMX":     {mx(0, ".")},
			"example.com TypeTXT":    {txt("v=spf1 include:_spf.example.net ~all")},
			"site.example.com TypeA": {a("104.21.0.2")},
		}),
	}
	found, err := v.Verify(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Outcome{
		"example.com A": OK, "www.example.com A": Fail, "app.example.com A": Fail,
		"old.example.com A": Fail, "example.com MX": OK, "new.example.com A": Skip,
		"example.com TXT": Fail, "site.example.com WORKER": OK,
	}
	for _, f := range found {
		if want[f.Name+" "+f.Type] != f.Outcome {
			t.Errorf("%s: %s, want %s", f, f.Outcome, want[f.Name+" "+f.Type])
		}
	}
	if len(found) != len(want) {
		t.Errorf("%d findings, want %d", len(found), len(want))
	}
	if _, err := v.Verify(context.Background(), d, "other.com"); err == nil {
		t.Error("a zone not in the document was accepted")
	}
}

type failTransport struct {
	t     *testing.T
	allow *bool
}

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !*f.allow {
		f.t.Errorf("a records-only check requested %s", r.URL)
	}
	return nil, errors.New("no requests")
}

func TestVerifyRecordsOnly(t *testing.T) {
	allow := false
	d := doc(Record{Name: "app", Type: "A", Values: []string{"192.0.2.1"}, Proxied: true, Purpose: App, Source: "s/prod",
		Check: &HTTPCheck{Path: "/readyz", Status: 200}})
	v := &Verifier{
		Cloudflare:  []netip.Prefix{netip.MustParsePrefix("104.16.0.0/13")},
		Query:       fake(map[string][]dnsmessage.Resource{"app.example.com TypeA": {a("104.21.0.1")}}),
		HTTP:        &http.Client{Transport: failTransport{t, &allow}},
		RecordsOnly: true,
	}
	found, err := v.Verify(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Type != "A" || found[0].Outcome != OK {
		t.Errorf("found %v, want only the A record, ok", found)
	}
	v.RecordsOnly, allow = false, true
	if found, err = v.Verify(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[1].Type != "HTTPS" || found[1].Outcome != Fail {
		t.Errorf("found %v, want the HTTPS check too, failed", found)
	}
}

func TestDialingReachesTheZonesAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// example.com is in the test server's certificate; no resolver is
	// asked, and an address that refuses is passed over.
	tr := dialing("example.com", []string{"::1", "127.0.0.1"})
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestZoneOfAndGet(t *testing.T) {
	d := doc(Record{Name: "app", Type: "A", Values: []string{"192.0.2.1"}, Proxied: true, Purpose: App, Source: "s/prod"})
	for host, want := range map[string]string{"app.example.com": "example.com", "example.com": "example.com", "a.b.example.com": "example.com", "example.org": "", "badexample.com": ""} {
		if got, ok := d.ZoneOf(host); got != want || ok != (want != "") {
			t.Errorf("ZoneOf(%s) = %q, %v; want %q", host, got, ok, want)
		}
	}
	v := &Verifier{Query: fake(map[string][]dnsmessage.Resource{})}
	if _, err := v.Get(context.Background(), "example.com", "new.example.com", &HTTPCheck{Path: "/", Status: 200}); !errors.Is(err, errNoRecord) {
		t.Errorf("a name the zone does not answer: %v", err)
	}
}

func TestVerifyFailPlanned(t *testing.T) {
	d := doc(Record{Name: "new", Type: "A", Values: []string{"192.0.2.9"}, Proxied: true, Purpose: App, Source: "s/prod", Status: Planned})
	v := &Verifier{Cloudflare: []netip.Prefix{netip.MustParsePrefix("104.16.0.0/13")}, Query: fake(map[string][]dnsmessage.Resource{})}
	found, err := v.Verify(context.Background(), d)
	if err != nil || len(found) != 1 || found[0].Outcome != Skip {
		t.Fatalf("a planned record: %v %v", found, err)
	}
	v.FailPlanned = true
	if found, _ = v.Verify(context.Background(), d); found[0].Outcome != Fail {
		t.Errorf("with FailPlanned, a planned record with no answer: %v", found)
	}
	v.Query = fake(map[string][]dnsmessage.Resource{"new.example.com TypeA": {a("104.21.0.9")}})
	if found, _ = v.Verify(context.Background(), d); found[0].Outcome != Fail || !strings.Contains(found[0].Detail, "mark it present") {
		t.Errorf("with FailPlanned, a planned record the zone answers: %v", found)
	}
}
