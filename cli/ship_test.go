package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grokify/oscompat/testutil"
)

// envScript writes the archive, and the environment it was given to the
// file $ENVLOG names.
const envScript = `#!/bin/sh
set -eu
out=""
while [ $# -gt 0 ]; do
  case "$1" in --out) out="$2"; shift 2 ;; *) shift ;; esac
done
mkdir -p "$out"
echo "$TOOL" > "$ENVLOG"
echo archive > "$out/example-app-0123abc.tgz"
`

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: the test's own git commands
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// TestShipDryRunPackagesWithTheReleaseEnvAndDeploysNothing checks that
// --dry-run stops after packaging, and that Options.ReleaseEnv reaches the
// release script.
func TestShipDryRunPackagesWithTheReleaseEnvAndDeploysNothing(t *testing.T) {
	testutil.RequirePOSIX(t, "the release script is a shell script run as a program")
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet", "--initial-branch=main")
	gitIn(t, repo, "config", "commit.gpgsign", "false")
	gitIn(t, repo, "config", "tag.gpgsign", "false")
	if err := os.MkdirAll(filepath.Join(repo, "ops", "host"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ops", "host", "release.sh"), []byte(envScript), 0o700); err != nil { //nolint:gosec // G306: the script must run
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "one")
	gitIn(t, repo, "tag", "v1.0.0")
	work := t.TempDir()
	config := filepath.Join(work, "config.yaml")
	if err := os.WriteFile(config, []byte("listen: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envLog := filepath.Join(work, "env.log")
	ledgerDir := filepath.Join(work, "deployments")

	cmd := New(Options{Fleet: testFleet, ReleaseEnv: func(self, fleet string) []string {
		return []string{"TOOL=" + self + " " + filepath.Base(fleet), "ENVLOG=" + envLog}
	}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"deploy", "--ledger-dir", ledgerDir, "ship", "example-app", "prod", "--tag", "v1.0.0",
		"--repo", repo, "--config", config, "--no-fetch", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ship --dry-run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "dry run: packaged example-app-0123abc.tgz") || !strings.Contains(out.String(), "nothing deployed") {
		t.Errorf("output:\n%s", out.String())
	}
	b, err := os.ReadFile(envLog) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatalf("the release script did not get ENVLOG: %v", err)
	}
	if got := strings.TrimSpace(string(b)); !strings.HasSuffix(got, " fleet.json") {
		t.Errorf("TOOL = %q", got)
	}
	if _, err := os.Stat(ledgerDir); !os.IsNotExist(err) {
		t.Errorf("a dry run wrote the ledger: %v", err)
	}
}

// TestSitesIsAnAliasOfFleet checks that --sites, which release scripts at
// older tags pass, names the fleet document as --fleet does.
func TestSitesIsAnAliasOfFleet(t *testing.T) {
	cmd := New(Options{Fleet: "/nonexistent/fleet.json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"validate", "--sites", testFleet})
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "OK") {
		t.Errorf("validate --sites: %v\n%s", err, out.String())
	}
	if strings.Contains(New(Options{}).UsageString(), "--sites") {
		t.Error("--sites is shown in the usage")
	}
}
