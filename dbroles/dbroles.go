// Package dbroles provisions an app stack's databases and roles on the
// fleet's PostgreSQL server: it builds a pgutil/provision plan from the
// fleet document, keeps each role's password in the stack's parameters,
// reusing one that is there and making one that is not, and runs the plan
// with psql on a fleet host that reaches the server.
//
// Each database has an owner role, <db>_owner, which migrates it, and, when
// the app runs as a separate role, an app role, <db>_app, with rows only.
// The role the app runs as gets the stack's connection limit and timeouts.
//
// A stack still running elsewhere keeps its live parameters until it moves:
// its new values wait under <prefix>next/, which a deploy never reads.
package dbroles

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/grokify/pgutil"
	"github.com/grokify/pgutil/provision"
	"github.com/grokify/pgutil/scram"

	"github.com/plexusone/deployforge/fleet"
)

// Timeouts set on the roles apps run as.
const (
	StatementTimeout         = "30s"
	IdleInTransactionTimeout = "60s"
)

// StagedDir is where a stack's new values wait, under its prefix, while its
// live parameters still serve another server.
const StagedDir = "next/"

// Store is a stack's parameters, by full name.
type Store interface {
	// Get returns a parameter's decrypted value, and false when there is
	// none.
	Get(ctx context.Context, name string) (value string, found bool, err error)
	// Put creates a secret parameter; it never overwrites one.
	Put(ctx context.Context, name, value string) error
}

// Server is the PostgreSQL server, as provisioning reaches it.
type Server struct {
	// Host is its endpoint.
	Host string
	// Port is 5432 when zero.
	Port int
	// Lockdown are its other databases, which no app's role may reach.
	Lockdown []string
	// Reachable are databases the provider owns whose CONNECT cannot be
	// revoked; they hold no app's data.
	Reachable []string
}

// Write is a parameter Apply creates. Its value is a secret: it is never
// printed or logged.
type Write struct {
	Name  string
	Value string
}

// Prepared is a stack's plan with the parameters to create and reuse.
type Prepared struct {
	App, Stack string
	// Base is where its parameters are: the stack's prefix, with StagedDir
	// when Staged.
	Base string
	// Staged is true for a stack running elsewhere until it moves.
	Staged bool
	// Plan is the databases and roles, with SCRAM verifiers.
	Plan provision.Plan
	// Writes are the parameters to create.
	Writes []Write
	// Reused are the parameters already there, whose passwords are kept.
	Reused []string
}

// param is one parameter of a database's roles.
type param struct {
	name, role, database string
	// password marks a bare password rather than a connection string.
	password bool
}

// params are a database's parameters, as the fleet document names them.
func params(d fleet.Database) ([]param, error) {
	if d.OwnerDSN == "" {
		return nil, fmt.Errorf("dbroles: database %s names no ownerDsn parameter", d.Name)
	}
	out := []param{{name: d.OwnerDSN, role: d.Owner(), database: d.Name}}
	if d.AppRole {
		if d.AppDSN == "" {
			return nil, fmt.Errorf("dbroles: database %s has an app role but names no appDsn parameter", d.Name)
		}
		out = append(out, param{name: d.AppDSN, role: d.App(), database: d.Name})
		if d.AppPassword != "" {
			out = append(out, param{name: d.AppPassword, role: d.App(), database: d.Name, password: true})
		}
	}
	return out, nil
}

// Prepare builds the stack's plan. A role's password comes from its
// parameters under Base when one exists, and is otherwise made with
// crypto/rand and written by Apply. A parameter naming another server, role
// or database is an error, never overwritten.
func Prepare(ctx context.Context, store Store, f *fleet.Fleet, app, stack string, server Server) (*Prepared, error) {
	a, s, err := f.Stack(app, stack)
	if err != nil {
		return nil, fmt.Errorf("dbroles: %w", err)
	}
	if s.Limits == nil {
		return nil, fmt.Errorf("dbroles: %s/%s has no limits in the fleet document; its roles need a connection limit", app, stack)
	}
	if len(s.Databases) == 0 {
		return nil, fmt.Errorf("dbroles: %s/%s has no databases", app, stack)
	}
	p := &Prepared{App: app, Stack: stack, Base: a.SecretPrefixFor(stack)}
	if s.Status == fleet.Elsewhere {
		p.Base += StagedDir
		p.Staged = true
	}

	passwords := map[string]string{}
	var missing []param
	for _, d := range s.Databases {
		prms, err := params(d)
		if err != nil {
			return nil, err
		}
		for _, prm := range prms {
			name := p.Base + prm.name
			value, found, err := store.Get(ctx, name)
			if err != nil {
				return nil, fmt.Errorf("dbroles: reading %s: %w", name, err)
			}
			if !found {
				missing = append(missing, prm)
				continue
			}
			pw, err := passwordOf(prm, value, server)
			if err != nil {
				return nil, fmt.Errorf("dbroles: %s: %w", name, err)
			}
			if prev, ok := passwords[prm.role]; ok && prev != pw {
				return nil, fmt.Errorf("dbroles: %s's parameters hold different passwords; make them agree before applying", prm.role)
			}
			passwords[prm.role] = pw
			p.Reused = append(p.Reused, name)
		}
	}
	for _, prm := range missing {
		pw, ok := passwords[prm.role]
		if !ok {
			if pw, err = newPassword(); err != nil {
				return nil, err
			}
			passwords[prm.role] = pw
		}
		value := pw
		if !prm.password {
			value = pgutil.DSN{User: prm.role, Password: pw, Host: server.Host, Port: server.Port, Database: prm.database, SSLMode: "require"}.String()
		}
		p.Writes = append(p.Writes, Write{Name: p.Base + prm.name, Value: value})
	}

	verifier := func(role string) (string, error) { return scram.Verifier(passwords[role]) }
	if p.Plan, err = plan(s.Databases, s.Limits.ConnectionLimit, server, verifier); err != nil {
		return nil, err
	}
	return p, nil
}

// VerifyPlan is the stack's plan for checking the server. It reads no
// secrets: the verifiers it holds are placeholders, which VerifySQL never
// uses.
func VerifyPlan(f *fleet.Fleet, app, stack string, server Server) (provision.Plan, error) {
	_, s, err := f.Stack(app, stack)
	if err != nil {
		return provision.Plan{}, fmt.Errorf("dbroles: %w", err)
	}
	if s.Limits == nil {
		return provision.Plan{}, fmt.Errorf("dbroles: %s/%s has no limits in the fleet document; its roles need a connection limit", app, stack)
	}
	placeholder, err := scram.VerifierWithSalt("placeholder", []byte("placeholder"), 1)
	if err != nil {
		return provision.Plan{}, err
	}
	return plan(s.Databases, s.Limits.ConnectionLimit, server, func(string) (string, error) { return placeholder, nil })
}

// plan builds the provision plan: the role the app runs as, the app role
// when there is one and the owner otherwise, gets the stack's connection
// limit and the timeouts.
func plan(dbs []fleet.Database, limit int, server Server, verifier func(role string) (string, error)) (provision.Plan, error) {
	var out provision.Plan
	role := func(name string, runtime bool) (provision.Role, error) {
		v, err := verifier(name)
		if err != nil {
			return provision.Role{}, fmt.Errorf("dbroles: %s: %w", name, err)
		}
		r := provision.Role{Name: name, Verifier: v}
		if runtime {
			r.ConnectionLimit, r.IdleInTransactionTimeout = limit, IdleInTransactionTimeout
		}
		return r, nil
	}
	for _, d := range dbs {
		owner, err := role(d.Owner(), !d.AppRole)
		if err != nil {
			return out, err
		}
		pd := provision.Database{Name: d.Name, Owner: owner}
		if d.AppRole {
			app, err := role(d.App(), true)
			if err != nil {
				return out, err
			}
			app.StatementTimeout = StatementTimeout
			pd.App = &app
		}
		out.Databases = append(out.Databases, pd)
	}
	out.Lockdown, out.Reachable = server.Lockdown, server.Reachable
	return out, out.Validate()
}

// passwordOf reads a role's password from its parameter, checking that a
// connection string names the role, the database and this server.
func passwordOf(prm param, value string, server Server) (string, error) {
	if prm.password {
		if value == "" {
			return "", errors.New("the password is empty")
		}
		return value, nil
	}
	d, err := pgutil.ParseDSN(value)
	if err != nil {
		return "", err
	}
	port := server.Port
	if port == 0 {
		port = 5432
	}
	if d.Port == 0 {
		d.Port = 5432
	}
	switch {
	case d.User != prm.role:
		return "", fmt.Errorf("it connects as %s, not %s", d.User, prm.role)
	case d.Database != prm.database:
		return "", fmt.Errorf("it connects to %s, not %s", d.Database, prm.database)
	case d.Host != server.Host || d.Port != port:
		return "", fmt.Errorf("it points at another server, %s:%d", d.Host, d.Port)
	case d.Password == "":
		return "", errors.New("it has no password")
	}
	return d.Password, nil
}

func newPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("dbroles: making a password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Runner runs SQL on the server as its administrator and returns the
// output, without psql's headers or alignment.
type Runner interface {
	Run(ctx context.Context, sql string) (string, error)
}

// Apply creates the missing parameters, then runs the plan's script.
// Parameters come first, so a password the server takes is never lost.
func Apply(ctx context.Context, p *Prepared, store Store, run Runner) error {
	for _, w := range p.Writes {
		if err := store.Put(ctx, w.Name, w.Value); err != nil {
			return fmt.Errorf("dbroles: writing %s: %w", w.Name, err)
		}
	}
	script, err := p.Plan.Script(provision.ScriptOptions{})
	if err != nil {
		return err
	}
	if _, err := run.Run(ctx, script); err != nil {
		return fmt.Errorf("dbroles: applying the plan: %w", err)
	}
	return nil
}

// Verify runs the plan's check and returns each problem; none means the
// server matches.
func Verify(ctx context.Context, plan provision.Plan, run Runner) ([]string, error) {
	q, err := plan.VerifySQL()
	if err != nil {
		return nil, err
	}
	out, err := run.Run(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("dbroles: verifying: %w", err)
	}
	var problems []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			problems = append(problems, l)
		}
	}
	return problems, nil
}
