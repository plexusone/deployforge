package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/deploy/systemd"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/internal/fleettest"
)

func testFleet() *fleet.Fleet {
	f := fleettest.Fleet()
	a := &f.Apps[0]
	a.LogFiles = []string{"security.jsonl"}
	prod := a.Stacks["prod"]
	prod.LoopbackHosts = []string{"auth.one.example.com"}
	a.Stacks["prod"] = prod
	staging := a.Stacks["staging"]
	staging.Limits.MemoryMax, staging.Limits.CPUQuota = "256M", "25%"
	a.Stacks["staging"] = staging
	return f
}

func TestTheScriptIsValidBashAndNamesTheStack(t *testing.T) {
	out, err := Render(testFleet(), "one-cloud", "staging")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(string(out))
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, msg)
	}
	got := string(out)
	for _, want := range []string{
		"unit=one-cloud-staging", "user=onecloudstaging", "set -eu",
		"useradd --system --home-dir", "systemctl enable", "MemoryMax=256M",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(got, "systemctl start") || strings.Contains(got, "systemctl restart \"$unit\"\n") {
		t.Error("the script starts a service that has no release yet")
	}
}

// stubs writes fake system commands that log what they were asked to do.
func stubs(t *testing.T, log string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"id", "useradd", "install", "systemctl"} {
		body := "#!/bin/bash\necho \"" + name + " $*\" >> '" + log + "'\n"
		switch name {
		case "id":
			// Report the user as missing on the first run and present afterwards.
			body = "#!/bin/bash\necho \"id $*\" >> '" + log + "'\n[ -f '" + log + ".user' ]\n"
		case "useradd":
			body += "touch '" + log + ".user'\n"
		case "install":
			// Real directories, so the script can write into them.
			body = "#!/bin/bash\necho \"install $*\" >> '" + log + "'\nfor a in \"$@\"; do case \"$a\" in /*) mkdir -p \"$a\" ;; esac; done\n"
		case "systemctl":
			body = "#!/bin/bash\necho \"systemctl $*\" >> '" + log + "'\n[ \"$1\" != is-active ] || exit 1\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { //nolint:gosec // G306: a test stub that must be executable
			t.Fatal(err)
		}
	}
	return dir
}

func TestTheScriptIsIdempotentAndWritesTheUnit(t *testing.T) {
	f := testFleet()
	script, err := Render(f, "one-cloud", "prod")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := systemd.Render(f, "one-cloud", "prod")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	bin := stubs(t, log)
	// install -d is stubbed with absolute paths, so give it the root-prefixed ones.
	for _, dir := range []string{filepath.Join(root, "etc", "systemd", "system"), filepath.Join(root, "etc", "logrotate.d")} {
		if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // G301: a scratch directory
			t.Fatal(err)
		}
	}
	hosts := filepath.Join(root, "etc", "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		cmd := exec.Command("bash", "-s")
		cmd.Stdin = strings.NewReader(string(script))
		cmd.Env = append(os.Environ(), "DEPLOYFORGE_ROOT="+root, "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
		return string(out)
	}

	first := run()
	if !strings.Contains(first, "one-cloud is ready for its first deploy") {
		t.Errorf("first run said %q", first)
	}
	written, err := os.ReadFile(filepath.Join(root, "etc", "systemd", "system", "one-cloud.service")) //nolint:gosec // G304: a path under the test's own temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(unit) {
		t.Errorf("the installed unit differs from the rendered one:\n%s", written)
	}
	rotate, err := os.ReadFile(filepath.Join(root, "etc", "logrotate.d", "one-cloud")) //nolint:gosec // G304: a path under the test's own temporary directory
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/var/log/one-cloud/security.jsonl {", "su onecloud onecloud", "create 0600 onecloud onecloud", "rotate 7", "delaycompress"} {
		if !strings.Contains(string(rotate), want) {
			t.Errorf("the logrotate rule lacks %q:\n%s", want, rotate)
		}
	}
	if strings.Contains(string(rotate), "copytruncate") {
		t.Errorf("the logrotate rule truncates in place:\n%s", rotate)
	}

	second := run()
	etcHosts, err := os.ReadFile(hosts) //nolint:gosec // G304: a path under the test's own temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(etcHosts), "127.0.0.1 auth.one.example.com\n"); n != 1 || strings.Contains(string(etcHosts), "one.example.com\n127") {
		t.Errorf("/etc/hosts after two runs:\n%s", etcHosts)
	}
	if !strings.HasPrefix(string(etcHosts), "127.0.0.1 localhost\n") {
		t.Errorf("the existing lines changed:\n%s", etcHosts)
	}
	if strings.Contains(second, "changed while running") {
		t.Errorf("an unchanged unit asked for a restart: %q", second)
	}
	calls, err := os.ReadFile(log) //nolint:gosec // G304: a path under the test's own temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(calls), "useradd"); n != 1 {
		t.Errorf("useradd ran %d times over two runs, want once:\n%s", n, calls)
	}
	if strings.Contains(string(calls), "systemctl start") {
		t.Errorf("the script started the service:\n%s", calls)
	}
	// No scratch files are left behind in the unit directory.
	for _, dir := range []string{"systemd/system", "logrotate.d"} {
		entries, err := os.ReadDir(filepath.Join(root, "etc", dir))
		if err != nil || len(entries) != 1 {
			t.Errorf("etc/%s holds %d files: %v", dir, len(entries), entries)
		}
	}
}

func TestRenderRefusesWhatSystemdRefuses(t *testing.T) {
	f := testFleet()
	prod := f.Apps[0].Stacks["prod"]
	prod.Limits = nil
	f.Apps[0].Stacks["prod"] = prod
	if _, err := Render(f, "one-cloud", "prod"); err == nil || !strings.Contains(err.Error(), "no limits") {
		t.Errorf("err = %v", err)
	}
	if _, err := Render(testFleet(), "other", "prod"); err == nil {
		t.Error("an unknown app was accepted")
	}
}

func TestAnAppWithoutLogFilesGetsNoRotationAndNoLoopbackLines(t *testing.T) {
	out, err := Render(fleettest.Fleet(), "one-cloud", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "logrotate") || strings.Contains(string(out), "/etc/hosts") {
		t.Errorf("unasked rotation or loopback lines:\n%s", out)
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(string(out))
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, msg)
	}
}
