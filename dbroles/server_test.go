package dbroles

import (
	"context"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
)

func TestResolveServer(t *testing.T) {
	f := withDatabases()
	f.DatabaseServer = &fleet.DatabaseServer{
		EndpointFrom:  &fleet.AddressSource{Pulumi: &fleet.PulumiOutput{Dir: "stack", Stack: "prod", Output: "dbEndpoint"}},
		AdminUser:     "admin",
		AdminDatabase: "fleet",
		AdminPassword: fleet.SecretSource{PulumiConfig: &fleet.PulumiConfig{Dir: "stack", Stack: "prod", Key: "fleet:dbMasterPassword"}},
		Via:           "web-1",
		Lockdown:      []string{"fleet"},
	}
	run := func(_ context.Context, _ string, argv ...string) ([]byte, error) {
		if argv[1] == "config" {
			return []byte("adm1n-pw\n"), nil
		}
		return []byte("db.internal\n"), nil
	}
	r, err := ResolveServer(context.Background(), f, nil, run)
	if err != nil {
		t.Fatal(err)
	}
	if r.Server.Host != "db.internal" || r.Port != 5432 || r.AdminPassword != "adm1n-pw" || r.Via.Address != "203.0.113.10" {
		t.Errorf("resolved %+v", r)
	}
	p := r.PSQL("/keys/fleet")
	if got := strings.Join(p.SSH, " "); got != "ssh -o BatchMode=yes -i /keys/fleet -o IdentityAgent=SSH_AUTH_SOCK ubuntu@203.0.113.10" {
		t.Errorf("ssh %s", got)
	}
	if strings.Contains(strings.Join(p.SSH, " "), "adm1n") {
		t.Error("the password is on the command line")
	}

	f.DatabaseServer.AdminPassword = fleet.SecretSource{Parameter: "/fleet/db-admin"}
	store := memStore{"/fleet/db-admin": "from-store"}
	if r, err := ResolveServer(context.Background(), f, store, run); err != nil || r.AdminPassword != "from-store" {
		t.Errorf("from the store: %v", err)
	}
	if _, err := ResolveServer(context.Background(), f, memStore{}, run); err == nil || !strings.Contains(err.Error(), "no administrator's password") {
		t.Errorf("a missing parameter: %v", err)
	}
	f.DatabaseServer = nil
	if _, err := ResolveServer(context.Background(), f, store, run); err == nil {
		t.Error("no server")
	}
}

func TestPSQLRefusesUnsafeValuesBeforeRunning(t *testing.T) {
	base := PSQL{SSH: []string{"false"}, Host: "db.internal", User: "admin", Database: "fleet", Password: "pw"}
	for name, mutate := range map[string]func(*PSQL){
		"a host with a quote":      func(p *PSQL) { p.Host = "db'; rm" },
		"a user that is not ident": func(p *PSQL) { p.User = "Ad Min" },
		"a two-line password":      func(p *PSQL) { p.Password = "a\nb" },
	} {
		p := base
		mutate(&p)
		if _, err := p.Run(context.Background(), "select 1"); err == nil || !strings.HasPrefix(err.Error(), "dbroles:") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
