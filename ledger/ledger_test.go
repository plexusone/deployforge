package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/internal/fleettest"
	"github.com/plexusone/deployforge/provenance"
)

func testFleet() *fleet.Fleet { return fleettest.Fleet() }

const (
	rel1 = "20261023T180000Z-abc1234"
	rel2 = "20261024T090000Z-def5678"
	rel3 = "20261025T100000Z-0123abc"
	sum  = "0000000000000000000000000000000000000000000000000000000000000000"
)

var t0 = time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)

func deploy(release, previous, rev string, at time.Time) Entry {
	return Entry{Time: at, App: "one-cloud", Stack: "staging", Action: Deploy, Release: release,
		Revision: rev, Tag: "v1.0.0", SHA256: sum, Previous: previous, Operator: "ops"}
}

func TestADeployHistoryIsRecordedAndReadBack(t *testing.T) {
	dir, f := t.TempDir(), testFleet()
	if err := Record(f, dir, deploy(rel1, "", "abc1234", t0)); err != nil {
		t.Fatal(err)
	}
	if err := Record(f, dir, deploy(rel2, rel1, "def5678", t0.Add(24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	path := Path(dir, f, f.Apps[0], "staging")
	if filepath.Base(path) != "one-cloud-staging.jsonl" {
		t.Fatalf("path = %s", path)
	}
	got, err := Read(path)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d entries, err = %v", len(got), err)
	}
	cur, ok := Current(got)
	if !ok || cur.Release != rel2 || cur.Previous != rel1 || cur.Operator != "ops" {
		t.Fatalf("current = %+v", cur)
	}
	// One line per entry, appended: the file is readable history.
	raw, _ := readFile(path)
	if n := strings.Count(string(raw), "\n"); n != 2 {
		t.Fatalf("%d lines", n)
	}
	// Prod has its own ledger.
	if _, ok := Current(mustRead(t, Path(dir, f, f.Apps[0], "prod"))); ok {
		t.Fatal("prod shares staging's ledger")
	}
}

// readFile reads a file the test itself wrote under a temporary directory.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: a path under the test's own temporary directory
}

func mustRead(t *testing.T, path string) []Entry {
	t.Helper()
	e, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestARollbackReturnsToAnEarlierDeploy(t *testing.T) {
	dir, f := t.TempDir(), testFleet()
	for _, e := range []Entry{deploy(rel1, "", "abc1234", t0), deploy(rel2, rel1, "def5678", t0.Add(time.Hour))} {
		if err := Record(f, dir, e); err != nil {
			t.Fatal(err)
		}
	}
	rb := Entry{Time: t0.Add(2 * time.Hour), App: "one-cloud", Stack: "staging", Action: Rollback, Release: rel1,
		Revision: "abc1234", Previous: rel2, Operator: "ops"}
	if err := Record(f, dir, rb); err != nil {
		t.Fatal(err)
	}
	cur, _ := Current(mustRead(t, Path(dir, f, f.Apps[0], "staging")))
	if cur.Action != Rollback || cur.Release != rel1 {
		t.Fatalf("current = %+v", cur)
	}
}

func TestTheLedgerRefusesWhatCannotBeTrue(t *testing.T) {
	f := testFleet()
	seed := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, e := range []Entry{deploy(rel1, "", "abc1234", t0), deploy(rel2, rel1, "def5678", t0.Add(time.Hour))} {
			if err := Record(f, dir, e); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	rollback := func(release, previous string, at time.Time) Entry {
		return Entry{Time: at, App: "one-cloud", Stack: "staging", Action: Rollback, Release: release,
			Revision: "abc1234", Previous: previous, Operator: "ops"}
	}
	for name, tc := range map[string]struct {
		entry Entry
		want  string
	}{
		"a deploy that skips the live release":      {deploy(rel3, rel1, "0123abc", t0.Add(2*time.Hour)), "release now live"},
		"a deploy with no previous after the first": {deploy(rel3, "", "0123abc", t0.Add(2*time.Hour)), "release now live"},
		"redeploying the live release":              {deploy(rel2, rel2, "def5678", t0.Add(2*time.Hour)), "already live"},
		"an entry dated before the last":            {deploy(rel3, rel2, "0123abc", t0), "before the last"},
		"a rollback to a release never deployed":    {rollback(rel3, rel2, t0.Add(2*time.Hour)), "never deployed"},
		"a rollback that misnames the live release": {rollback(rel1, rel3, t0.Add(2*time.Hour)), "release now live"},
		"a rollback to the release already live":    {rollback(rel2, rel2, t0.Add(2*time.Hour)), "already live"},
	} {
		dir := seed(t)
		path := Path(dir, f, f.Apps[0], "staging")
		before, _ := readFile(path)
		err := Record(f, dir, tc.entry)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tc.want)
		}
		if after, _ := readFile(path); string(after) != string(before) {
			t.Errorf("%s: a refused entry changed the ledger", name)
		}
	}
	t.Run("a first rollback or a first deploy with a previous", func(t *testing.T) {
		dir := t.TempDir()
		if err := Record(f, dir, rollback(rel1, rel2, t0)); err == nil || !strings.Contains(err.Error(), "nothing has been deployed") {
			t.Errorf("first rollback: %v", err)
		}
		if err := Record(f, dir, deploy(rel1, rel2, "abc1234", t0)); err == nil || !strings.Contains(err.Error(), "nothing has been deployed") {
			t.Errorf("first deploy with a previous: %v", err)
		}
	})
}

func TestAMalformedEntryReportsEveryProblem(t *testing.T) {
	f := testFleet()
	err := Entry{Time: time.Now(), App: "nope", Stack: "qa", Action: "ship", Release: "latest",
		Revision: "HEAD", Tag: "../x", Previous: "old", Operator: " "}.Validate(f)
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"in UTC", `no app "nope"`, `action "ship"`, `release "latest"`, `revision "HEAD"`, `tag "../x"`, `previous "old"`, "operator"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	e := deploy(rel1, "", "abc1234", t0)
	e.SHA256 = "abc"
	if err := e.Validate(f); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("a deploy with a short checksum: %v", err)
	}
	e = Entry{Time: t0, App: "one-cloud", Stack: "prod", Action: Rollback, Release: rel1, Revision: "abc1234", Operator: "ops"}
	if err := e.Validate(f); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Errorf("a rollback with no previous: %v", err)
	}
}

func TestReadNamesTheBrokenLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.jsonl")
	good := `{"time":"2026-10-23T18:00:00Z","app":"one-cloud","stack":"staging","action":"deploy","release":"` + rel1 + `","revision":"abc1234","sha256":"` + sum + `","operator":"ops"}`
	if err := os.WriteFile(path, []byte(good+"\n\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("err = %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(good, `"operator"`, `"extra":1,"operator"`, 1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Error("an unknown field was accepted")
	}
	if e, err := Read(filepath.Join(t.TempDir(), "missing.jsonl")); err != nil || e != nil {
		t.Errorf("a missing ledger = %v, %v", e, err)
	}
}

func TestADeployRecordsItsInputsAndOnlyADeployHasThem(t *testing.T) {
	dir, f := t.TempDir(), testFleet()
	e := deploy(rel1, "", "abc1234", t0)
	e.Inputs = []provenance.Input{{Repo: "github.com/plexusone/systemforge", Commit: "0123456789abcdef0123456789abcdef01234567"}}
	if err := Record(f, dir, e); err != nil {
		t.Fatal(err)
	}
	got, err := Read(Path(dir, f, f.Apps[0], "staging"))
	if err != nil || len(got) != 1 || len(got[0].Inputs) != 1 || got[0].Inputs[0] != e.Inputs[0] {
		t.Fatalf("read back %+v, %v", got, err)
	}

	bad := map[string]Entry{}
	twice := deploy(rel2, rel1, "def5678", t0.Add(time.Hour))
	twice.Inputs = []provenance.Input{e.Inputs[0], e.Inputs[0]}
	bad["a repository twice"] = twice
	short := deploy(rel2, rel1, "def5678", t0.Add(time.Hour))
	short.Inputs = []provenance.Input{{Repo: "github.com/plexusone/systemforge", Commit: "abc1234"}}
	bad["a short commit"] = short
	rb := Entry{Time: t0.Add(time.Hour), App: "one-cloud", Stack: "staging", Action: Rollback, Release: rel1,
		Revision: "abc1234", Previous: rel2, Operator: "ops", Inputs: e.Inputs}
	bad["a rollback"] = rb
	for name, b := range bad {
		if err := b.Validate(f); err == nil || !strings.Contains(err.Error(), "input") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
