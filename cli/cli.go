// Package cli is deployforge's command tree. New returns the root command; a
// host tool can mount it under its own command and add commands beside it.
package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/dbroles"
	"github.com/plexusone/deployforge/dbsnapshot"
	"github.com/plexusone/deployforge/deploy/deployer"
	"github.com/plexusone/deployforge/deploy/hooks"
	"github.com/plexusone/deployforge/deploy/hostaddr"
	"github.com/plexusone/deployforge/deploy/onhost"
	"github.com/plexusone/deployforge/deploy/secrets"
	"github.com/plexusone/deployforge/deploy/secrets/awsssm"
	"github.com/plexusone/deployforge/deploy/systemd"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/ledger"
	"github.com/plexusone/deployforge/provenance"
	"github.com/plexusone/deployforge/release"
	"github.com/plexusone/deployforge/schema"
)

// Options configure the command tree.
type Options struct {
	// Use is the root command's name; deployforge when empty.
	Use string
	// Fleet is the fleet document's default path; fleet.json when empty.
	Fleet string
	// Store opens the secret store a release reads; AWS SSM Parameter
	// Store with the default AWS configuration when nil.
	Store func(c *cobra.Command) (secrets.Store, error)
	// Dial reaches hosts with an SSH key; the system's ssh and scp when nil.
	Dial func(key string) deployer.Dialer
	// Params opens the parameter store database provisioning reads and
	// writes; AWS SSM Parameter Store when nil.
	Params func(c *cobra.Command) (dbroles.Store, error)
	// Lightsail opens the client database snapshots use; Lightsail with the
	// default AWS configuration when nil.
	Lightsail func(c *cobra.Command) (dbsnapshot.API, error)
}

// lightsailOpener is the option's opener, or Lightsail with the default
// AWS configuration.
func lightsailOpener(o Options) func(c *cobra.Command) (dbsnapshot.API, error) {
	if o.Lightsail != nil {
		return o.Lightsail
	}
	return func(c *cobra.Command) (dbsnapshot.API, error) { return lightsailAPI(c.Context()) }
}

// Environment variables the commands read.
const (
	EnvSSHKey     = "DEPLOYFORGE_SSH_KEY"
	EnvOperator   = "DEPLOYFORGE_OPERATOR"
	EnvHostBinary = "DEPLOYFORGE_HOST_BINARY"
)

// New returns the root command.
func New(o Options) *cobra.Command {
	fleetPath := cmp.Or(o.Fleet, "fleet.json")
	root := &cobra.Command{
		Use:           cmp.Or(o.Use, "deployforge"),
		Short:         "Ship Go apps to your own servers: release archives, systemd, Caddy",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&fleetPath, "fleet", fleetPath, "the fleet document")
	load := func() (*fleet.Fleet, error) { return fleet.Load(fleetPath) }
	root.AddCommand(
		validateCmd(load),
		schemaCmd(),
		nextPortCmd(load),
		unitCmd(load),
		hostCmd(load, dialer(o)),
		caddyfileCmd(load),
		caddyCmd(load, dialer(o)),
		dbCmd(load, o),
		releaseCmd(load),
		deployCmd(load, &fleetPath, o),
		ledgerCmd(load),
		onHostCmd(),
	)
	return root
}

// dialer is the option's dialer, or the system's ssh and scp, reaching a
// host at its resolved address: read from its source, such as a Pulumi
// output, when the fleet document gives none.
func dialer(o Options) func(key string) deployer.Dialer {
	base := o.Dial
	if base == nil {
		base = deployer.SSHDialer
	}
	return func(key string) deployer.Dialer {
		dial := base(key)
		return func(h fleet.Host) (deployer.Transport, error) {
			resolved, err := hostaddr.Resolve(context.Background(), h, nil)
			if err != nil {
				return nil, err
			}
			return dial(resolved)
		}
	}
}

func validateCmd(load func() (*fleet.Fleet, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the fleet document, reporting every problem",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			stacks := 0
			for _, a := range f.Apps {
				stacks += len(a.Stacks)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%d hosts, %d apps and %d stacks OK\n", len(f.Hosts), len(f.Apps), stacks)
			return err
		},
	}
}

func schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the fleet document's JSON Schema",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			_, err := c.OutOrStdout().Write(schema.Fleet())
			return err
		},
	}
}

func nextPortCmd(load func() (*fleet.Fleet, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "next-port <host>",
		Short: "Print the lowest port in the range no stack on the host uses",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			if _, ok := f.Host(args[0]); !ok {
				return fmt.Errorf("no host %q", args[0])
			}
			p, err := f.NextPort(args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(c.OutOrStdout(), p)
			return err
		},
	}
}

func unitCmd(load func() (*fleet.Fleet, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "unit <app> <stack>",
		Short: "Print an app stack's systemd unit",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			unit, err := systemd.Render(f, args[0], args[1])
			if err != nil {
				return err
			}
			_, err = c.OutOrStdout().Write(unit)
			return err
		},
	}
}

func releaseCmd(load func() (*fleet.Fleet, error)) *cobra.Command {
	root := &cobra.Command{
		Use:   "release",
		Short: "Package an app's build output as a release archive",
	}
	var o release.Options
	var inputs []string
	pkg := &cobra.Command{
		Use:   "package <app>",
		Short: "Check a clean, tagged checkout and its build output, and write the archive and its checksum",
		Long: "package checks that the app's checkout is clean, is the repository the fleet document names, has no\n" +
			"replace directives, and is at a release tag; that the build output holds a static linux/amd64 binary named\n" +
			"for the app; and then writes a reproducible archive and its .sha256 file to --out.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			o.App = args[0]
			if o.Inputs, err = provenance.ParseAll(inputs); err != nil {
				return err
			}
			res, err := release.Package(f, o)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if res.Existing {
				if _, err := fmt.Fprintln(out, "already packaged: the same archive was there, and is kept"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(out, "archive:  %s\nsha256:   %s\nrevision: %s\ntag:      %s\n",
				res.Archive, res.SHA256, res.Revision, cmp.Or(res.Tag, "(untagged)")); err != nil {
				return err
			}
			for _, in := range res.Inputs {
				if _, err := fmt.Fprintf(out, "input:    %s\n", in); err != nil {
					return err
				}
			}
			return nil
		},
	}
	fl := pkg.Flags()
	fl.StringVar(&o.Repo, "repo", "", "the app's checkout the release is built from")
	fl.StringVar(&o.From, "from", "", "the directory the app's build wrote: the binary named for the app, and its web files")
	fl.StringVar(&o.Out, "out", "dist", "directory to write the archive and its checksum to")
	fl.BoolVar(&o.AllowUntagged, "allow-untagged", false, "package a revision with no release tag, for a trial or a local build")
	fl.StringArrayVar(&inputs, "input", nil, "an upstream repo@commit the build took from a local commit; repeat for each")
	mustRequire(pkg, "repo", "from")
	root.AddCommand(pkg)
	return root
}

func deployCmd(load func() (*fleet.Fleet, error), fleetPath *string, o Options) *cobra.Command {
	var ledgerDir, hostBinary, sshKey, operator, hooksDir string
	var healthTimeout, keep int
	root := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy a release to an app stack, roll it back, or unlock it",
		Long: `Deploy a release to one app stack, roll it back, or clear a stale deploy lock,
from the operator's machine.

Hosts are reached as <sshUser>@<address> from the fleet document, with the
system's ssh and scp, so the operator's SSH configuration, agent and known
hosts apply.

Secrets come from the stack's secret prefix: <prefix><VAR> becomes
/etc/<unit>/env, <prefix>migrate/<VAR> reaches the migrate step only, and
<prefix>files/<name> becomes /etc/<unit>/<name>. They travel to the host on
ssh's standard input, never on a command line or the operator's disk.

Environment:
  ` + EnvSSHKey + `        the hosts' SSH private key, or --ssh-key
  ` + EnvOperator + `      who is deploying (default $USER)
  ` + EnvHostBinary + `   deployforge's linux/amd64 build, or --host-binary
  AWS_PROFILE, AWS_REGION    the AWS profile and region, when secrets are in SSM`,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&ledgerDir, "ledger-dir", "deployments", "directory of the ledger files")
	pf.StringVar(&sshKey, "ssh-key", os.Getenv(EnvSSHKey), "the hosts' SSH private key, used through the agent SSH_AUTH_SOCK names")
	pf.StringVar(&hostBinary, "host-binary", cmp.Or(os.Getenv(EnvHostBinary), "bin/deployforge-linux-amd64"), "deployforge's linux/amd64 build, which runs on the host")
	pf.StringVar(&operator, "operator", cmp.Or(os.Getenv(EnvOperator), os.Getenv("USER")), "who is deploying, for the lock, the audit log and the ledger")
	pf.IntVar(&healthTimeout, "health-timeout", 60, "seconds to wait for every host name to answer /readyz after a restart")
	pf.IntVar(&keep, "keep", 5, "releases kept on the host, at least 2")
	pf.StringVar(&hooksDir, "hooks", hooks.DefaultDir, "directory of hook scripts run around a deploy: "+strings.Join(hooks.Names(), ", "))

	newDeployer := func(c *cobra.Command) (*deployer.Deployer, error) {
		f, err := load()
		if err != nil {
			return nil, err
		}
		return &deployer.Deployer{
			Fleet: f, LedgerDir: ledgerDir, Dial: dialer(o)(sshKey), HostBinary: hostBinary,
			Operator: operator, HealthTimeout: healthTimeout, Keep: keep, Log: c.ErrOrStderr(),
			Hooks: &hooks.Runner{Dir: hooksDir, Out: c.ErrOrStderr()},
		}, nil
	}
	openStore := o.Store
	if openStore == nil {
		openStore = func(c *cobra.Command) (secrets.Store, error) { return awsssm.New(c.Context()) }
	}

	var ro deployer.ReleaseOptions
	rel := &cobra.Command{
		Use:   "release <app> <stack>",
		Short: "Check a release archive, ship it to the stack, and record it in the ledger",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			d, err := newDeployer(c)
			if err != nil {
				return err
			}
			if d.Store, err = openStore(c); err != nil {
				return err
			}
			ro.App, ro.Stack = args[0], args[1]
			e, err := d.Release(c.Context(), ro)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%s/%s now runs %s\n", e.App, e.Stack, e.Release)
			return err
		},
	}
	fl := rel.Flags()
	fl.StringVar(&ro.Archive, "archive", "", "the release archive (deployforge release package)")
	fl.StringVar(&ro.Config, "config", "", "the stack's config.yaml")
	fl.StringVar(&ro.SHA256, "sha256", "", "the archive's checksum; default the first field of <archive>.sha256")
	fl.StringVar(&ro.Tag, "tag", "", "the app repository's release tag, for the ledger")
	fl.StringVar(&ro.Selfcheck, "selfcheck", "report", "enforce, report or off: run the app's self-check before and after the switch, if the fleet document turns it on")
	mustRequire(rel, "archive", "config")
	root.AddCommand(rel)

	root.AddCommand(&cobra.Command{
		Use:   "rollback <app> <stack>",
		Short: "Switch the stack back to the release before the current one, and record it",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			d, err := newDeployer(c)
			if err != nil {
				return err
			}
			e, err := d.Rollback(c.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%s/%s now runs %s\n", e.App, e.Stack, e.Release)
			return err
		},
	})
	root.AddCommand(shipCmd(load, fleetPath, newDeployer, openStore, lightsailOpener(o)))
	root.AddCommand(&cobra.Command{
		Use:   "unlock <app> <stack>",
		Short: "Remove the stack's deploy lock, left by a deploy that was cut off",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			d, err := newDeployer(c)
			if err != nil {
				return err
			}
			if err := d.Unlock(c.Context(), args[0], args[1]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%s/%s unlocked\n", args[0], args[1])
			return err
		},
	})
	return root
}

func ledgerCmd(load func() (*fleet.Fleet, error)) *cobra.Command {
	var dir string
	root := &cobra.Command{
		Use:   "ledger",
		Short: "Record and read the deploy ledger: what release each stack runs, since when",
	}
	root.PersistentFlags().StringVar(&dir, "dir", "deployments", "directory of the ledger files")

	var e ledger.Entry
	var at, action, selfcheck string
	var inputs []string
	record := &cobra.Command{
		Use:   "record",
		Short: "Append a deploy or rollback to a stack's ledger, after checking it against the history",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			f, err := load()
			if err != nil {
				return err
			}
			e.Action = ledger.Action(action)
			if e.Inputs, err = provenance.ParseAll(inputs); err != nil {
				return err
			}
			if selfcheck != "" {
				parts := strings.SplitN(selfcheck, ":", 4)
				if len(parts) != 4 {
					return fmt.Errorf("--selfcheck must be mode:pre:post:failed,..., got %q", selfcheck)
				}
				e.Selfcheck = &ledger.Selfcheck{Mode: parts[0], Pre: parts[1], Post: parts[2]}
				if parts[3] != "" {
					e.Selfcheck.Failed = strings.Split(parts[3], ",")
				}
			}
			e.Time = time.Now().UTC()
			if at != "" {
				if e.Time, err = time.Parse(time.RFC3339, at); err != nil {
					return fmt.Errorf("--time must be RFC 3339, such as 2026-10-23T18:00:00Z: %w", err)
				}
			}
			if err := ledger.Record(f, dir, e); err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "recorded %s of %s on %s/%s\n", e.Action, e.Release, e.App, e.Stack)
			return err
		},
	}
	fl := record.Flags()
	fl.StringVar(&e.App, "app", "", "the app's name")
	fl.StringVar(&e.Stack, "stack", "", "the stack's name")
	fl.StringVar(&action, "action", "deploy", "deploy or rollback")
	fl.StringVar(&e.Release, "release", "", "release directory now live, <UTC timestamp>-<revision>")
	fl.StringVar(&e.Revision, "revision", "", "the app repository's commit the release was built from")
	fl.StringVar(&e.Tag, "tag", "", "the app repository's release tag")
	fl.StringVar(&e.SHA256, "sha256", "", "release archive checksum (required for a deploy)")
	fl.StringVar(&e.Previous, "previous", "", "release that was live before; empty for a first deploy")
	fl.StringVar(&e.Operator, "operator", cmp.Or(os.Getenv(EnvOperator), os.Getenv("USER")), "who ran the deploy")
	fl.StringVar(&at, "time", "", "when it went live, RFC 3339; default now")
	fl.StringArrayVar(&inputs, "input", nil, "an upstream repo@commit a local build used; repeat for each")
	fl.StringVar(&selfcheck, "selfcheck", "", "the self-check's outcome, mode:pre:post:failed,...")
	mustRequire(record, "app", "stack", "release", "revision")
	root.AddCommand(record)

	var n int
	show := &cobra.Command{
		Use:   "show <app> <stack>",
		Short: "Print a stack's most recent ledger entries",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			entries, err := stackEntries(load, dir, args[0], args[1])
			if err != nil {
				return err
			}
			if n > 0 && len(entries) > n {
				entries = entries[len(entries)-n:]
			}
			for _, e := range entries {
				if _, err := fmt.Fprintf(c.OutOrStdout(), "%s  %-8s %s  rev %s  tag %s  by %s\n",
					e.Time.Format(time.RFC3339), e.Action, e.Release, e.Revision, cmp.Or(e.Tag, "-"), e.Operator); err != nil {
					return err
				}
			}
			return nil
		},
	}
	show.Flags().IntVarP(&n, "last", "n", 10, "how many entries; 0 for all")
	root.AddCommand(show)

	root.AddCommand(&cobra.Command{
		Use:   "current <app> <stack>",
		Short: "Print the release a stack is running; fails if it was never deployed",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			entries, err := stackEntries(load, dir, args[0], args[1])
			if err != nil {
				return err
			}
			cur, ok := ledger.Current(entries)
			if !ok {
				return fmt.Errorf("%s/%s has never been deployed", args[0], args[1])
			}
			_, err = fmt.Fprintln(c.OutOrStdout(), cur.Release)
			return err
		},
	})
	return root
}

func stackEntries(load func() (*fleet.Fleet, error), dir, app, stack string) ([]ledger.Entry, error) {
	f, err := load()
	if err != nil {
		return nil, err
	}
	a, _, err := f.Stack(app, stack)
	if err != nil {
		return nil, err
	}
	return ledger.Read(ledger.Path(dir, f, a, stack))
}

// workDir is what mktemp -d makes on the host.
var workDir = regexp.MustCompile(`^/tmp/tmp\.[A-Za-z0-9]+$`)

// onHostCmd is the root side of a deploy: the operator's deployforge runs it
// on the host, as root, with the request on standard input.
func onHostCmd() *cobra.Command {
	var work string
	c := &cobra.Command{
		Use:    "on-host",
		Short:  "Carry out a deploy request on the host, as root (run by deployforge deploy)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) (err error) {
			if !workDir.MatchString(work) {
				return fmt.Errorf("on-host: --work %q is not a directory mktemp -d made", work)
			}
			if os.Geteuid() != 0 {
				return errors.New("on-host: must run as root")
			}
			// The upload, including the archive, goes once the request is done.
			defer func() {
				if rmErr := os.RemoveAll(work); rmErr != nil {
					err = errors.Join(err, rmErr)
				}
			}()
			dec := json.NewDecoder(c.InOrStdin())
			dec.DisallowUnknownFields()
			var req onhost.Request
			if err := dec.Decode(&req); err != nil {
				return fmt.Errorf("on-host: reading the request: %w", err)
			}
			h := &onhost.Host{Sys: onhost.Systemd{}, Log: c.ErrOrStderr()}
			res, err := h.Run(c.Context(), req, work)
			if err != nil {
				return err
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(res)
		},
	}
	c.Flags().StringVar(&work, "work", "", "the directory the operator uploaded the archive to")
	return c
}

// mustRequire marks flags required; the flags are defined beside the call,
// so a failure is a programming error.
func mustRequire(c *cobra.Command, names ...string) {
	for _, n := range names {
		if err := c.MarkFlagRequired(n); err != nil {
			panic(err)
		}
	}
}
