package cli

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deploy/cfranges"
	"github.com/plexusone/deployforge/deploy/deployer"
	"github.com/plexusone/deployforge/deploy/host"
	"github.com/plexusone/deployforge/deploy/systemd"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/hostports"
)

func hostCmd(load func() (*fleet.Fleet, error), dial func(key string) deployer.Dialer, ports func(c *cobra.Command) (hostports.API, error), adminCIDRs []string) *cobra.Command {
	root := &cobra.Command{
		Use:   "host",
		Short: "Prepare an app stack's place on its host, and check a host's firewall",
	}
	var apply, check bool
	var sshKey string
	setup := &cobra.Command{
		Use:   "setup <app> <stack>",
		Short: "Print the stack's host-setup script, with --apply run it on the stack's host, or with --check compare the host's unit",
		Long: "The script makes the stack's Unix user, its /opt and /etc directories, its systemd unit, the\n" +
			"rotation of its log files, and the /etc/hosts lines for its loopback host names. It is idempotent and\n" +
			"never starts the service: a stack has no release to run until its first deploy.\n\n" +
			"--check reads the unit installed on the host and compares it with the one the fleet document gives,\n" +
			"comments and blank lines aside, and exits non-zero when they differ, such as after a change to the\n" +
			"stack's limits. It changes nothing; --apply installs the new unit, and the next deploy restarts the\n" +
			"service under it.",
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
			if apply && check {
				return errors.New("host setup: give --apply or --check, not both")
			}
			if !apply && !check {
				_, err := c.OutOrStdout().Write(script)
				return err
			}
			a, st, err := f.Stack(args[0], args[1])
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
			if check {
				return checkUnit(c, tr, f, a, args[1], h.Name)
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
	setup.Flags().BoolVar(&check, "check", false, "compare the unit installed on the stack's host with the fleet document's, changing nothing")
	setup.Flags().StringVar(&sshKey, "ssh-key", cmp.Or(os.Getenv(EnvSSHKey), ""), "the host's SSH private key, used through the agent SSH_AUTH_SOCK names")
	root.AddCommand(setup, hostPortsCmd(load, ports, adminCIDRs))
	return root
}

// hostPortsCmd checks a host's Lightsail firewall (package hostports).
func hostPortsCmd(load func() (*fleet.Fleet, error), open func(c *cobra.Command) (hostports.API, error), adminCIDRs []string) *cobra.Command {
	var admin []string
	var ipv6 bool
	cmd := &cobra.Command{
		Use:   "ports <host> --admin-cidr <cidr>...",
		Short: "Check the host's Lightsail firewall: 443 open exactly to Cloudflare's ranges, 22 only to the admin CIDRs, nothing else",
		Long: "ports reads the Lightsail instance's port states, the host's lightsailName in the fleet document, and\n" +
			"exits non-zero unless HTTPS (443) is open exactly to Cloudflare's IPv4 ranges (with --ipv6, its IPv6\n" +
			"ranges too), SSH (22) only to the --admin-cidr addresses, and no other port is open. It changes nothing.\n\n" +
			"Run it after any change to the firewall. Lightsail replaces an instance's rules as a whole, and a tool\n" +
			"that replaces them new-first, then deletes the old set, can close the ports it just opened: every site\n" +
			"behind the host then times out while the host itself is healthy.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(admin) == 0 {
				return errors.New("host ports: give each address SSH should be open to with --admin-cidr")
			}
			var want hostports.Want
			for _, a := range admin {
				p, err := netip.ParsePrefix(a)
				if err != nil {
					return fmt.Errorf("host ports: --admin-cidr %q: %w", a, err)
				}
				want.Admin = append(want.Admin, p)
			}
			f, err := load()
			if err != nil {
				return err
			}
			h, ok := f.Host(args[0])
			if !ok {
				return fmt.Errorf("host ports: no host %s", args[0])
			}
			if h.LightsailName == "" {
				return fmt.Errorf("host ports: host %s has no lightsailName in the fleet document", h.Name)
			}
			ranges, err := cfranges.Ranges()
			if err != nil {
				return err
			}
			for _, p := range ranges {
				if p.Addr().Is4() || ipv6 {
					want.Edge = append(want.Edge, p)
				}
			}
			api, err := open(c)
			if err != nil {
				return err
			}
			r, err := hostports.Check(c.Context(), api, h.LightsailName, want)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			for _, l := range r.OK {
				if _, err := fmt.Fprintf(out, "  ok       %s\n", l); err != nil {
					return err
				}
			}
			for _, l := range r.Problems {
				if _, err := fmt.Fprintf(out, "  problem  %s\n", l); err != nil {
					return err
				}
			}
			if len(r.Problems) > 0 {
				return fmt.Errorf("host ports: %d problem(s) on %s", len(r.Problems), h.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&admin, "admin-cidr", adminCIDRs, "an address SSH should be open to, as a CIDR such as 192.0.2.10/32; repeat for each; the tool's admin addresses when not given")
	cmd.Flags().BoolVar(&ipv6, "ipv6", false, "expect HTTPS open to Cloudflare's IPv6 ranges too")
	return cmd
}

// checkUnit compares the stack's unit installed on its host with the one
// the fleet document gives, comments and blank lines aside, and fails with
// the first line that differs.
func checkUnit(c *cobra.Command, tr deployer.Transport, f *fleet.Fleet, a fleet.App, stack, host string) error {
	want, err := systemd.Render(f, a.Name, stack)
	if err != nil {
		return err
	}
	unit := f.Unit(a, stack)
	path := "/etc/systemd/system/" + unit + ".service"
	var got bytes.Buffer
	if err := tr.Sudo(c.Context(), []string{"cat", path}, nil, &got, io.Discard); err != nil {
		return fmt.Errorf("host setup --check: %s has no %s; run host setup %s %s --apply", host, path, a.Name, stack)
	}
	if line, mine, theirs, same := firstDifference(unitLines(got.Bytes()), unitLines(want)); !same {
		return fmt.Errorf("host setup --check: %s's %s differs from the fleet document's at line %d: the host has %q, the fleet gives %q; run host setup %s %s --apply, then deploy, which restarts the service under it",
			host, path, line, mine, theirs, a.Name, stack)
	}
	_, err = fmt.Fprintf(c.OutOrStdout(), "%s's %s matches the fleet document\n", host, path)
	return err
}

// unitLines are a unit file's lines without comments and blank lines,
// which systemd does not read, so two units that differ only in them are
// the same unit.
func unitLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		out = append(out, t)
	}
	return out
}

// firstDifference is the first line, counted from 1, where a and b differ,
// with each side's line there ("" past its end); same is true when they do
// not differ.
func firstDifference(a, b []string) (line int, x, y string, same bool) {
	for i := range max(len(a), len(b)) {
		var la, lb string
		if i < len(a) {
			la = a[i]
		}
		if i < len(b) {
			lb = b[i]
		}
		if la != lb {
			return i + 1, la, lb, false
		}
	}
	return 0, "", "", true
}
