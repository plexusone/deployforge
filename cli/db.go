package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/grokify/pgutil/provision"
	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/dbroles"
	"github.com/plexusone/deployforge/deploy/secrets/awsssm"
	"github.com/plexusone/deployforge/fleet"
)

func dbCmd(load func() (*fleet.Fleet, error), o Options) *cobra.Command {
	root := &cobra.Command{
		Use:   "db",
		Short: "Provision app stacks' databases and roles, move them between servers, and snapshot the server",
	}
	openParams := o.Params
	if openParams == nil {
		openParams = func(c *cobra.Command) (dbroles.Store, error) { return awsssm.New(c.Context()) }
	}
	var sshKey string
	roles := &cobra.Command{
		Use:   "roles",
		Short: "Plan, apply and verify a stack's databases and roles on the fleet's database server",
		Long: "roles provisions each database of a stack with its <db>_owner role and, when the app runs as one, its\n" +
			"<db>_app role, with the stack's connection limit and timeouts on the role the app runs as, and CONNECT\n" +
			"for the stack's own roles only. Each role's password is kept in the stack's parameters: reused when there,\n" +
			"made and stored when not, and sent to the server only as a SCRAM verifier. psql runs on the database\n" +
			"server's via host, with the administrator's password on its standard input.",
	}
	roles.PersistentFlags().StringVar(&sshKey, "ssh-key", os.Getenv(EnvSSHKey), "the via host's SSH private key, used through the agent SSH_AUTH_SOCK names")

	prepare := func(c *cobra.Command, args []string) (*dbroles.Prepared, dbroles.Store, dbroles.Resolved, error) {
		f, err := load()
		if err != nil {
			return nil, nil, dbroles.Resolved{}, err
		}
		store, err := openParams(c)
		if err != nil {
			return nil, nil, dbroles.Resolved{}, err
		}
		srv, err := dbroles.ResolveServer(c.Context(), f, store, nil)
		if err != nil {
			return nil, nil, dbroles.Resolved{}, err
		}
		p, err := dbroles.Prepare(c.Context(), store, f, args[0], args[1], srv.Server)
		return p, store, srv, err
	}

	roles.AddCommand(&cobra.Command{
		Use:   "plan <app> <stack>",
		Short: "Show the parameters it would create or reuse, and the SQL, without passwords; change nothing",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			p, _, _, err := prepare(c, args)
			if err != nil {
				return err
			}
			return printRolesPlan(c.OutOrStdout(), p)
		},
	})

	var confirm bool
	apply := &cobra.Command{
		Use:   "apply <app> <stack>",
		Short: "Create the missing parameters, provision the databases and roles, then verify; needs --confirm",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			p, store, srv, err := prepare(c, args)
			if err != nil {
				return err
			}
			if err := printRolesPlan(c.OutOrStdout(), p); err != nil {
				return err
			}
			if !confirm {
				return errors.New("db roles: apply writes parameters and changes the server; read the plan above, then run again with --confirm")
			}
			run := srv.PSQL(sshKey)
			if err := dbroles.Apply(c.Context(), p, store, run); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(c.OutOrStdout(), "applied: %d parameter(s) created\n", len(p.Writes)); err != nil {
				return err
			}
			return verifyRoles(c, p.Plan, run)
		},
	}
	apply.Flags().BoolVar(&confirm, "confirm", false, "apply the plan")
	roles.AddCommand(apply)

	roles.AddCommand(&cobra.Command{
		Use:   "verify <app> <stack>",
		Short: "Check the server against the plan; exit non-zero on any difference. Reads no app's secret",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			store, err := openParams(c)
			if err != nil {
				return err
			}
			srv, err := dbroles.ResolveServer(c.Context(), f, store, nil)
			if err != nil {
				return err
			}
			plan, err := dbroles.VerifyPlan(f, args[0], args[1], srv.Server)
			if err != nil {
				return err
			}
			return verifyRoles(c, plan, srv.PSQL(sshKey))
		},
	})
	root.AddCommand(roles, copyCmd(load, openParams), snapshotCmd(load, lightsailOpener(o)))
	return root
}

func verifyRoles(c *cobra.Command, plan provision.Plan, run dbroles.Runner) error {
	problems, err := dbroles.Verify(c.Context(), plan, run)
	if err != nil {
		return err
	}
	for _, p := range problems {
		if _, err := fmt.Fprintf(c.OutOrStdout(), "  problem: %s\n", p); err != nil {
			return err
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("db roles: %d problem(s)", len(problems))
	}
	_, err = fmt.Fprintf(c.OutOrStdout(), "verified: %d database(s) match the plan, and no role reaches another's database\n", len(plan.Databases))
	return err
}

func printRolesPlan(w io.Writer, p *dbroles.Prepared) error {
	script, err := p.Plan.Script(provision.ScriptOptions{Redact: true})
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: parameters under %s", p.App, p.Stack, p.Base)
	if p.Staged {
		b.WriteString(" (staged: the stack's live parameters are untouched until it moves)")
	}
	b.WriteString("\n")
	for _, wr := range p.Writes {
		fmt.Fprintf(&b, "  create %s\n", wr.Name)
	}
	for _, r := range p.Reused {
		fmt.Fprintf(&b, "  reuse  %s\n", r)
	}
	b.WriteString("\nSQL, with each password as a redacted SCRAM verifier:\n\n")
	b.WriteString(script)
	_, err = io.WriteString(w, b.String())
	return err
}
