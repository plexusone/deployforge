package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/dbcopy"
	"github.com/plexusone/deployforge/dbimport"
	"github.com/plexusone/deployforge/dbroles"
	"github.com/plexusone/deployforge/fleet"
)

// importCmd loads a data-only dump into one of a stack's databases (package
// dbimport).
func importCmd(load func() (*fleet.Fleet, error), openParams func(*cobra.Command) (dbroles.Store, error), hook func(*cobra.Command) ImportReplacer) *cobra.Command {
	var sshKey, file string
	var replaces []string
	var confirm bool
	var replacer ImportReplacer
	cmd := &cobra.Command{
		Use:   "import <app> <stack> <database> --file <dump>",
		Short: "Load a data-only dump into a stack's database as its owner, in one transaction; rolled back without --confirm",
		Long: "import reads a plain data-only dump (pg_dump --data-only), replaces text in it (--replace OLD=NEW, such as\n" +
			"an ID that differs between the source and the stack, or a local host name), and loads it into the\n" +
			"database as its owner, with psql on the database server's via host, in one transaction. Tables whose\n" +
			"row-level security is forced on their owner have FORCE lifted for the load and put back. Without\n" +
			"--confirm the transaction is rolled back, so the row counts show what the load would give. The dump\n" +
			"may hold no psql meta-command outside COPY data. The owner's connection reaches the host on ssh's\n" +
			"standard input.",
		Args: cobra.ExactArgs(3),
		RunE: func(c *cobra.Command, args []string) error {
			if file == "" {
				return errors.New("db import: set --file to a data-only dump")
			}
			var rs []dbimport.Replacement
			for _, s := range replaces {
				r, err := dbimport.ParseReplacement(s)
				if err != nil {
					return err
				}
				rs = append(rs, r)
			}
			data, err := os.ReadFile(file) //nolint:gosec // G304: the operator names the dump
			if err != nil {
				return fmt.Errorf("db import: %w", err)
			}
			if len(dbimport.Tables(data)) == 0 {
				return fmt.Errorf("db import: %s has no COPY data for public tables", file)
			}
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
			owners, err := dbcopy.Owners(c.Context(), store, f, args[0], args[1], dbcopy.Live, args[2])
			if err != nil {
				return err
			}
			if len(owners) != 1 {
				return fmt.Errorf("db import: %s %s has no database %s", args[0], args[1], args[2])
			}
			dsn := owners[0].DSN
			r := dbroles.Remote{SSH: srv.PSQL(sshKey).SSH}
			out := c.OutOrStdout()
			if replacer != nil {
				more, err := replacer(c.Context(), dsn, r, out)
				if err != nil {
					return err
				}
				rs = append(rs, more...)
			}
			result, err := dbimport.Import(c.Context(), dsn, dbimport.Replace(data, rs), confirm, r)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprint(out, result); err != nil {
				return err
			}
			if !confirm {
				_, err = fmt.Fprintln(out, "rolled back: run again with --confirm to load it")
			}
			return err
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&file, "file", "", "the data-only dump (pg_dump --data-only, plain format)")
	fl.StringArrayVar(&replaces, "replace", nil, "OLD=NEW text to replace throughout the dump, such as an ID or a host; repeat for each")
	fl.BoolVar(&confirm, "confirm", false, "commit the load; without it, the transaction is rolled back")
	fl.StringVar(&sshKey, "ssh-key", os.Getenv(EnvSSHKey), "the via host's SSH private key, used through the agent SSH_AUTH_SOCK names")
	if hook != nil {
		replacer = hook(cmd)
	}
	return cmd
}
