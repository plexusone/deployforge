package dbcopy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/grokify/pgutil"

	"github.com/plexusone/deployforge/dbroles"
	"github.com/plexusone/deployforge/fleet"
)

const prefix = "/example/prod/one-cloud/"

// memStore is a parameter store in memory; Put never overwrites.
type memStore map[string]string

func (m memStore) Get(_ context.Context, n string) (string, bool, error) {
	v, ok := m[n]
	return v, ok, nil
}

func (m memStore) Put(_ context.Context, n, v string) error {
	if _, ok := m[n]; ok {
		return errors.New("exists")
	}
	m[n] = v
	return nil
}

func (m memStore) Set(_ context.Context, n, v string) error {
	m[n] = v
	return nil
}

type fakeRunner struct{}

func (fakeRunner) Run(context.Context, string) (string, error) { return "", nil }

// elsewhere is a fleet whose prod stack still runs on its old server.
func elsewhere() *fleet.Fleet {
	return &fleet.Fleet{
		FormatVersion: fleet.FormatVersion, PortRange: fleet.PortRange{Min: 8101, Max: 8199},
		Hosts: []fleet.Host{{Name: "web-1", Address: "203.0.113.10"}},
		Apps: []fleet.App{{
			Name: "one-cloud", Repo: "github.com/example/one-cloud", Domain: "one.example.com",
			SecretPrefix: "/example/{stack}/one-cloud/",
			Stacks: map[string]fleet.Stack{"prod": {
				Host: "web-1", Hosts: []string{"one.example.com"}, Port: 8101, Status: fleet.Elsewhere,
				Databases: []fleet.Database{
					{Name: "one_auth", OwnerDSN: "AUTH_DB_DSN"},
					{Name: "one", AppRole: true, OwnerDSN: "migrate/ONE_PG_ADMIN_DSN", AppDSN: "ONE_PG_DSN", AppPassword: "migrate/ONE_APP_PASSWORD"},
				},
				Limits: &fleet.Limits{MemoryMax: "512M", CPUQuota: "50%", ConnectionLimit: 10},
			}},
		}},
	}
}

// old is a connection on the old server; its password is a test value.
func old(db string) string {
	return pgutil.DSN{User: "old", Password: "pw", Host: "old.server", Database: db}.String()
}

// staged makes the new server's parameters as db roles apply does.
func staged(t *testing.T, store memStore, f *fleet.Fleet) {
	t.Helper()
	p, err := dbroles.Prepare(context.Background(), store, f, "one-cloud", "prod", dbroles.Server{Host: "new.server"})
	if err != nil {
		t.Fatal(err)
	}
	if err := dbroles.Apply(context.Background(), p, store, fakeRunner{}); err != nil {
		t.Fatal(err)
	}
}

func TestPlanAndPromote(t *testing.T) {
	ctx := context.Background()
	f := elsewhere()
	store := memStore{
		prefix + "AUTH_DB_DSN":              old("one_auth"),
		prefix + "migrate/ONE_PG_ADMIN_DSN": pgutil.DSN{User: "admin", Password: "pw", Host: "old.server", Database: "postgres"}.String(),
		prefix + "ONE_PG_DSN":               old("one"),
		prefix + "migrate/ONE_APP_PASSWORD": "old-password",
	}
	if _, err := Plan(ctx, store, f, "one-cloud", "prod", Live, Staged); err == nil || !strings.Contains(err.Error(), "next/AUTH_DB_DSN does not exist") {
		t.Errorf("a copy to unstaged parameters: %v", err)
	}
	staged(t, store, f)
	copies, err := Plan(ctx, store, f, "one-cloud", "prod", Live, Staged)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 || copies[0].Source != old("one_auth") || copies[0].AppRole != "" || copies[1].AppRole != "one_app" ||
		!strings.Contains(copies[1].Target, "one_owner:") {
		t.Fatalf("copies: %+v", copies)
	}
	// The migrate-only owner connection is a base connection: switched,
	// and noted without its password.
	src, err := pgutil.ParseDSN(copies[1].Source)
	if err != nil || src.Database != "one" || len(copies[1].Notes) != 1 || strings.Contains(copies[1].Notes[0], "pw") ||
		!strings.Contains(copies[1].Notes[0], `base connection to "postgres" as admin; switched to one`) {
		t.Errorf("a base connection: %+v, %v", copies[1].Notes, err)
	}

	done, err := Promote(ctx, store, f, "one-cloud", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 4 || store[prefix+"previous/AUTH_DB_DSN"] != old("one_auth") || store[prefix+"AUTH_DB_DSN"] != store[prefix+"next/AUTH_DB_DSN"] {
		t.Fatalf("promote: %v", done)
	}
	if again, err := Promote(ctx, store, f, "one-cloud", "prod"); err != nil || len(again) != 0 || store[prefix+"previous/AUTH_DB_DSN"] != old("one_auth") {
		t.Errorf("promote again: %v, %v", again, err)
	}
	// The last copy reads the old server through previous/.
	final, err := Plan(ctx, store, f, "one-cloud", "prod", Previous, Live)
	if err != nil || final[0].Source != old("one_auth") || !strings.Contains(final[0].Target, "one_auth_owner:") {
		t.Errorf("final copy: %+v, %v", final, err)
	}
	if _, err := Plan(ctx, store, f, "one-cloud", "prod", Live, Live); err == nil {
		t.Error("a copy onto itself was accepted")
	}
}

func TestPlanRefusesAnotherDatabaseAndUnknownSkips(t *testing.T) {
	ctx := context.Background()
	f := elsewhere()
	store := memStore{}
	staged(t, store, f)
	store[prefix+"AUTH_DB_DSN"] = pgutil.DSN{User: "admin", Password: "pw", Host: "old.server", Database: "postgres"}.String()
	store[prefix+"migrate/ONE_PG_ADMIN_DSN"] = old("one")
	_, err := Plan(ctx, store, f, "one-cloud", "prod", Live, Staged)
	if err == nil || !strings.Contains(err.Error(), `AUTH_DB_DSN names database "postgres", not one_auth`) || strings.Contains(err.Error(), "pw") {
		t.Errorf("another database: %v", err)
	}
	store[prefix+"AUTH_DB_DSN"] = old("one_auth")
	if copies, err := Plan(ctx, store, f, "one-cloud", "prod", Live, Staged, "one_auth"); err != nil || len(copies) != 1 || copies[0].Database != "one" {
		t.Errorf("skip: %+v, %v", copies, err)
	}
	if _, err := Plan(ctx, store, f, "one-cloud", "prod", Live, Staged, "nope"); err == nil {
		t.Error("skipping an unknown database was accepted")
	}
}

func TestPromoteNeedsEveryStagedValue(t *testing.T) {
	store := memStore{prefix + "next/AUTH_DB_DSN": "x"}
	if _, err := Promote(context.Background(), store, elsewhere(), "one-cloud", "prod"); err == nil {
		t.Error("promoted with values missing")
	}
	if len(store) != 1 {
		t.Errorf("a refused promote changed parameters: %v", store)
	}
}

// fakeRemote records each command and its standard input, and answers
// with out.
type fakeRemote struct {
	commands, stdins []string
	out              string
}

func (r *fakeRemote) Exec(_ context.Context, command, stdin string) (string, error) {
	r.commands, r.stdins = append(r.commands, command), append(r.stdins, stdin)
	return r.out, nil
}

func TestRunSendsTheConnectionsOnStandardInput(t *testing.T) {
	r := &fakeRemote{}
	err := Run(context.Background(), []Copy{{Database: "one", Source: "src", Target: "dst", AppRole: "one_app"}}, r)
	if err != nil || len(r.commands) != 1 || strings.Contains(r.commands[0], "src") ||
		!strings.HasPrefix(r.stdins[0], "src\ndst\none_app\n") || !strings.Contains(r.stdins[0], "pg_dump") {
		t.Errorf("run: %v, %q", err, r.commands)
	}
}

func TestCountsAndCompare(t *testing.T) {
	r := &fakeRemote{out: "public.a 3\npublic.b unreadable\n"}
	got, err := Counts(context.Background(), "dsn", r)
	if err != nil || got["public.a"] != 3 || got["public.b"] != Unreadable || !strings.HasPrefix(r.stdins[0], "dsn\nBEGIN;") {
		t.Fatalf("counts %v, %v", got, err)
	}
	if d := CompareCounts(map[string]int64{"a": 3}, map[string]int64{"a": 3}); len(d) != 0 {
		t.Errorf("equal counts differ: %v", d)
	}
	d := CompareCounts(map[string]int64{"a": 3, "b": 1}, map[string]int64{"a": 2, "c": 4})
	if len(d) != 3 || !strings.Contains(d[0], "3 rows in the source, 2 in the target") {
		t.Errorf("diffs %v", d)
	}
	if _, err := Counts(context.Background(), "dsn", &fakeRemote{out: "garbage"}); err == nil {
		t.Error("an unexpected line was accepted")
	}
}
