package dbroles

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/grokify/pgutil"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/internal/fleettest"
)

type memStore map[string]string

func (m memStore) Get(_ context.Context, name string) (string, bool, error) {
	v, ok := m[name]
	return v, ok, nil
}

func (m memStore) Put(_ context.Context, name, value string) error {
	if _, ok := m[name]; ok {
		return errors.New("exists")
	}
	m[name] = value
	return nil
}

type fakeRunner struct {
	sql []string
	out string
}

func (f *fakeRunner) Run(_ context.Context, sql string) (string, error) {
	f.sql = append(f.sql, sql)
	return f.out, nil
}

var server = Server{Host: "db.internal", Lockdown: []string{"fleet"}, Reachable: []string{"template1"}}

// withDatabases gives prod an auth database the app runs as the owner of,
// and an app database with an app role.
func withDatabases() *fleet.Fleet {
	f := fleettest.Fleet()
	prod := f.Apps[0].Stacks["prod"]
	prod.Databases = []fleet.Database{
		{Name: "one_auth", OwnerDSN: "AUTH_DB_DSN"},
		{Name: "one", AppRole: true, OwnerDSN: "migrate/ONE_PG_ADMIN_DSN", AppDSN: "ONE_PG_DSN", AppPassword: "migrate/ONE_APP_PASSWORD"},
	}
	f.Apps[0].Stacks["prod"] = prod
	return f
}

func TestPrepareMakesThenReuses(t *testing.T) {
	ctx := context.Background()
	f, store := withDatabases(), memStore{}
	p, err := Prepare(ctx, store, f, "one-cloud", "prod", server)
	if err != nil {
		t.Fatal(err)
	}
	if p.Staged || p.Base != "/example/prod/one-cloud/" || len(p.Writes) != 4 || len(p.Reused) != 0 {
		t.Fatalf("prepared %+v", p)
	}
	run := &fakeRunner{}
	if err := Apply(ctx, p, store, run); err != nil {
		t.Fatal(err)
	}
	if len(run.sql) != 1 || !strings.Contains(run.sql[0], "one_owner") || strings.Contains(run.sql[0], store["/example/prod/one-cloud/migrate/ONE_APP_PASSWORD"]) {
		t.Errorf("the script is missing a role or holds a password:\n%s", run.sql)
	}
	app, err := pgutil.ParseDSN(store["/example/prod/one-cloud/ONE_PG_DSN"])
	if err != nil {
		t.Fatal(err)
	}
	if app.User != "one_app" || app.Host != "db.internal" || app.Password != store["/example/prod/one-cloud/migrate/ONE_APP_PASSWORD"] {
		t.Errorf("app DSN %+v does not hold the app role's password", app)
	}

	again, err := Prepare(ctx, store, f, "one-cloud", "prod", server)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Writes) != 0 || len(again.Reused) != 4 {
		t.Errorf("a second run writes %d and reuses %d", len(again.Writes), len(again.Reused))
	}
	// The role the app runs as gets the stack's limit: the auth database's
	// owner, and the app database's app role, not its owner.
	for _, d := range again.Plan.Databases {
		switch d.Name {
		case "one_auth":
			if d.Owner.ConnectionLimit != 10 || d.App != nil {
				t.Errorf("one_auth: %+v", d)
			}
		case "one":
			if d.Owner.ConnectionLimit != 0 || d.App == nil || d.App.ConnectionLimit != 10 || d.App.StatementTimeout != StatementTimeout {
				t.Errorf("one: owner %+v app %+v", d.Owner, d.App)
			}
		}
	}
}

func TestAStackElsewhereStagesItsParameters(t *testing.T) {
	f := withDatabases()
	prod := f.Apps[0].Stacks["prod"]
	prod.Status = fleet.Elsewhere
	f.Apps[0].Stacks["prod"] = prod
	p, err := Prepare(context.Background(), memStore{}, f, "one-cloud", "prod", server)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Staged || p.Base != "/example/prod/one-cloud/next/" || !strings.HasPrefix(p.Writes[0].Name, "/example/prod/one-cloud/next/") {
		t.Errorf("prepared %+v", p)
	}
}

func TestPrepareRefusesParametersForAnotherServerRoleOrDatabase(t *testing.T) {
	for name, dsn := range map[string]string{
		"another server":   pgutil.DSN{User: "one_app", Password: "x", Host: "elsewhere", Database: "one"}.String(),
		"another role":     pgutil.DSN{User: "admin", Password: "x", Host: "db.internal", Database: "one"}.String(),
		"another database": pgutil.DSN{User: "one_app", Password: "x", Host: "db.internal", Database: "postgres"}.String(),
		"no password":      pgutil.DSN{User: "one_app", Host: "db.internal", Database: "one"}.String(),
	} {
		store := memStore{"/example/prod/one-cloud/ONE_PG_DSN": dsn}
		if _, err := Prepare(context.Background(), store, withDatabases(), "one-cloud", "prod", server); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	store := memStore{
		"/example/prod/one-cloud/ONE_PG_DSN":               pgutil.DSN{User: "one_app", Password: "a", Host: "db.internal", Database: "one"}.String(),
		"/example/prod/one-cloud/migrate/ONE_APP_PASSWORD": "b",
	}
	if _, err := Prepare(context.Background(), store, withDatabases(), "one-cloud", "prod", server); err == nil || !strings.Contains(err.Error(), "different passwords") {
		t.Errorf("two passwords for one role: %v", err)
	}
}

func TestPrepareNeedsParametersLimitsAndDatabases(t *testing.T) {
	f := withDatabases()
	prod := f.Apps[0].Stacks["prod"]
	prod.Databases[0].OwnerDSN = ""
	f.Apps[0].Stacks["prod"] = prod
	if _, err := Prepare(context.Background(), memStore{}, f, "one-cloud", "prod", server); err == nil || !strings.Contains(err.Error(), "no ownerDsn") {
		t.Errorf("no ownerDsn: %v", err)
	}
	f = withDatabases()
	prod = f.Apps[0].Stacks["prod"]
	prod.Limits = nil
	f.Apps[0].Stacks["prod"] = prod
	if _, err := Prepare(context.Background(), memStore{}, f, "one-cloud", "prod", server); err == nil || !strings.Contains(err.Error(), "no limits") {
		t.Errorf("no limits: %v", err)
	}
	if _, err := Prepare(context.Background(), memStore{}, withDatabases(), "one-cloud", "staging", server); err == nil {
		t.Error("a stack with no databases")
	}
}

func TestVerify(t *testing.T) {
	plan, err := VerifyPlan(withDatabases(), "one-cloud", "prod", server)
	if err != nil {
		t.Fatal(err)
	}
	run := &fakeRunner{out: "role one_app has no connection limit\n\n"}
	problems, err := Verify(context.Background(), plan, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || len(run.sql) != 1 {
		t.Errorf("problems %q", problems)
	}
}
