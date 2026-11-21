package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/dbroles"
	"github.com/plexusone/deployforge/deploy/deployer"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/origincert"
)

// fakeTransport records what a command does on a host.
type fakeTransport struct {
	calls  []string
	script string
	// csr is what openssl req prints on the host.
	csr string
}

func (f *fakeTransport) TempDir(context.Context) (string, error) { return "/tmp/tmp.abc", nil }
func (f *fakeTransport) Upload(_ context.Context, local, remote string) error {
	data, err := os.ReadFile(local) //nolint:gosec // G304: the file the command under test wrote
	if err != nil {
		return err
	}
	f.script = string(data)
	f.calls = append(f.calls, "upload "+remote)
	return nil
}
func (f *fakeTransport) Sudo(_ context.Context, argv []string, _ io.Reader, stdout, _ io.Writer) error {
	f.calls = append(f.calls, "sudo "+strings.Join(argv, " "))
	switch argv[0] {
	case "bash":
		_, err := io.WriteString(stdout, "example-app is ready for its first deploy\n")
		return err
	case "openssl":
		_, err := io.WriteString(stdout, f.csr)
		return err
	}
	return nil
}

func TestHostSetupPrintsOrApplies(t *testing.T) {
	out, err := run(t, "host", "setup", "example-app", "prod")
	if err != nil || !strings.HasPrefix(out, "#!/bin/bash") || !strings.Contains(out, "unit=example-app") {
		t.Fatalf("print: %v\n%s", err, out)
	}

	tr := &fakeTransport{}
	var dialled fleet.Host
	cmd := New(Options{Fleet: testFleet, Dial: func(string) deployer.Dialer {
		return func(h fleet.Host) (deployer.Transport, error) { dialled = h; return tr, nil }
	}})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"host", "setup", "example-app", "prod", "--apply"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("apply: %v\n%s", err, buf.String())
	}
	if dialled.Name != "web-1" || tr.script != out {
		t.Errorf("dialled %+v; uploaded the printed script: %v", dialled, tr.script == out)
	}
	want := "upload /tmp/tmp.abc/host-setup.sh|sudo bash /tmp/tmp.abc/host-setup.sh|sudo rm -rf /tmp/tmp.abc"
	if got := strings.Join(tr.calls, "|"); got != want {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(buf.String(), "ready for its first deploy") {
		t.Errorf("output = %q", buf.String())
	}
}

func TestCaddyInstallValidatesFirstAndKeepsThePrevious(t *testing.T) {
	tr := &fakeTransport{}
	cmd := New(Options{Fleet: testFleet, Dial: func(string) deployer.Dialer {
		return func(fleet.Host) (deployer.Transport, error) { return tr, nil }
	}})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"caddy", "install", "web-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	want := []string{
		"upload /tmp/tmp.abc/Caddyfile",
		"sudo caddy validate --config /tmp/tmp.abc/Caddyfile --adapter caddyfile",
		"sudo test -f /etc/caddy/Caddyfile",
		"sudo cp -p /etc/caddy/Caddyfile /etc/caddy/Caddyfile.previous",
		"sudo install -o root -g root -m 0644 /tmp/tmp.abc/Caddyfile /etc/caddy/Caddyfile",
		"sudo systemctl reload caddy",
		"sudo rm -rf /tmp/tmp.abc",
	}
	if got := strings.Join(tr.calls, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", got)
	}
	if !strings.Contains(tr.script, "app.example.com, auth.example.com {") {
		t.Errorf("uploaded Caddyfile:\n%s", tr.script)
	}
}

func TestCaddyfilePrintsAHostsLiveStacks(t *testing.T) {
	out, err := run(t, "caddyfile", "web-1")
	if err != nil || !strings.Contains(out, "import app 8101") || strings.Contains(out, "stage-app") {
		t.Errorf("%v\n%s", err, out)
	}
	if out, err := run(t, "caddyfile", "web-1", "--with", "example-app/staging"); err != nil || !strings.Contains(out, "stage-app.example.com {") {
		t.Errorf("with: %v\n%s", err, out)
	}
}

// memParams is a parameter store in memory.
type memParams map[string]string

func (m memParams) Get(_ context.Context, name string) (string, bool, error) {
	v, ok := m[name]
	return v, ok, nil
}

func (m memParams) Put(_ context.Context, name, value string) error {
	m[name] = value
	return nil
}

func TestDBRolesPlanShowsNoPasswordAndApplyNeedsConfirm(t *testing.T) {
	dir := t.TempDir()
	doc := `{
  "formatVersion": "1",
  "portRange": {"min": 8101, "max": 8199},
  "hosts": [{"name": "web-1", "address": "203.0.113.10"}],
  "databaseServer": {"endpoint": "db.internal", "adminUser": "admin", "adminDatabase": "fleet",
    "adminPassword": {"parameter": "/fleet/db-admin"}, "via": "web-1"},
  "apps": [{
    "name": "example-app", "repo": "github.com/example/example-app", "secretPrefix": "/example/{stack}/example-app/",
    "stacks": {"prod": {"host": "web-1", "hosts": ["app.example.com"], "port": 8101, "status": "live",
      "limits": {"memoryMax": "512M", "cpuQuota": "50%", "connectionLimit": 10},
      "databases": [{"name": "example", "appRole": true, "ownerDsn": "migrate/EXAMPLE_PG_ADMIN_DSN", "appDsn": "EXAMPLE_PG_DSN"}]}}
  }]
}`
	path := dir + "/fleet.json"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	params := memParams{"/fleet/db-admin": "adm1n"}
	cmd := New(Options{Fleet: path, Params: func(*cobra.Command) (dbroles.Store, error) { return params, nil }})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"db", "roles", "plan", "example-app", "prod"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"create /example/prod/example-app/migrate/EXAMPLE_PG_ADMIN_DSN", "create /example/prod/example-app/EXAMPLE_PG_DSN", "example_owner", "example_app"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "adm1n") || strings.Contains(out, "SCRAM-SHA-256$4096:") && !strings.Contains(out, "redacted") {
		t.Errorf("the plan shows a secret:\n%s", out)
	}
	if len(params) != 1 {
		t.Errorf("plan wrote parameters: %v", params)
	}
	buf.Reset()
	cmd.SetArgs([]string{"db", "roles", "apply", "example-app", "prod"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("apply without --confirm: %v", err)
	}
	if len(params) != 1 {
		t.Errorf("apply without --confirm wrote parameters: %v", params)
	}
}

// fakeOrigin signs nothing; it returns a certificate for the request.
type fakeOrigin struct{ got origincert.Request }

func (f *fakeOrigin) Create(_ context.Context, r origincert.Request) (*origincert.Certificate, error) {
	f.got = r
	return &origincert.Certificate{ID: "42", Certificate: "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n", Hostnames: r.Hostnames}, nil
}
func (f *fakeOrigin) List(context.Context, string) ([]origincert.Certificate, error) { return nil, nil }
func (f *fakeOrigin) Revoke(context.Context, string) error                           { return nil }

func TestOriginCertInstallMakesTheKeyOnTheHost(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	tr := &fakeTransport{csr: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))}
	origin := &fakeOrigin{}
	cmd := New(Options{Fleet: testFleet,
		Dial:     func(string) deployer.Dialer { return func(fleet.Host) (deployer.Transport, error) { return tr, nil } },
		OriginCA: func() (origincert.API, error) { return origin, nil },
	})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"origin-cert", "install", "example.com", "web-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	want := []string{
		"sudo install -d -o root -g caddy -m 0750 /etc/caddy/certs",
		"sudo openssl req -new -newkey rsa:2048 -noenc -batch -keyout /etc/caddy/certs/example.com.key.new",
		"upload /tmp/tmp.abc/example.com.pem",
		"sudo install -o root -g caddy -m 0640 /tmp/tmp.abc/example.com.pem /etc/caddy/certs/example.com.pem",
		"sudo install -o root -g caddy -m 0640 /etc/caddy/certs/example.com.key.new /etc/caddy/certs/example.com.key",
		"sudo rm -f /etc/caddy/certs/example.com.key.new",
		"sudo rm -rf /tmp/tmp.abc",
	}
	if got := strings.Join(tr.calls, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", got)
	}
	if origin.got.KeyType != "ecc" || origin.got.Hostnames[1] != "*.example.com" {
		t.Errorf("request %+v", origin.got)
	}
	if !strings.Contains(tr.script, "BEGIN CERTIFICATE") {
		t.Errorf("uploaded %q", tr.script)
	}
	if _, err := run(t, "origin-cert", "install", "Example.com;", "web-1"); err == nil {
		t.Error("a zone that is not a zone name")
	}
}
