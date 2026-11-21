// Package caddy renders a host's Caddyfile from the fleet: one block per
// stack on the host, each forwarding its host names to its app's loopback
// port. An app behind Cloudflare is served with its zone's origin
// certificate, and Cloudflare's ranges are trusted to name the client; an
// app with no edge gets certificates from Caddy itself.
package caddy

import (
	"bytes"
	_ "embed"
	"fmt"
	"net/mail"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/plexusone/deployforge/fleet"
)

//go:embed Caddyfile.tmpl
var source string

var tmpl = template.Must(template.New("Caddyfile").Funcs(template.FuncMap{
	"join": func(items any, sep string) string {
		switch v := items.(type) {
		case []string:
			return strings.Join(v, sep)
		case []netip.Prefix:
			out := make([]string, len(v))
			for i, p := range v {
				out[i] = p.String()
			}
			return strings.Join(out, sep)
		}
		panic(fmt.Sprintf("caddy: join of %T", items))
	},
}).Parse(source))

// DefaultCertDir holds each zone's origin certificate on the host, as
// <domain>.pem and <domain>.key.
const DefaultCertDir = "/etc/caddy/certs"

// Options are the settings a Caddyfile needs beyond the fleet.
type Options struct {
	// Host is the fleet host the Caddyfile is for.
	Host string
	// CertDir holds the zones' origin certificates; DefaultCertDir when empty.
	CertDir string
	// Ranges are Cloudflare's proxy ranges, required when an app on the
	// host is behind Cloudflare.
	Ranges []netip.Prefix
	// With are stacks, as app/stack, to serve although they are not live
	// yet: a stack served on its host before its DNS points there.
	With []string
}

// Block is one stack's site block.
type Block struct {
	App, Stack string
	// Domain is the app's zone, which names its origin certificate.
	Domain string
	// Cert says the block serves the zone's origin certificate.
	Cert  bool
	Hosts []string
	Port  int
}

type data struct {
	Host, Email, CertDir string
	// DisableCerts is true when every block serves an origin certificate.
	DisableCerts bool
	Ranges       []netip.Prefix
	Blocks       []Block
}

var certDirPattern = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

// Blocks lists the live stacks on the host, and those named in with as
// app/stack, in fleet order with each app's primary stack first. Other
// stacks get no block, so Caddy serves no host name that is not meant to
// point at it. A name in with that the host does not run is an error.
func Blocks(f *fleet.Fleet, host string, with ...string) ([]Block, error) {
	var out []Block
	found := map[string]bool{}
	for _, a := range f.Apps {
		for _, stack := range f.StackNames(a) {
			s := a.Stacks[stack]
			if s.Host != host {
				continue
			}
			name := a.Name + "/" + stack
			if slices.Contains(with, name) {
				found[name] = true
			} else if s.Status != fleet.Live {
				continue
			}
			out = append(out, Block{App: a.Name, Stack: stack, Domain: a.Domain, Cert: a.Edge == fleet.Cloudflare, Hosts: s.Hosts, Port: s.Port})
		}
	}
	for _, w := range with {
		if !found[w] {
			return nil, fmt.Errorf("caddy: %s is not an app and stack on %s", w, host)
		}
	}
	return out, nil
}

// Render writes the Caddyfile for one host of a checked fleet.
func Render(f *fleet.Fleet, o Options) ([]byte, error) {
	if _, ok := f.Host(o.Host); !ok {
		return nil, fmt.Errorf("caddy: no host %q", o.Host)
	}
	certDir := o.CertDir
	if certDir == "" {
		certDir = DefaultCertDir
	}
	if !certDirPattern.MatchString(certDir) {
		return nil, fmt.Errorf("caddy: certificate directory %q must be an absolute path of plain names", certDir)
	}
	if f.ACMEEmail != "" {
		if a, err := mail.ParseAddress(f.ACMEEmail); err != nil || a.Address != f.ACMEEmail {
			return nil, fmt.Errorf("caddy: acmeEmail %q is not a plain address", f.ACMEEmail)
		}
	}
	blocks, err := Blocks(f, o.Host, o.With...)
	if err != nil {
		return nil, err
	}
	d := data{Host: o.Host, Email: f.ACMEEmail, CertDir: certDir, Blocks: blocks}
	d.DisableCerts = len(blocks) > 0 && !slices.ContainsFunc(blocks, func(b Block) bool { return !b.Cert })
	if slices.ContainsFunc(blocks, func(b Block) bool { return b.Cert }) {
		if len(o.Ranges) == 0 {
			return nil, fmt.Errorf("caddy: an app on %s is behind cloudflare, so Cloudflare's ranges are required", o.Host)
		}
		d.Ranges = o.Ranges
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, d); err != nil {
		return nil, fmt.Errorf("caddy: %w", err)
	}
	return b.Bytes(), nil
}
