// Package zones defines the DNS zones the fleet owns: the document format
// of zones.json, loading and checking it, verifying it against the zones'
// name servers, and its generated page. Every record of every zone is
// listed with what it serves, so a change to one zone's records is made
// knowing what else lives there. The Go types are the source of truth for
// zones.json's JSON Schema.
package zones

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/plexusone/deployforge/fleet"
)

// FormatVersion is the version of the zones.json format this package reads.
const FormatVersion = "1"

// Document is zones.json: every zone the fleet owns.
type Document struct {
	// Schema is the JSON Schema the file follows, for editors.
	Schema string `json:"$schema,omitempty" jsonschema:"description=The JSON Schema this file follows\\, for editors"`
	// FormatVersion is the version of the format the file follows.
	FormatVersion string `json:"formatVersion" jsonschema:"description=Version of the zones.json format the file follows,enum=1"`
	// Zones are the zones, in no particular order.
	Zones []Zone `json:"zones" jsonschema:"description=Every DNS zone the fleet owns"`
}

// DNSHost is who answers for a zone.
type DNSHost string

// Who answers.
const (
	// Cloudflare: the zone's name servers are Cloudflare's.
	Cloudflare DNSHost = "cloudflare"
	// Registrar: the registrar's own name servers still answer.
	Registrar DNSHost = "registrar"
)

// Zone is one domain and all of its records.
type Zone struct {
	// Name is the domain.
	Name string `json:"name" jsonschema:"description=The domain,pattern=^[a-z0-9-]+(\\.[a-z0-9-]+)+$"`
	// Registrar is where the domain is registered.
	Registrar string `json:"registrar" jsonschema:"description=Where the domain is registered"`
	// DNS is who answers for the zone.
	DNS DNSHost `json:"dns" jsonschema:"description=Who answers for the zone,enum=cloudflare,enum=registrar"`
	// Records are every record of the zone, one per name and type.
	Records []Record `json:"records" jsonschema:"description=Every record of the zone\\, one entry per name and type"`
	// Hardening is the zone's part of the security baseline that deployforge
	// zones harden applies.
	Hardening *Hardening `json:"hardening,omitempty" jsonschema:"description=The zone's settings for the security baseline deployforge zones harden applies"`
	// Notes are anything else a reader changing the zone must know.
	Notes string `json:"notes,omitempty" jsonschema:"description=What a reader changing the zone must know"`
}

// Hardening is what differs between zones in the security baseline; the
// rest is the same for every zone (package zoneharden).
type Hardening struct {
	// AllowCountries, when set, are the only countries allowed on
	// CountryHosts, as ISO 3166-1 alpha-2 codes.
	AllowCountries []string `json:"allowCountries,omitempty" jsonschema:"description=The only countries allowed on countryHosts\\, as ISO 3166-1 alpha-2 codes,items.pattern=^[A-Z]{2}$"`
	// CountryHosts are the labels under the zone AllowCountries applies to.
	CountryHosts []string `json:"countryHosts,omitempty" jsonschema:"description=Labels under the zone that allowCountries applies to\\, such as app and auth"`
	// BotFightModeOff turns Bot Fight Mode off, which challenges visitors
	// with a script.
	BotFightModeOff bool `json:"botFightModeOff,omitempty" jsonschema:"description=Turn Bot Fight Mode off\\, which challenges visitors with a script"`
	// WWWRedirect redirects www to the apex with a rule.
	WWWRedirect bool `json:"wwwRedirect,omitempty" jsonschema:"description=Redirect www to the apex\\, 301\\, keeping the path and query"`
	// RateLimit limits requests to one path per client address, such as
	// a sign-in endpoint.
	RateLimit *RateLimit `json:"rateLimit,omitempty" jsonschema:"description=A limit on requests to one path per client address\\, such as a sign-in endpoint"`
	// NoCachePrefixes are path prefixes never cached, such as /api/,
	// besides the documents: / and .html.
	NoCachePrefixes []string `json:"noCachePrefixes,omitempty" jsonschema:"description=Path prefixes never cached\\, such as /api/\\, besides / and .html documents,items.pattern=^/"`
}

// RateLimit is the zone's one rate-limited path. Cloudflare's Free plan
// allows one rule, counted per address and data center over 10 seconds.
type RateLimit struct {
	// Path is the exact request path limited, such as /login-token.
	Path string `json:"path" jsonschema:"description=The exact request path limited\\, such as /login-token,pattern=^/"`
	// Requests is how many requests one address may make in 10 seconds;
	// default 5.
	Requests int `json:"requests,omitempty" jsonschema:"description=Requests one address may make in 10 seconds; default 5,minimum=1"`
	// WhenRecord, when set, is the record label that must be present
	// before the limit applies, such as auth: a host that does not serve
	// the path yet needs no limit.
	WhenRecord string `json:"whenRecord,omitempty" jsonschema:"description=A record label that must be present before the limit applies\\, such as auth"`
}

// Limit is Requests, or its default.
func (r *RateLimit) Limit() int {
	if r.Requests > 0 {
		return r.Requests
	}
	return 5
}

// Purpose is what a record is for.
type Purpose string

// Purposes.
const (
	// StaticSite records serve a static website, such as a marketing site.
	StaticSite Purpose = "static-site"
	// App records serve an app stack of the fleet document.
	App Purpose = "app"
	// Redirect records exist only so a Cloudflare redirect rule answers.
	Redirect Purpose = "redirect"
	// Mail records route mail or say who may send it.
	Mail Purpose = "mail"
	// Verification records prove the domain to a service.
	Verification Purpose = "verification"
	// Parked records are a registrar's placeholder, to replace.
	Parked Purpose = "parked"
)

// Status is whether a record should exist now.
type Status string

// Statuses.
const (
	// Present records exist; verify checks them.
	Present Status = "present"
	// Planned records are to be added by a runbook step; verify skips them.
	Planned Status = "planned"
)

// Record is all the values of one name and type.
type Record struct {
	// Name is the label under the zone, or @ for the zone itself.
	Name string `json:"name" jsonschema:"description=The label under the zone\\, or @ for the zone itself,pattern=^(@|[a-z0-9_]([a-z0-9_-]*[a-z0-9])?(\\.[a-z0-9_]([a-z0-9_-]*[a-z0-9])?)*)$"`
	// Type is the record type. WORKER is a Cloudflare Workers custom
	// domain: Cloudflare keeps its record, which answers like a proxied
	// one, and its value is the Worker's name.
	Type string `json:"type" jsonschema:"description=The record type,enum=A,enum=AAAA,enum=CNAME,enum=MX,enum=TXT,enum=WORKER"`
	// Values are the record's values. For a proxied record they are the
	// origin, which only Cloudflare sees. An MX value is the priority and
	// the host, such as "10 mx.example.com"; a CNAME's is one host.
	Values []string `json:"values" jsonschema:"description=The values; for a proxied record\\, the origin\\, which only Cloudflare sees; an MX value is the priority and host"`
	// Proxied records are answered with Cloudflare's addresses.
	Proxied bool `json:"proxied,omitempty" jsonschema:"description=Whether Cloudflare proxies the record and answers with its own addresses"`
	// Purpose is what the record is for.
	Purpose Purpose `json:"purpose" jsonschema:"description=What the record is for,enum=static-site,enum=app,enum=redirect,enum=mail,enum=verification,enum=parked"`
	// Source is where what it serves comes from: a static site's
	// repository, or the fleet's app and stack as app/stack.
	Source string `json:"source,omitempty" jsonschema:"description=Where what it serves comes from: a static site's repository or the fleet's app/stack"`
	// Host is what serves it, such as github-pages, cloudflare-workers,
	// cloudflare-email-routing or the fleet host's name.
	Host string `json:"host,omitempty" jsonschema:"description=What serves it\\, such as github-pages or the fleet host's name"`
	// Status is whether it should exist now.
	Status Status `json:"status,omitempty" jsonschema:"description=Whether it should exist now; planned records are skipped by verify,enum=present,enum=planned"`
	// Check is an HTTPS request that must answer as given.
	Check *HTTPCheck `json:"check,omitempty" jsonschema:"description=An HTTPS request to the host that must answer as given"`
	// Notes are anything else to know.
	Notes string `json:"notes,omitempty" jsonschema:"description=Anything else to know about the record"`
}

// HTTPCheck is an HTTPS GET of the record's host, without following
// redirects.
type HTTPCheck struct {
	// Path is the path requested.
	Path string `json:"path" jsonschema:"description=The path requested,pattern=^/"`
	// Status is the status it must answer.
	Status int `json:"status" jsonschema:"description=The status it must answer"`
	// Location, when set, is what a redirect's Location must start with.
	Location string `json:"location,omitempty" jsonschema:"description=What a redirect's Location must start with"`
}

// State is a record's status, present when unset.
func (r *Record) State() Status {
	if r.Status == "" {
		return Present
	}
	return r.Status
}

// FQDN is the record's full name under zone.
func (r *Record) FQDN(zone string) string {
	if r.Name == "@" {
		return zone
	}
	return r.Name + "." + zone
}

// Load reads and checks zones.json.
func Load(path string) (*Document, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the zones document to read
	if err != nil {
		return nil, fmt.Errorf("zones: %w", err)
	}
	return Parse(data)
}

// Parse decodes and checks a zones document. Unknown fields are an error.
func Parse(data []byte) (*Document, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d Document
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("zones: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

var (
	zonePattern   = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	labelPattern  = regexp.MustCompile(`^(@|[a-z0-9_]([a-z0-9_-]*[a-z0-9])?(\.[a-z0-9_]([a-z0-9_-]*[a-z0-9])?)*)$`)
	hostPattern   = regexp.MustCompile(`^([a-z0-9_]([a-z0-9_-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?\.?$`)
	mxPattern     = regexp.MustCompile(`^\d{1,5} (\.|\S+)$`)
	workerPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// Validate checks the document's rules, returning every problem found.
func (d *Document) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf("zones: "+format, a...)) }
	if d.FormatVersion != FormatVersion {
		bad("formatVersion %q: this version of deployforge reads %q", d.FormatVersion, FormatVersion)
	}
	seenZone := map[string]bool{}
	for _, z := range d.Zones {
		if !zonePattern.MatchString(z.Name) {
			bad("zone %q: not a domain", z.Name)
		}
		if seenZone[z.Name] {
			bad("zone %s is listed twice", z.Name)
		}
		seenZone[z.Name] = true
		if z.DNS != Cloudflare && z.DNS != Registrar {
			bad("%s: dns %q is neither cloudflare nor registrar", z.Name, z.DNS)
		}
		seen := map[string]bool{}
		for _, r := range z.Records {
			at := fmt.Sprintf("%s: %s %s", z.Name, r.Name, r.Type)
			if seen[r.Name+" "+r.Type] {
				bad("%s is listed twice; give one entry all its values", at)
			}
			seen[r.Name+" "+r.Type] = true
			errs = append(errs, r.validate(at, z.DNS)...)
		}
		if h := z.Hardening; h != nil {
			for _, c := range h.AllowCountries {
				if !countryPattern.MatchString(c) {
					bad("%s: hardening: %q is not a two-letter country code", z.Name, c)
				}
			}
			if (len(h.AllowCountries) == 0) != (len(h.CountryHosts) == 0) {
				bad("%s: hardening: allowCountries and countryHosts go together", z.Name)
			}
			for _, l := range h.CountryHosts {
				if !seen[l+" A"] && !seen[l+" AAAA"] && !seen[l+" CNAME"] && !seen[l+" WORKER"] {
					bad("%s: hardening: country host %q is not a record of the zone", z.Name, l)
				}
			}
			if rl := h.RateLimit; rl != nil {
				if !strings.HasPrefix(rl.Path, "/") || strings.ContainsAny(rl.Path, "\" \t\r\n") {
					bad("%s: hardening: rateLimit.path %q must be a path starting with /", z.Name, rl.Path)
				}
				if rl.Requests < 0 {
					bad("%s: hardening: rateLimit.requests must be positive", z.Name)
				}
			}
			for _, prefix := range h.NoCachePrefixes {
				if !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "\" \t\r\n") {
					bad("%s: hardening: noCachePrefixes %q must be a path starting with /", z.Name, prefix)
				}
			}
		}
	}
	return errors.Join(errs...)
}

var countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)

func (r *Record) validate(at string, dns DNSHost) []error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("zones: %s: "+format, append([]any{at}, a...)...))
	}
	if !labelPattern.MatchString(r.Name) {
		bad("name is not @ or a label")
	}
	if len(r.Values) == 0 {
		bad("no values")
	}
	for _, v := range r.Values {
		switch r.Type {
		case "A":
			if a, err := netip.ParseAddr(v); err != nil || !a.Is4() {
				bad("%q is not an IPv4 address", v)
			}
		case "AAAA":
			if a, err := netip.ParseAddr(v); err != nil || !a.Is6() {
				bad("%q is not an IPv6 address", v)
			}
		case "CNAME":
			if !hostPattern.MatchString(v) {
				bad("%q is not a host name", v)
			}
		case "MX":
			if !mxPattern.MatchString(v) {
				bad("%q is not a priority and a host", v)
			}
		case "TXT":
		case "WORKER":
			if !workerPattern.MatchString(v) {
				bad("%q is not a Worker's name", v)
			}
		default:
			bad("type %q is not A, AAAA, CNAME, MX, TXT or WORKER", r.Type)
		}
	}
	if (r.Type == "CNAME" || r.Type == "WORKER") && len(r.Values) > 1 {
		bad("a %s has one value", r.Type)
	}
	if r.Type == "WORKER" && (!r.Proxied || dns != Cloudflare) {
		bad("a Workers custom domain is proxied, on a Cloudflare zone")
	}
	if r.Proxied {
		if dns != Cloudflare {
			bad("proxied, but the zone is not on Cloudflare")
		}
		if !slices.Contains([]string{"A", "AAAA", "CNAME", "WORKER"}, r.Type) {
			bad("only A, AAAA, CNAME and WORKER records can be proxied")
		}
	}
	switch r.Purpose {
	case StaticSite, App, Redirect, Mail, Verification, Parked:
	default:
		bad("purpose %q is not one of static-site, app, redirect, mail, verification, parked", r.Purpose)
	}
	if r.Purpose == StaticSite && r.Source == "" {
		bad("a static site names its source repository")
	}
	if r.Purpose == App && strings.Count(r.Source, "/") != 1 {
		bad("an app's source is its fleet app/stack, not %q", r.Source)
	}
	if s := r.State(); s != Present && s != Planned {
		bad("status %q is neither present nor planned", s)
	}
	if c := r.Check; c != nil {
		if !strings.HasPrefix(c.Path, "/") {
			bad("check path %q does not start with /", c.Path)
		}
		if c.Status < 100 || c.Status > 599 {
			bad("check status %d is not an HTTP status", c.Status)
		}
	}
	return errs
}

// CheckApps checks the document against the fleet: every app record names
// an app and stack that exist, and every host name of a stack that is not
// planned has a record, so no live host name is missing from the zones.
func (d *Document) CheckApps(f *fleet.Fleet) error {
	var errs []error
	have := map[string]bool{}
	for _, z := range d.Zones {
		for _, r := range z.Records {
			have[r.FQDN(z.Name)] = true
			if r.Purpose != App {
				continue
			}
			app, stack, _ := strings.Cut(r.Source, "/")
			if _, _, err := f.Stack(app, stack); err != nil {
				errs = append(errs, fmt.Errorf("zones: %s %s: source %s is not an app and stack of the fleet", r.FQDN(z.Name), r.Type, r.Source))
			}
		}
	}
	for _, a := range f.Apps {
		for _, stack := range f.StackNames(a) {
			s := a.Stacks[stack]
			if s.Status == fleet.Planned {
				continue
			}
			for _, h := range s.Hosts {
				if !have[h] {
					errs = append(errs, fmt.Errorf("zones: %s serves %s/%s, which is %s, and has no record in the zones document", h, a.Name, stack, s.Status))
				}
			}
		}
	}
	return errors.Join(errs...)
}
