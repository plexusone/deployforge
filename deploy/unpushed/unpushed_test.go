package unpushed

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

		"github.com/plexusone/deployforge/internal/fleettest"
	"github.com/plexusone/deployforge/ledger"
	"github.com/plexusone/deployforge/provenance"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // G204: the test's own git commands
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// checkout makes <src>/<repo> with a bare origin, one pushed commit and one
// commit only on local main, and returns both commits.
func checkout(t *testing.T, src, repo string) (pushed, local string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	run(t, t.TempDir(), "git", "init", "--quiet", "--bare", origin)
	dir := filepath.Join(src, filepath.FromSlash(repo))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "init", "--quiet", "--initial-branch=main")
	run(t, dir, "git", "config", "commit.gpgsign", "false")
	run(t, dir, "git", "remote", "add", "origin", origin)
	run(t, dir, "git", "commit", "--quiet", "--allow-empty", "-m", "one")
	run(t, dir, "git", "push", "--quiet", "origin", "main")
	pushed = run(t, dir, "git", "rev-parse", "HEAD")
	run(t, dir, "git", "commit", "--quiet", "--allow-empty", "-m", "two")
	return pushed, run(t, dir, "git", "rev-parse", "HEAD")
}


const sum = "0000000000000000000000000000000000000000000000000000000000000000"

func TestLiveFindsTheCommitsOnNoRemoteBranch(t *testing.T) {
	src, dir, f := t.TempDir(), t.TempDir(), fleettest.Fleet()
	appPushed, appLocal := checkout(t, src, "github.com/example/one-cloud")
	_, libLocal := checkout(t, src, "github.com/example/lib")
	libPushed := run(t, filepath.Join(src, "github.com/example/lib"), "git", "rev-parse", "HEAD~1")
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

	// Prod runs a pushed app commit with a local upstream; staging was
	// deployed from a local app commit, then rolled back to a pushed one.
	rec := func(e ledger.Entry) {
		t.Helper()
		if err := ledger.Record(f, dir, e); err != nil {
			t.Fatal(err)
		}
	}
	rel := func(at time.Time, rev string) string { return at.Format("20060102T150405Z") + "-" + rev[:12] }
	prod := rel(t0, appPushed)
	rec(ledger.Entry{Time: t0, App: "one-cloud", Stack: "prod", Action: ledger.Deploy, Release: prod, Revision: appPushed[:12],
		SHA256: sum, Operator: "ops", Inputs: []provenance.Input{{Repo: "github.com/example/lib", Commit: libLocal}}})
	s1, s2 := rel(t0, appPushed), rel(t0.Add(time.Hour), appLocal)
	rec(ledger.Entry{Time: t0, App: "one-cloud", Stack: "staging", Action: ledger.Deploy, Release: s1, Revision: appPushed[:12],
		SHA256: sum, Operator: "ops", Inputs: []provenance.Input{{Repo: "github.com/example/lib", Commit: libPushed}}})
	rec(ledger.Entry{Time: t0.Add(time.Hour), App: "one-cloud", Stack: "staging", Action: ledger.Deploy, Release: s2, Revision: appLocal[:12],
		SHA256: sum, Previous: s1, Operator: "ops"})
	rec(ledger.Entry{Time: t0.Add(2 * time.Hour), App: "one-cloud", Stack: "staging", Action: ledger.Rollback, Release: s1, Revision: appPushed[:12],
		Previous: s2, Operator: "ops"})

	got, err := Live(f, Options{LedgerDir: dir, Src: src})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"prod github.com/example/one-cloud":    true,  // pushed
		"prod github.com/example/lib":          false, // only on local main
		"staging github.com/example/one-cloud": true,  // the rollback's release, pushed
		"staging github.com/example/lib":       true,  // that release's input, from its deploy
	}
	if len(got) != len(want) {
		t.Fatalf("got %d commits: %+v", len(got), got)
	}
	for _, c := range got {
		k := c.Stack + " " + c.Repo
		if c.Err != nil || c.Pushed != want[k] {
			t.Errorf("%s: pushed %v, err %v; want pushed %v", k, c.Pushed, c.Err, want[k])
		}
	}

	// Once local main is pushed, nothing is left.
	run(t, filepath.Join(src, "github.com/example/lib"), "git", "push", "--quiet", "origin", "main")
	got, err = Live(f, Options{LedgerDir: dir, Src: src, Fetch: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if !c.Pushed || c.Err != nil {
			t.Errorf("after the push: %+v", c)
		}
	}
}

func TestLiveSaysWhenACommitCannotBeChecked(t *testing.T) {
	src, dir, f := t.TempDir(), t.TempDir(), fleettest.Fleet()
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if err := ledger.Record(f, dir, ledger.Entry{Time: t0, App: "one-cloud", Stack: "prod", Action: ledger.Deploy,
		Release: "20261108T120000Z-abc1234def56", Revision: "abc1234def56", SHA256: sum, Operator: "ops"}); err != nil {
		t.Fatal(err)
	}
	got, err := Live(f, Options{LedgerDir: dir, Src: src})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Err == nil || got[0].Pushed {
		t.Fatalf("a missing checkout: %+v", got)
	}
}
