package caddy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/deploy/cfranges"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/internal/fleettest"
)

func behindCloudflare() *fleet.Fleet {
	f := fleettest.Fleet()
	f.Apps[0].Edge = fleet.Cloudflare
	staging := f.Apps[0].Stacks["staging"]
	staging.Status = fleet.Planned
	f.Apps[0].Stacks["staging"] = staging
	return f
}

func TestAnAppBehindCloudflareGetsItsOriginCertificateAndTheTrustedRanges(t *testing.T) {
	ranges, err := cfranges.Ranges()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(behindCloudflare(), Options{Host: "web-1", Ranges: ranges})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"trusted_proxies static 173.245.48.0/20 ",
		"client_ip_headers CF-Connecting-IP X-Forwarded-For",
		"# one-cloud prod\none.example.com, auth.one.example.com {",
		"tls /etc/caddy/certs/one.example.com.pem /etc/caddy/certs/one.example.com.key",
		"import app 8101",
		"header_up X-Forwarded-For {client_ip}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, got)
		}
	}
	// A planned stack is not served.
	if strings.Contains(got, "stage-app") {
		t.Errorf("a planned stack is served:\n%s", got)
	}
	if _, err := Render(behindCloudflare(), Options{Host: "web-1"}); err == nil || !strings.Contains(err.Error(), "ranges are required") {
		t.Errorf("no ranges: %v", err)
	}
}

func TestAnAppWithNoEdgeGetsCertificatesFromCaddy(t *testing.T) {
	f := fleettest.Fleet()
	f.ACMEEmail = "ops@example.com"
	out, err := Render(f, Options{Host: "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "tls ") || strings.Contains(got, "trusted_proxies") {
		t.Errorf("an app with no edge has origin certificates or trusted proxies:\n%s", got)
	}
	if !strings.Contains(got, "email ops@example.com") || !strings.Contains(got, "stage-app.one.example.com {") {
		t.Errorf("Caddyfile:\n%s", got)
	}
	f.ACMEEmail = "Ops <ops@example.com>"
	if _, err := Render(f, Options{Host: "web-1"}); err == nil {
		t.Error("an address with a display name")
	}
}

func TestWithServesAStackBeforeItIsLiveAndOnlyOnItsHost(t *testing.T) {
	f := behindCloudflare()
	ranges, err := cfranges.Ranges()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(f, Options{Host: "web-1", Ranges: ranges, With: []string{"one-cloud/staging"}})
	if err != nil || !strings.Contains(string(out), "stage-app.one.example.com {") {
		t.Errorf("with: %v\n%s", err, out)
	}
	if _, err := Render(f, Options{Host: "web-1", Ranges: ranges, With: []string{"one-cloud/qa"}}); err == nil {
		t.Error("with an unknown stack")
	}
	f.Hosts = append(f.Hosts, fleet.Host{Name: "web-2", Address: "203.0.113.11"})
	out, err = Render(f, Options{Host: "web-2", Ranges: ranges})
	if err != nil || strings.Contains(string(out), "one.example.com") {
		t.Errorf("another host serves web-1's stacks: %v\n%s", err, out)
	}
	if _, err := Render(f, Options{Host: "nowhere"}); err == nil {
		t.Error("an unknown host")
	}
}

// TestCaddyAdaptsTheCaddyfile has Caddy itself parse both kinds of
// Caddyfile, when Caddy is installed.
func TestCaddyAdaptsTheCaddyfile(t *testing.T) {
	bin, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("caddy is not installed")
	}
	ranges, err := cfranges.Ranges()
	if err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]*fleet.Fleet{"cloudflare": behindCloudflare(), "no edge": fleettest.Fleet()} {
		out, err := Render(f, Options{Host: "web-1", Ranges: ranges})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "Caddyfile")
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatal(err)
		}
		if msg, err := exec.Command(bin, "adapt", "--config", path, "--adapter", "caddyfile").CombinedOutput(); err != nil { //nolint:gosec // G204: caddy on a file the test wrote
			t.Errorf("%s: caddy adapt: %v\n%s", name, err, msg)
		}
	}
}
