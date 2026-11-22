package preflight_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plexusone/credforge"
	"github.com/plexusone/systemspec-credential/credspec"

	"github.com/plexusone/deployforge/deploy/preflight"
)

// observer holds the parameter names in held.
type observer struct{ held []string }

func (o observer) Observe(_ context.Context, s credforge.Scope) ([]credforge.Observation, error) {
	var out []credforge.Observation
	for _, p := range o.held {
		k := credforge.Key{Type: s.Type, Account: s.Account, Path: p}
		if s.Contains(k) {
			out = append(out, credforge.Observation{Key: k, Modified: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)})
		}
	}
	return out, nil
}

type connector struct {
	ob      credforge.Observer
	unbound string
}

func (c connector) Observer(_, account string) (credforge.Observer, error) {
	if account == c.unbound {
		return nil, nil
	}
	return c.ob, nil
}

func (connector) References(string, string) (credforge.ReferenceFinder, error) { return nil, nil }

func secret(id, account, path string, status credspec.Status) credspec.Credential {
	return credspec.Credential{
		ID: id, Name: id, System: "example-app", Kind: credspec.KindRandomSecret, Status: status, Owner: "ops",
		Issuer:     credspec.Issuer{Type: credspec.IssuerSelf},
		Generation: credspec.Generation{Method: credspec.MethodRandom},
		Stores:     []credspec.Store{{Type: credspec.StoreAWSSSM, Role: credspec.RolePrimary, Account: account, Path: path}},
		Rotation:   credspec.Rotation{Policy: credspec.PolicyOnDemand},
		Handling:   credspec.Handling{Generate: credspec.ActorHuman, Distribute: credspec.ActorSystem, Rotate: credspec.ActorHuman},
	}
}

func TestCheck(t *testing.T) {
	const prefix = "/example/prod/example-app/"
	reg := credspec.NewRegistry(
		secret("present", "hosting", prefix+"SESSION_KEY", credspec.StatusActive),
		secret("missing", "hosting", prefix+"API_KEY", credspec.StatusActive),
		secret("unseen", "other", prefix+"OTHER", credspec.StatusActive),
		secret("planned", "hosting", prefix+"LATER", credspec.StatusPlanned),
		secret("elsewhere", "hosting", "/example/staging/example-app/API_KEY", credspec.StatusActive),
	)
	ch := &credforge.Checker{Connector: connector{ob: observer{held: []string{prefix + "SESSION_KEY"}}, unbound: "other"}}
	r, err := preflight.Check(context.Background(), ch, reg, credspec.NewLog(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	if r.Checked != 3 || len(r.Missing) != 1 || r.Missing[0].Credential != "missing" || len(r.Unobserved) != 1 {
		t.Fatalf("result %+v", r)
	}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "missing ("+prefix+"API_KEY)") {
		t.Errorf("error %v", err)
	}
	if (preflight.Result{Checked: 2}).Err() != nil {
		t.Error("nothing missing is an error")
	}
}
