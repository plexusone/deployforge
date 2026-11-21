package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deploy/caddy"
	"github.com/plexusone/deployforge/deploy/cfranges"
	"github.com/plexusone/deployforge/deploy/deployer"
	"github.com/plexusone/deployforge/fleet"
)

// renderCaddyfile renders a host's Caddyfile with the committed Cloudflare
// ranges.
func renderCaddyfile(f *fleet.Fleet, host string, with []string) ([]byte, error) {
	ranges, err := cfranges.Ranges()
	if err != nil {
		return nil, err
	}
	return caddy.Render(f, caddy.Options{Host: host, Ranges: ranges, With: with})
}

func caddyfileCmd(load func() (*fleet.Fleet, error)) *cobra.Command {
	var with []string
	cmd := &cobra.Command{
		Use:   "caddyfile <host>",
		Short: "Print a host's Caddyfile: one block per live stack on it",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			out, err := renderCaddyfile(f, args[0], with)
			if err != nil {
				return err
			}
			_, err = c.OutOrStdout().Write(out)
			return err
		},
	}
	cmd.Flags().StringArrayVar(&with, "with", nil, "also serve this app/stack, which is not live yet; repeat for each")
	return cmd
}

// Where Caddy's configuration lives on a host.
const (
	caddyfilePath     = "/etc/caddy/Caddyfile"
	caddyfilePrevious = "/etc/caddy/Caddyfile.previous"
)

func caddyCmd(load func() (*fleet.Fleet, error), dial func(key string) deployer.Dialer) *cobra.Command {
	root := &cobra.Command{
		Use:   "caddy",
		Short: "Install a host's Caddyfile",
	}
	var with []string
	var sshKey string
	install := &cobra.Command{
		Use:   "install <host>",
		Short: "Validate the host's Caddyfile on the host, keep the previous one, install it and reload Caddy",
		Long: "install renders the host's Caddyfile, uploads it, has Caddy validate it on the host before anything is\n" +
			"replaced, keeps the running one as " + caddyfilePrevious + ", installs the new one and reloads Caddy, never\n" +
			"restarting it, so the other apps keep serving. To go back, copy the previous one over it and reload.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) (err error) {
			f, err := load()
			if err != nil {
				return err
			}
			out, err := renderCaddyfile(f, args[0], with)
			if err != nil {
				return err
			}
			h, _ := f.Host(args[0]) // Render found it
			tr, err := dial(sshKey)(h)
			if err != nil {
				return err
			}
			local, err := os.CreateTemp("", "deployforge-Caddyfile-*")
			if err != nil {
				return err
			}
			defer func() {
				if rerr := os.Remove(local.Name()); rerr != nil {
					err = errors.Join(err, fmt.Errorf("caddy install: removing the local copy: %w", rerr))
				}
			}()
			if _, err := local.Write(out); err != nil {
				return fmt.Errorf("caddy install: %w", err)
			}
			if err := local.Close(); err != nil {
				return fmt.Errorf("caddy install: %w", err)
			}
			ctx := c.Context()
			work, err := tr.TempDir(ctx)
			if err != nil {
				return fmt.Errorf("caddy install: temporary directory on %s: %w", h.Name, err)
			}
			remote := work + "/Caddyfile"
			if err := tr.Upload(ctx, local.Name(), remote); err != nil {
				return fmt.Errorf("caddy install: %w", err)
			}
			var log bytes.Buffer
			sudo := func(argv ...string) error { return tr.Sudo(ctx, argv, nil, &log, &log) }
			defer func() {
				if rerr := sudo("rm", "-rf", work); rerr != nil {
					err = errors.Join(err, fmt.Errorf("caddy install: removing %s on %s: %w", work, h.Name, rerr))
				}
			}()
			if err := sudo("caddy", "validate", "--config", remote, "--adapter", "caddyfile"); err != nil {
				return fmt.Errorf("caddy install: the new Caddyfile is not valid on %s, and nothing was changed: %w\n%s", h.Name, err, log.String())
			}
			// A host's first Caddyfile has nothing to keep.
			if sudo("test", "-f", caddyfilePath) == nil {
				if err := sudo("cp", "-p", caddyfilePath, caddyfilePrevious); err != nil {
					return fmt.Errorf("caddy install: keeping the previous Caddyfile: %w\n%s", err, log.String())
				}
			}
			if err := sudo("install", "-o", "root", "-g", "root", "-m", "0644", remote, caddyfilePath); err != nil {
				return fmt.Errorf("caddy install: %w\n%s", err, log.String())
			}
			if err := sudo("systemctl", "reload", "caddy"); err != nil {
				return fmt.Errorf("caddy install: installed, but the reload failed; copy %s back and reload: %w\n%s", caddyfilePrevious, err, log.String())
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%s: Caddyfile installed and Caddy reloaded; commit it if you keep it\n", h.Name)
			return err
		},
	}
	install.Flags().StringArrayVar(&with, "with", nil, "also serve this app/stack, which is not live yet; repeat for each")
	install.Flags().StringVar(&sshKey, "ssh-key", os.Getenv(EnvSSHKey), "the host's SSH private key, used through the agent SSH_AUTH_SOCK names")
	root.AddCommand(install)
	return root
}
