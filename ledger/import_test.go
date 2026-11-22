package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacy writes entries as another tool's ledger, with the app under
// field.
func legacy(t *testing.T, path, field string, entries ...Entry) {
	t.Helper()
	var lines []string
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if field != "app" {
			m[field] = m["app"]
			delete(m, "app")
		}
		if b, err = json.Marshal(m); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestImport(t *testing.T) {
	f := testFleet()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "old.jsonl")
	legacy(t, src, "site", deploy(rel1, "", "abc1234", t0), deploy(rel2, rel1, "def5678", t0.Add(time.Hour)))
	n, err := Import(f, dir, "one-cloud", "staging", src, "site")
	if err != nil || n != 2 {
		t.Fatalf("import: %d, %v", n, err)
	}
	a, _ := f.App("one-cloud")
	got := mustRead(t, Path(dir, f, a, "staging"))
	if cur, _ := Current(got); cur.Release != rel2 || cur.App != "one-cloud" {
		t.Errorf("current after import: %+v", cur)
	}
	if _, err := Import(f, dir, "one-cloud", "staging", src, "site"); err == nil || !strings.Contains(err.Error(), "already has 2 entries") {
		t.Errorf("into a ledger with entries: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("left behind in %s: %v, %v", dir, entries, err)
	}
}

func TestImportInPlace(t *testing.T) {
	f := testFleet()
	dir := t.TempDir()
	a, _ := f.App("one-cloud")
	path := Path(dir, f, a, "staging")
	legacy(t, path, "site", deploy(rel1, "", "abc1234", t0))
	if n, err := Import(f, dir, "one-cloud", "staging", path, "site"); err != nil || n != 1 {
		t.Fatalf("in place: %d, %v", n, err)
	}
	if got := mustRead(t, path); len(got) != 1 || got[0].App != "one-cloud" {
		t.Errorf("after: %+v", got)
	}
}

func TestImportRefusesWhatCannotBeTrue(t *testing.T) {
	f := testFleet()
	for name, tc := range map[string]struct {
		field   string
		entries []Entry
		want    string
	}{
		"no such field":   {"site", nil, "no entries"},
		"another stack":   {"app", []Entry{func() Entry { e := deploy(rel1, "", "abc1234", t0); e.Stack = "prod"; return e }()}, "not one-cloud/staging"},
		"a broken chain":  {"app", []Entry{deploy(rel1, "", "abc1234", t0), deploy(rel2, rel3, "def5678", t0.Add(time.Hour))}, "line 2"},
		"the wrong field": {"site", []Entry{deploy(rel1, "", "abc1234", t0)}, ""},
	} {
		src := filepath.Join(t.TempDir(), "old.jsonl")
		legacy(t, src, tc.field, tc.entries...)
		field := tc.field
		if name == "the wrong field" {
			field = "name"
			tc.want = `no "name" field`
		}
		dir := t.TempDir()
		if _, err := Import(f, dir, "one-cloud", "staging", src, field); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		a, _ := f.App("one-cloud")
		if _, err := os.Stat(Path(dir, f, a, "staging")); err == nil {
			t.Errorf("%s: a ledger was written", name)
		}
	}
}
