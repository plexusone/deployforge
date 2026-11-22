// Package preflight checks, before a deploy, that every secret the
// credential registry stores under an app stack's prefix is in the
// parameter store. It lists names through credforge and reads no value.
package preflight

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/plexusone/credforge"
	"github.com/plexusone/systemspec-credential/credspec"
)

// Missing is a secret the registry stores under the prefix that the
// parameter store does not hold.
type Missing struct {
	Credential string
	Parameter  string
}

// Result is what the check found.
type Result struct {
	// Checked are the parameters looked for.
	Checked int
	// Missing are those the store does not hold.
	Missing []Missing
	// Unobserved are parameters no binding reaches, so nothing is known.
	Unobserved []string
}

// Err is an error naming each missing secret, or nil.
func (r Result) Err() error {
	if len(r.Missing) == 0 {
		return nil
	}
	lines := make([]string, len(r.Missing))
	for i, m := range r.Missing {
		lines[i] = m.Credential + " (" + m.Parameter + ")"
	}
	return fmt.Errorf("preflight: %d secret(s) the registry lists are not in the parameter store: %s; set them with deployforge credentials generate or install",
		len(r.Missing), strings.Join(lines, ", "))
}

// Check compares the active credentials with a primary SSM store under
// prefix against the store.
func Check(ctx context.Context, ch *credforge.Checker, reg *credspec.Registry, log *credspec.Log, prefix string) (Result, error) {
	under := func(c credspec.Credential) bool {
		return slices.ContainsFunc(c.Primary(), func(s credspec.Store) bool {
			return s.Type == credspec.StoreAWSSSM && strings.HasPrefix(s.Path, prefix)
		})
	}
	rep, err := ch.Check(ctx, reg, log, credforge.Options{Only: under, Grace: credforge.DefaultGrace})
	if err != nil {
		return Result{}, fmt.Errorf("preflight: %w", err)
	}
	var r Result
	for _, f := range rep.Findings {
		if f.Credential == "" || f.Store.Type != credspec.StoreAWSSSM || !strings.HasPrefix(f.Store.Path, prefix) {
			continue
		}
		r.Checked++
		switch f.State {
		case credforge.StateMissing:
			r.Missing = append(r.Missing, Missing{Credential: f.Credential, Parameter: f.Store.Path})
		case credforge.StateUnobserved:
			r.Unobserved = append(r.Unobserved, f.Store.Path)
		}
	}
	return r, nil
}
