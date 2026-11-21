package cli

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deploy/deployer"
	"github.com/plexusone/deployforge/deploy/host"
	"github.com/plexusone/deployforge/fleet"
)

func hostCmd(load func() (*fleet.Fleet, error), dial func(key string) deployer.Dialer) *cobra.Command {
	root := &cobra.Command{
		Use:   "host",
		Short: "Prepare an app stack's place on its host",
	}
	var apply bool
	var sshKey string
	setup := &cobra.Command{
		Use:   "setup <app> <stack>",
		Short: "Print the stack's host-setup script, or with --apply run it on the stack's host",
		Long: "The script makes the stack's Unix user, its /opt and /etc directories, its systemd unit, the\n" +
			"rotation of its log files, and the /etc/hosts lines for its loopback host names. It is idempotent and\n" +
			"never starts the service: a stack has no release to run until its first deploy.",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) (err error) {
			f, err := load()
			if err != nil {
				return err
			}
			script, err := host.Render(f, args[0], args[1])
			if err != nil {
				return err
			}
			if !apply {
				_, err := c.OutOrStdout().Write(script)
				return err
			}
			_, st, err := f.Stack(args[0], args[1])
			if err != nil {
				return err
			}
			h, ok := f.Host(st.Host)
			if !ok {
				return fmt.Errorf("no host %q", st.Host)
			}
			tr, err := dial(sshKey)(h)
			if err != nil {
				return err
			}
			local, err := os.CreateTemp("", "deployforge-host-setup-*.sh")
			if err != nil {
				return err
			}
			defer func() {
				if rerr := os.Remove(local.Name()); rerr != nil {
					err = errors.Join(err, fmt.Errorf("host setup: removing the local copy: %w", rerr))
				}
			}()
			if _, err := local.Write(script); err != nil {
				return fmt.Errorf("host setup: %w", err)
			}
			if err := local.Close(); err != nil {
				return fmt.Errorf("host setup: %w", err)
			}
			work, err := tr.TempDir(c.Context())
			if err != nil {
				return fmt.Errorf("host setup: temporary directory on %s: %w", h.Name, err)
			}
			remote := work + "/host-setup.sh"
			if err := tr.Upload(c.Context(), local.Name(), remote); err != nil {
				return fmt.Errorf("host setup: %w", err)
			}
			var out bytes.Buffer
			runErr := tr.Sudo(c.Context(), []string{"bash", remote}, nil, &out, c.ErrOrStderr())
			cleanErr := tr.Sudo(c.Context(), []string{"rm", "-rf", work}, nil, &out, c.ErrOrStderr())
			if _, err := c.OutOrStdout().Write(out.Bytes()); err != nil {
				return err
			}
			if runErr != nil {
				return fmt.Errorf("host setup on %s: %w", h.Name, runErr)
			}
			if cleanErr != nil {
				return fmt.Errorf("host setup on %s: removing %s: %w", h.Name, filepath.Base(work), cleanErr)
			}
			return nil
		},
	}
	setup.Flags().BoolVar(&apply, "apply", false, "run the script on the stack's host over SSH, with sudo")
	setup.Flags().StringVar(&sshKey, "ssh-key", cmp.Or(os.Getenv(EnvSSHKey), ""), "the host's SSH private key, used through the agent SSH_AUTH_SOCK names")
	root.AddCommand(setup)
	return root
}
