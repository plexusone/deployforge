// Package zoneharden applies a security baseline to a Cloudflare zone
// through Cloudflare's API, in place of the dashboard: the TLS settings, the
// custom rules, a rate limit on one path, the Free Managed Ruleset, the
// cache rule and the www redirect.
//
// Rules this package writes carry a ref beginning with RefPrefix. A phase's
// other rules, such as ones made in the dashboard, are kept as they are and
// listed; a desired rule that one of them already matches is not added
// again. Plan says what apply would change; apply changes it, then
// verifies; verify fails unless everything is in place.
package zoneharden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"

	"github.com/plexusone/deployforge/zones"
)

// Mode is what a run does.
type Mode string

// The modes.
const (
	// Plan says what is missing and what apply would do; it changes nothing.
	Plan Mode = "plan"
	// Apply changes what is missing, then verifies.
	Apply Mode = "apply"
	// Verify checks that everything is in place.
	Verify Mode = "verify"
)

// Modes are the valid modes.
var Modes = []Mode{Plan, Apply, Verify}

// RefPrefix marks the rules this package writes.
const RefPrefix = "fleet-"

// FreeManagedRuleset is the name of the managed ruleset the Free plan
// may deploy.
const FreeManagedRuleset = "Cloudflare Free Managed Ruleset"

// Phases of the rulesets this package manages.
const (
	PhaseCustom   = "http_request_firewall_custom"
	PhaseManaged  = "http_request_firewall_managed"
	PhaseRate     = "http_ratelimit"
	PhaseCache    = "http_request_cache_settings"
	PhaseRedirect = "http_request_dynamic_redirect"
)

// API is the part of the Cloudflare client a run uses: its generic REST
// calls, relative to the API's base URL.
type API interface {
	Get(ctx context.Context, path string, params any, res any, opts ...option.RequestOption) error
	Put(ctx context.Context, path string, params any, res any, opts ...option.RequestOption) error
	Patch(ctx context.Context, path string, params any, res any, opts ...option.RequestOption) error
}

// Setting is a zone setting and the value the baseline wants.
type Setting struct {
	ID    string
	Value string
}

// Settings are the baseline's zone settings.
var Settings = []Setting{
	{ID: "ssl", Value: "strict"},
	{ID: "always_use_https", Value: "on"},
	{ID: "min_tls_version", Value: "1.2"},
	{ID: "rocket_loader", Value: "off"},
	{ID: "email_obfuscation", Value: "off"},
}

// Rule is a ruleset rule as Cloudflare's API takes it.
type Rule struct {
	Ref              string         `json:"ref,omitempty"`
	Description      string         `json:"description,omitempty"`
	Expression       string         `json:"expression"`
	Action           string         `json:"action"`
	ActionParameters map[string]any `json:"action_parameters,omitempty"`
	Ratelimit        map[string]any `json:"ratelimit,omitempty"`
	Enabled          *bool          `json:"enabled,omitempty"`
}

// ScannedPaths are the paths scanners probe that no app serves.
var ScannedPaths = []string{"/.env", "/.git", "/wp-admin", "/phpmyadmin", "/xmlrpc.php"}

// Methods are the HTTP methods apps use; any other is blocked.
var Methods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// Desired is a zone's baseline: its settings and each phase's rules.
// managedID is the Free Managed Ruleset's ID in the zone.
func Desired(z zones.Zone, managedID string) (map[string][]Rule, error) {
	h := z.Hardening
	if h == nil {
		return nil, fmt.Errorf("zoneharden: %s has no hardening in zones.json", z.Name)
	}
	quoted := func(ss []string) string {
		q := make([]string, len(ss))
		for i, s := range ss {
			q[i] = fmt.Sprintf("%q", s)
		}
		return strings.Join(q, " ")
	}
	var paths []string
	for _, p := range ScannedPaths {
		paths = append(paths, fmt.Sprintf("starts_with(http.request.uri.path, %q)", p))
	}
	custom := []Rule{
		{Ref: RefPrefix + "scanned-paths", Description: "Block paths scanners probe that no app serves",
			Expression: strings.Join(paths, " or "), Action: "block"},
		{Ref: RefPrefix + "methods", Description: "Block methods no app uses",
			Expression: "not http.request.method in {" + quoted(Methods) + "}", Action: "block"},
	}
	if len(h.AllowCountries) > 0 {
		var hosts []string
		for _, l := range h.CountryHosts {
			hosts = append(hosts, l+"."+z.Name)
		}
		if len(hosts) == 0 {
			return nil, fmt.Errorf("zoneharden: %s allows countries on no hosts", z.Name)
		}
		custom = append(custom, Rule{Ref: RefPrefix + "countries", Description: "Block other countries on the app hosts",
			Expression: "http.host in {" + quoted(hosts) + "} and not ip.src.country in {" + quoted(h.AllowCountries) + "}", Action: "block"})
	}
	noCache := []string{`http.request.uri.path eq "/"`, `ends_with(http.request.uri.path, ".html")`}
	for _, prefix := range h.NoCachePrefixes {
		noCache = append(noCache, fmt.Sprintf("starts_with(http.request.uri.path, %q)", prefix))
	}
	out := map[string][]Rule{
		PhaseCustom: custom,
		PhaseCache: {{Ref: RefPrefix + "no-cache", Description: "Never cache documents or the paths named",
			Expression: strings.Join(noCache, " or "),
			Action:     "set_cache_settings", ActionParameters: map[string]any{"cache": false}}},
	}
	if managedID != "" {
		out[PhaseManaged] = []Rule{{Ref: RefPrefix + "free-managed", Description: FreeManagedRuleset,
			Expression: "true", Action: "execute", ActionParameters: map[string]any{"id": managedID}}}
	}
	if rl := h.RateLimit; rl != nil && (rl.WhenRecord == "" || hasPresent(z, rl.WhenRecord)) {
		// The Free plan allows one rule, keyed by IP and data center, with a
		// 10-second period and timeout.
		out[PhaseRate] = []Rule{{Ref: RefPrefix + "rate-limit", Description: "Limit requests to " + rl.Path + " per address",
			Expression: fmt.Sprintf("http.request.uri.path eq %q", rl.Path), Action: "block",
			Ratelimit: map[string]any{"characteristics": []any{"ip.src", "cf.colo.id"}, "period": 10, "requests_per_period": rl.Limit(), "mitigation_timeout": 10}}}
	}
	if h.WWWRedirect {
		out[PhaseRedirect] = []Rule{{Ref: RefPrefix + "www", Description: "www to the apex",
			Expression: fmt.Sprintf("http.host eq %q", "www."+z.Name), Action: "redirect",
			ActionParameters: map[string]any{"from_value": map[string]any{
				"status_code":           301,
				"target_url":            map[string]any{"expression": fmt.Sprintf("concat(%q, http.request.uri.path)", "https://"+z.Name)},
				"preserve_query_string": true,
			}}}}
	}
	return out, nil
}

// hasPresent reports whether the zone has a present record named label.
func hasPresent(z zones.Zone, label string) bool {
	for _, r := range z.Records {
		if r.Name == label && r.State() == zones.Present {
			return true
		}
	}
	return false
}

// ErrNotInPlace is returned by verify, and by apply when its final check
// fails, when anything differs from the baseline.
var ErrNotInPlace = errors.New("zoneharden: not everything is in place")

// Result is what a run found.
type Result struct {
	// Missing counts the settings and phases that differ from the
	// baseline, or that apply could not confirm.
	Missing int
}

// Run plans, applies or verifies the baseline on each zone, reporting to
// out.
func Run(ctx context.Context, api API, mode Mode, zs []zones.Zone, out io.Writer) (Result, error) {
	if !slices.Contains(Modes, mode) {
		return Result{}, fmt.Errorf("zoneharden: mode %q is not one of %v", mode, Modes)
	}
	r := &run{api: api, mode: mode, out: out}
	all := func() error {
		for _, z := range zs {
			if err := r.zone(ctx, z); err != nil {
				return err
			}
		}
		return nil
	}
	if err := all(); err != nil {
		return Result{}, err
	}
	if mode == Apply {
		r.mode, r.missing = Verify, 0
		if err := all(); err != nil {
			return Result{}, err
		}
		r.mode = Apply
	}
	if r.missing > 0 {
		r.say("%d item(s) need attention", r.missing)
		if mode == Plan {
			return Result{Missing: r.missing}, r.err
		}
		return Result{Missing: r.missing}, ErrNotInPlace
	}
	r.say("everything is in place")
	return Result{}, r.err
}

type run struct {
	api     API
	mode    Mode
	out     io.Writer
	missing int
	err     error
}

func (r *run) say(format string, a ...any) {
	if r.err == nil {
		_, r.err = fmt.Fprintf(r.out, format+"\n", a...)
	}
}

// differs reports an item that differs from the baseline, and whether to
// change it.
func (r *run) differs(item, fix string) bool {
	if r.mode == Apply {
		r.say("  apply    %s: %s", item, fix)
		return true
	}
	r.missing++
	r.say("  missing  %s: apply would %s", item, fix)
	return false
}

type envelope[T any] struct {
	Result T `json:"result"`
}

func (r *run) zone(ctx context.Context, z zones.Zone) error {
	r.say("%s", z.Name)
	var found envelope[[]struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}]
	if err := r.api.Get(ctx, "zones?name="+url.QueryEscape(z.Name), nil, &found); err != nil {
		return fmt.Errorf("zoneharden: finding %s: %w", z.Name, err)
	}
	if len(found.Result) != 1 {
		return fmt.Errorf("zoneharden: the token sees %d zones named %s; it needs Zone Read on it", len(found.Result), z.Name)
	}
	id := found.Result[0].ID
	for _, s := range Settings {
		if err := r.setting(ctx, id, s); err != nil {
			return err
		}
	}
	if z.Hardening != nil && z.Hardening.BotFightModeOff {
		if err := r.botFightMode(ctx, id); err != nil {
			return err
		}
	}
	managedID, err := r.freeManaged(ctx, id)
	if err != nil {
		return err
	}
	want, err := Desired(z, managedID)
	if err != nil {
		return err
	}
	for _, phase := range []string{PhaseCustom, PhaseManaged, PhaseRate, PhaseCache, PhaseRedirect} {
		if rules, ok := want[phase]; ok {
			if err := r.phase(ctx, id, phase, rules); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *run) setting(ctx context.Context, zoneID string, s Setting) error {
	var got envelope[struct {
		Value    any  `json:"value"`
		Editable bool `json:"editable"`
	}]
	path := "zones/" + zoneID + "/settings/" + s.ID
	if err := r.api.Get(ctx, path, nil, &got); err != nil {
		return fmt.Errorf("zoneharden: reading %s: %w", s.ID, err)
	}
	have := fmt.Sprint(got.Result.Value)
	if have == s.Value {
		r.say("  ok       %s %s", s.ID, s.Value)
		return nil
	}
	if r.differs(s.ID+" is "+have, "set it to "+s.Value) {
		if err := r.api.Patch(ctx, path, map[string]any{"value": s.Value}, nil); err != nil {
			return fmt.Errorf("zoneharden: setting %s: %w", s.ID, err)
		}
	}
	return nil
}

func (r *run) botFightMode(ctx context.Context, zoneID string) error {
	var got envelope[struct {
		FightMode bool `json:"fight_mode"`
	}]
	path := "zones/" + zoneID + "/bot_management"
	if err := r.api.Get(ctx, path, nil, &got); err != nil {
		return fmt.Errorf("zoneharden: reading Bot Fight Mode: %w", err)
	}
	if !got.Result.FightMode {
		r.say("  ok       Bot Fight Mode off")
		return nil
	}
	if r.differs("Bot Fight Mode is on", "turn it off") {
		if err := r.api.Put(ctx, path, map[string]any{"fight_mode": false}, nil); err != nil {
			return fmt.Errorf("zoneharden: turning Bot Fight Mode off: %w", err)
		}
	}
	return nil
}

// freeManaged returns the Free Managed Ruleset's ID in the zone, or ""
// when the zone's plan offers none.
func (r *run) freeManaged(ctx context.Context, zoneID string) (string, error) {
	var got envelope[[]struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}]
	if err := r.api.Get(ctx, "zones/"+zoneID+"/rulesets", nil, &got); err != nil {
		return "", fmt.Errorf("zoneharden: listing rulesets: %w", err)
	}
	for _, rs := range got.Result {
		if rs.Kind == "managed" && rs.Name == FreeManagedRuleset {
			return rs.ID, nil
		}
	}
	r.say("  note     no %s is offered to the zone", FreeManagedRuleset)
	return "", nil
}

func (r *run) phase(ctx context.Context, zoneID, phase string, want []Rule) error {
	var got envelope[struct {
		Rules []Rule `json:"rules"`
	}]
	path := "zones/" + zoneID + "/rulesets/phases/" + phase + "/entrypoint"
	if err := r.api.Get(ctx, path, nil, &got); err != nil && !isNotFound(err) {
		return fmt.Errorf("zoneharden: reading %s: %w", phase, err)
	}
	var others []Rule
	stale := 0
	for _, e := range got.Result.Rules {
		if strings.HasPrefix(e.Ref, RefPrefix) {
			if !slices.ContainsFunc(want, func(w Rule) bool { return Matches(w, e) }) {
				stale++
			}
			continue
		}
		others = append(others, e)
		r.say("  kept     %s rule %q, not this tool's", phase, e.Description)
	}
	next := slices.Clone(others)
	var missing []string
	for _, w := range want {
		if slices.ContainsFunc(others, func(e Rule) bool { return Matches(w, e) }) {
			continue
		}
		next = append(next, w)
		if !slices.ContainsFunc(got.Result.Rules, func(e Rule) bool { return Matches(w, e) }) {
			missing = append(missing, w.Description)
		}
	}
	if len(missing) == 0 && stale == 0 {
		r.say("  ok       %s: %d rule(s)", phase, len(want))
		return nil
	}
	fix := "add " + strings.Join(missing, "; ")
	if stale > 0 {
		fix = fmt.Sprintf("replace %d out-of-date rule(s); %s", stale, fix)
	}
	if r.differs(phase, fix) {
		// Send only what a rule is, not what Cloudflare added to it.
		send := make([]Rule, len(next))
		for i, n := range next {
			send[i] = Rule{Ref: n.Ref, Description: n.Description, Expression: n.Expression, Action: n.Action,
				ActionParameters: n.ActionParameters, Ratelimit: n.Ratelimit, Enabled: n.Enabled}
		}
		if err := r.api.Put(ctx, path, map[string]any{"rules": send}, nil); err != nil {
			return fmt.Errorf("zoneharden: writing %s: %w", phase, err)
		}
	}
	return nil
}

// Matches reports whether existing rule e does what want does: the same
// action and expression, ignoring enclosing parentheses and spacing, every
// action parameter and rate-limit field want sets, and not disabled.
func Matches(want, e Rule) bool {
	if e.Action != want.Action || normalize(e.Expression) != normalize(want.Expression) {
		return false
	}
	if e.Enabled != nil && !*e.Enabled {
		return false
	}
	return subset(want.ActionParameters, e.ActionParameters) && subset(want.Ratelimit, e.Ratelimit)
}

// normalize drops enclosing parentheses, which the dashboard adds, and
// collapses spacing.
func normalize(expr string) string {
	s := strings.Join(strings.Fields(expr), " ")
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && balanced(s[1:len(s)-1]) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func balanced(s string) bool {
	depth := 0
	for _, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// subset reports whether every field of want has the same JSON value in
// have.
func subset(want, have map[string]any) bool {
	for k, w := range want {
		h, ok := have[k]
		if !ok {
			return false
		}
		if wm, ok := w.(map[string]any); ok {
			hm, ok := h.(map[string]any)
			if !ok || !subset(wm, hm) {
				return false
			}
			continue
		}
		if !jsonEqual(w, h) {
			return false
		}
	}
	return true
}

func jsonEqual(a, b any) bool {
	var x, y any
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil || json.Unmarshal(ja, &x) != nil || json.Unmarshal(jb, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func isNotFound(err error) bool {
	var e *cf.Error
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}
