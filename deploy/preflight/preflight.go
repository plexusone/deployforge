// Package preflight checks, before a deploy, that every secret the
// credential registry stores under an app stack's prefix is in the
// parameter store. It lists names through credforge and reads no value.
package preflight

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

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

// Found is a credential a store holds, with when the store last set it.
type Found struct {
	Credential string
	Parameter  string
	Modified   time.Time
	// Recorded is whether the event log records the credential.
	Recorded bool
}

// Inspection is everything a stack's prefix holds, set against the
// registry: what credentials stores hold, what they lack, and names under
// the prefix no credential names.
type Inspection struct {
	// Present are the credentials stores hold, recorded or not.
	Present []Found
	// Missing are the credentials stores lack. One that is optional, or
	// needed only while another rotates, is never missing.
	Missing []Missing
	// Unlisted are names under the prefix no credential names, apart from
	// Staged ones.
	Unlisted []string
	// Staged counts names under next/ and previous/ below the prefix: a
	// database move's staged or replaced values, kept beside the live ones.
	Staged int
	// Unobserved are parameters no binding reaches.
	Unobserved []string
	// Report is credforge's report, restricted to the prefix, for Record.
	Report credforge.Report
}

// Unrecorded are the present credentials the event log does not record.
func (in Inspection) Unrecorded() []Found {
	var out []Found
	for _, f := range in.Present {
		if !f.Recorded {
			out = append(out, f)
		}
	}
	return out
}

// Inspect lists the stack's prefix and compares it with every active
// credential stored under it, and with includePlanned the planned ones too,
// such as those of a stack not yet deployed.
func Inspect(ctx context.Context, ch *credforge.Checker, reg *credspec.Registry, log *credspec.Log, prefix string, includePlanned bool) (Inspection, error) {
	under := func(c credspec.Credential) bool {
		return slices.ContainsFunc(c.Primary(), func(s credspec.Store) bool {
			return s.Type == credspec.StoreAWSSSM && strings.HasPrefix(s.Path, prefix)
		})
	}
	rep, err := ch.Check(ctx, reg, log, credforge.Options{Only: under, IncludePlanned: includePlanned, Grace: credforge.DefaultGrace})
	if err != nil {
		return Inspection{}, fmt.Errorf("preflight: %w", err)
	}
	var in Inspection
	for _, f := range rep.Findings {
		if f.Store.Type != credspec.StoreAWSSSM || !strings.HasPrefix(f.Store.Path, prefix) {
			continue
		}
		in.Report.Findings = append(in.Report.Findings, f)
		rest := strings.TrimPrefix(f.Store.Path, prefix)
		switch {
		case f.Credential == "" && (strings.HasPrefix(rest, "next/") || strings.HasPrefix(rest, "previous/")):
			in.Staged++
		case f.Credential == "":
			if f.State == credforge.StateUnlisted {
				in.Unlisted = append(in.Unlisted, f.Store.Path)
			}
		case f.State == credforge.StateMissing:
			in.Missing = append(in.Missing, Missing{Credential: f.Credential, Parameter: f.Store.Path})
		case f.State == credforge.StateUnobserved:
			in.Unobserved = append(in.Unobserved, f.Store.Path)
		case f.Modified != nil:
			in.Present = append(in.Present, Found{Credential: f.Credential, Parameter: f.Store.Path, Modified: *f.Modified,
				Recorded: f.State != credforge.StateUnrecorded})
		}
	}
	return in, nil
}
