package dbimport_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/dbimport"
)

const dump = `SET statement_timeout = 0;
\restrict abc
COPY public.items (id, org, name) FROM stdin;
1	org-old	First \N
2	org-old	Second
\.
COPY public.tags (id, item_id) FROM stdin;
1	1
\.
\unrestrict abc
`

type fakeRemote struct {
	command, stdin string
	out            string
	err            error
}

func (f *fakeRemote) Exec(_ context.Context, command, stdin string) (string, error) {
	f.command, f.stdin = command, stdin
	return f.out, f.err
}

func TestTables(t *testing.T) {
	if got := strings.Join(dbimport.Tables([]byte(dump)), ","); got != "items,tags" {
		t.Errorf("Tables = %s", got)
	}
}

func TestImportRollsBackWithoutApply(t *testing.T) {
	r := &fakeRemote{out: "items: 2 rows\ntags: 1 rows\n"}
	out, err := dbimport.Import(context.Background(), "postgres://owner@db/app", []byte(dump), false, r)
	if err != nil || out != r.out {
		t.Fatalf("Import = %q, %v", out, err)
	}
	if r.command != dbimport.Command {
		t.Errorf("command %q", r.command)
	}
	dsn, script, _ := strings.Cut(r.stdin, "\n")
	if dsn != "postgres://owner@db/app" {
		t.Errorf("the connection is not the first line of standard input: %q", dsn)
	}
	for _, want := range []string{"BEGIN;", "NO FORCE ROW LEVEL SECURITY", "COPY public.items", "SET row_security = off;",
		"SELECT 'items' || ': ' || count(*) || ' rows' FROM public.items;", " FORCE ROW LEVEL SECURITY", "ROLLBACK;"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	if strings.Contains(script, `\restrict`) || strings.Contains(script, "COMMIT;") {
		t.Error("the script keeps \\restrict or commits without apply")
	}
	if !strings.Contains(script, `1	org-old	First \N`) {
		t.Error("COPY data was changed")
	}
}

func TestImportCommitsWithApply(t *testing.T) {
	r := &fakeRemote{}
	if _, err := dbimport.Import(context.Background(), "dsn", []byte(dump), true, r); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(r.stdin), "COMMIT;") {
		t.Error("apply does not commit")
	}
}

func TestImportRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ dsn, data string }{
		"meta-command":    {"dsn", "\\connect other\nCOPY public.a (x) FROM stdin;\n1\n\\.\n"},
		"no COPY":         {"dsn", "SELECT 1;\n"},
		"unfinished COPY": {"dsn", "COPY public.a (x) FROM stdin;\n1\n"},
		"two-line dsn":    {"dsn\nmore", dump},
	} {
		r := &fakeRemote{}
		if _, err := dbimport.Import(context.Background(), tc.dsn, []byte(tc.data), false, r); err == nil {
			t.Errorf("%s: imported", name)
		}
		if r.stdin != "" {
			t.Errorf("%s: reached the host", name)
		}
	}
}

func TestImportReportsTheHostsError(t *testing.T) {
	r := &fakeRemote{out: "ERROR: duplicate key\n", err: errors.New("exit status 3")}
	if _, err := dbimport.Import(context.Background(), "dsn", []byte(dump), true, r); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("err = %v", err)
	}
}

func TestReplace(t *testing.T) {
	rs := []dbimport.Replacement{{Old: "org-old", New: "org-new"}, {Old: "org-new", New: "never"}}
	got := string(dbimport.Replace([]byte("a org-old b org-new"), rs))
	if got != "a org-new b never" {
		t.Errorf("Replace = %q", got)
	}
	if _, err := dbimport.ParseReplacement("=x"); err == nil {
		t.Error("an empty OLD parsed")
	}
	if r, err := dbimport.ParseReplacement("http://a.localhost=https://a.example.com"); err != nil || r.New != "https://a.example.com" {
		t.Errorf("ParseReplacement = %+v, %v", r, err)
	}
}
