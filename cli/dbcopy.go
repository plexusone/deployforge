package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/dbcopy"
	"github.com/plexusone/deployforge/dbroles"
	"github.com/plexusone/deployforge/fleet"
)

// copyCmd moves a stack's databases between servers: copy, compare and
// promote (package dbcopy).
func copyCmd(load func() (*fleet.Fleet, error), openParams func(*cobra.Command) (dbroles.Store, error)) *cobra.Command {
	var sshKey, from, to string
	var skip []string
	root := &cobra.Command{
		Use:   "copy",
		Short: "Copy a stack's databases from one server to another, compare their row counts, and promote the new one",
		Long: "copy reads each database's owner connection under --from and --to, each live, next or previous under the\n" +
			"stack's secret prefix, and runs pg_dump | psql on the database server's via host in one transaction that\n" +
			"first drops what the target's owner has, with no owners or grants from the source, so the target ends\n" +
			"with exactly the source's objects and its owner owns everything. The connections stay in memory and\n" +
			"reach the host on ssh's standard input.\n\n" +
			"Moving a stack: db roles apply makes the new server's roles under next/; copy run --from live --to\n" +
			"next; copy promote; deploy; then, for writes made meanwhile, copy run --from previous --to live.",
	}
	pf := root.PersistentFlags()
	pf.StringVar(&from, "from", "live", "the source connections: live, next or previous")
	pf.StringVar(&to, "to", "next", "the target connections: live, next or previous")
	pf.StringArrayVar(&skip, "skip", nil, "a database not to copy, left for the app's migrate to build; repeat for each")
	pf.StringVar(&sshKey, "ssh-key", os.Getenv(EnvSSHKey), "the via host's SSH private key, used through the agent SSH_AUTH_SOCK names")

	prepare := func(c *cobra.Command, args []string) ([]dbcopy.Copy, dbroles.Remote, error) {
		src, ok1 := dbcopy.Dirs[from]
		dst, ok2 := dbcopy.Dirs[to]
		if !ok1 || !ok2 {
			return nil, dbroles.Remote{}, errors.New("db copy: --from and --to are each live, next or previous")
		}
		f, err := load()
		if err != nil {
			return nil, dbroles.Remote{}, err
		}
		store, err := openParams(c)
		if err != nil {
			return nil, dbroles.Remote{}, err
		}
		srv, err := dbroles.ResolveServer(c.Context(), f, store, nil)
		if err != nil {
			return nil, dbroles.Remote{}, err
		}
		copies, err := dbcopy.Plan(c.Context(), store, f, args[0], args[1], src, dst, skip...)
		if err != nil {
			return nil, dbroles.Remote{}, err
		}
		for _, cp := range copies {
			for _, n := range cp.Notes {
				if _, err := fmt.Fprintf(c.OutOrStdout(), "note: %s\n", n); err != nil {
					return nil, dbroles.Remote{}, err
				}
			}
		}
		return copies, dbroles.Remote{SSH: srv.PSQL(sshKey).SSH}, nil
	}

	var confirm bool
	run := &cobra.Command{
		Use:   "run <app> <stack>",
		Short: "Copy each database from --from to --to, replacing what the target has, then compare row counts; needs --confirm",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			if !confirm {
				return errors.New("db copy: run replaces each target database's contents; run again with --confirm")
			}
			copies, r, err := prepare(c, args)
			if err != nil {
				return err
			}
			for _, cp := range copies {
				if _, err := fmt.Fprintf(c.OutOrStdout(), "copying %s from %s to %s\n", cp.Database, from, to); err != nil {
					return err
				}
				if err := dbcopy.Run(c.Context(), []dbcopy.Copy{cp}, r); err != nil {
					return err
				}
			}
			return compareCopies(c, copies, r)
		},
	}
	run.Flags().BoolVar(&confirm, "confirm", false, "copy")
	root.AddCommand(run)

	root.AddCommand(&cobra.Command{
		Use:   "verify <app> <stack>",
		Short: "Compare each database's tables and exact row counts under --from and --to; exit non-zero on any difference",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			copies, r, err := prepare(c, args)
			if err != nil {
				return err
			}
			return compareCopies(c, copies, r)
		},
	})

	var confirmPromote bool
	promote := &cobra.Command{
		Use:   "promote <app> <stack>",
		Short: "Switch the stack's live database parameters to the staged ones, keeping the old under previous/; needs --confirm",
		Long: "promote checks that every staged parameter exists, then keeps each live one under previous/, unless one\n" +
			"is there already, and replaces it with its staged value. Deploys then use the new server. Set the\n" +
			"stack's status to live in the fleet document, and deploy.",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			if !confirmPromote {
				return errors.New("db copy: promote switches the live parameters to the new server; run again with --confirm")
			}
			f, err := load()
			if err != nil {
				return err
			}
			p, err := openParams(c)
			if err != nil {
				return err
			}
			store, ok := p.(dbcopy.Store)
			if !ok {
				return errors.New("db copy: the parameter store cannot replace a value, which promote needs")
			}
			done, err := dbcopy.Promote(c.Context(), store, f, args[0], args[1])
			for _, n := range done {
				if _, werr := fmt.Fprintf(c.OutOrStdout(), "promoted %s\n", n); werr != nil {
					return errors.Join(err, werr)
				}
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%d parameter(s) promoted\n", len(done))
			return err
		},
	}
	promote.Flags().BoolVar(&confirmPromote, "confirm", false, "promote")
	root.AddCommand(promote)
	return root
}

// compareCopies counts every table of each source and target and reports
// each difference.
func compareCopies(c *cobra.Command, copies []dbcopy.Copy, r dbroles.Remote) error {
	bad := 0
	for _, cp := range copies {
		src, err := dbcopy.Counts(c.Context(), cp.Source, r)
		if err != nil {
			return fmt.Errorf("db copy: counting %s's source: %w", cp.Database, err)
		}
		dst, err := dbcopy.Counts(c.Context(), cp.Target, r)
		if err != nil {
			return fmt.Errorf("db copy: counting %s's target: %w", cp.Database, err)
		}
		diffs := dbcopy.CompareCounts(src, dst)
		var rows int64
		for _, n := range src {
			rows += n
		}
		if _, err := fmt.Fprintf(c.OutOrStdout(), "%s: %d tables, %d rows in the source; %d difference(s)\n", cp.Database, len(src), rows, len(diffs)); err != nil {
			return err
		}
		for _, d := range diffs {
			if _, err := fmt.Fprintf(c.OutOrStdout(), "  %s\n", d); err != nil {
				return err
			}
		}
		bad += len(diffs)
	}
	if bad > 0 {
		return fmt.Errorf("db copy: %d difference(s) between source and target", bad)
	}
	return nil
}
