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

func TestInspect(t *testing.T) {
	const prefix = "/example/prod/example-app/"
	reg := credspec.NewRegistry(
		secret("present", "hosting", prefix+"SESSION_KEY", credspec.StatusActive),
		secret("missing", "hosting", prefix+"API_KEY", credspec.StatusActive),
		secret("planned", "hosting", prefix+"LATER", credspec.StatusPlanned),
	)
	held := []string{prefix + "SESSION_KEY", prefix + "STRAY", prefix + "next/DB_DSN", prefix + "previous/DB_DSN"}
	ch := &credforge.Checker{Connector: connector{ob: observer{held: held}}}

	in, err := preflight.Inspect(context.Background(), ch, reg, credspec.NewLog(), prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Present) != 1 || in.Present[0].Credential != "present" || in.Present[0].Recorded {
		t.Errorf("present %+v", in.Present)
	}
	if len(in.Missing) != 1 || in.Missing[0].Credential != "missing" {
		t.Errorf("missing %+v", in.Missing)
	}
	if len(in.Unlisted) != 1 || in.Unlisted[0] != prefix+"STRAY" || in.Staged != 2 {
		t.Errorf("unlisted %v, staged %d", in.Unlisted, in.Staged)
	}
	if len(in.Unrecorded()) != 1 {
		t.Errorf("unrecorded %+v", in.Unrecorded())
	}

	in, err = preflight.Inspect(context.Background(), ch, reg, credspec.NewLog(), prefix, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Missing) != 2 {
		t.Errorf("with planned, missing %+v", in.Missing)
	}
}
