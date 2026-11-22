package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/plexusone/credforge"
	forgecli "github.com/plexusone/credforge/cli"
	"github.com/plexusone/credforge/targets"
	credcli "github.com/plexusone/systemspec-credential/cli"
	"github.com/plexusone/systemspec-credential/credspec"
	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/credpage"
	"github.com/plexusone/deployforge/deploy/preflight"
	"github.com/plexusone/deployforge/fleet"
)

// credentialFiles are a fleet's credential files, as paths from the
// working directory.
type credentialFiles struct {
	registry, events, reviews, targets string
}

// filesOf resolves the fleet document's credential paths against its
// directory.
func filesOf(f *fleet.Fleet, fleetPath string) (credentialFiles, error) {
	c := f.Credentials
	if c == nil {
		return credentialFiles{}, errors.New("credentials: the fleet document has no credentials section naming its registry")
	}
	dir := filepath.Dir(fleetPath)
	at := func(p, beside string) string {
		if p == "" {
			return filepath.Join(filepath.Dir(filepath.Join(dir, c.Registry)), beside)
		}
		return filepath.Join(dir, p)
	}
	out := credentialFiles{
		registry: filepath.Join(dir, c.Registry),
		events:   at(c.Events, "events.json"),
		reviews:  at(c.Reviews, "reviews.json"),
	}
	if c.Targets != "" {
		out.targets = filepath.Join(dir, c.Targets)
	}
	return out, nil
}

// TargetsFor is how the fleet's credential stores are reached: the targets
// file the fleet document names, or GitHub with GITHUB_TOKEN or GH_TOKEN
// and SSM with the default AWS configuration. Every app's GitHub
// repository is scanned, so a workflow there reading a secret the registry
// does not store is reported.
func TargetsFor(f *fleet.Fleet, file string) (*targets.File, error) {
	var t *targets.File
	if file != "" {
		var err error
		if t, err = targets.Load(file); err != nil {
			return nil, err
		}
	} else {
		t = &targets.File{
			FormatVersion: targets.FormatVersion,
			Bindings: []targets.Binding{
				{Store: credspec.StoreGitHubActions, Account: "*", GitHub: &targets.GitHub{}},
				{Store: credspec.StoreAWSSSM, Account: "*", AWS: &targets.AWS{}},
			},
		}
	}
	for _, a := range f.Apps {
		if repo, ok := strings.CutPrefix(a.Repo, "github.com/"); ok && !slices.Contains(t.Scan.GitHubRepositories, repo) {
			t.Scan.GitHubRepositories = append(t.Scan.GitHubRepositories, repo)
		}
	}
	return t, t.Validate()
}

// credentialsCmd is systemspec-credential's commands over the fleet's
// registry (validate, status, record, report and more), with credforge's
// check, generate, install, review and setup beside them, and the page. The fleet document
// names the files; a flag given on the command line wins.
func credentialsCmd(load func() (*fleet.Fleet, error), fleetPath *string) *cobra.Command {
	files := func() (*fleet.Fleet, credentialFiles, error) {
		f, err := load()
		if err != nil {
			return nil, credentialFiles{}, err
		}
		cf, err := filesOf(f, *fleetPath)
		return f, cf, err
	}
	registry := func() (*credspec.Registry, error) {
		_, cf, err := files()
		if err != nil {
			return nil, err
		}
		return credspec.LoadRegistry(cf.registry)
	}
	cmd := credcli.New(credcli.Options{
		Use:      "credentials",
		Short:    "Check, report on, generate, install, set up and record the credentials the fleet's registry describes",
		Registry: registry,
	})
	// tool is how the setup runbook's commands run these commands: this
	// program, the commands down to credentials, and the fleet document.
	tool := func() []string {
		out := []string{program()}
		out = append(out, strings.Fields(cmd.CommandPath())[1:]...)
		return append(out, "--fleet", *fleetPath)
	}
	forge := forgecli.Options{
		Tool:     tool,
		Registry: registry,
		Targets: func() (*targets.File, error) {
			f, cf, err := files()
			if err != nil {
				return nil, err
			}
			return TargetsFor(f, cf.targets)
		},
	}
	cmd.AddCommand(forgecli.Check(forge), forgecli.Generate(forge), forgecli.Install(forge), forgecli.Review(forge), forgecli.Setup(forge),
		credentialsPageCmd(files, tool))
	// The flags' defaults come from the fleet document, which is read once
	// the command line, with --fleet, is parsed.
	cmd.PersistentPreRunE = func(c *cobra.Command, _ []string) error {
		_, cf, err := files()
		if err != nil {
			return err
		}
		for _, d := range []struct{ name, value string }{{"events", cf.events}, {"reviews", cf.reviews}} {
			name, v := d.name, d.value
			fl := c.Flags().Lookup(name)
			if fl == nil || fl.Changed {
				continue
			}
			if err := fl.Value.Set(v); err != nil {
				return fmt.Errorf("credentials: --%s: %w", name, err)
			}
		}
		return nil
	}
	return cmd
}

// preflightFor checks a stack's secrets against the fleet's credential
// registry before a deploy, naming the missing ones. A parameter no binding
// reaches is reported and does not stop it.
func preflightFor(o Options, fleetPath *string) func(c *cobra.Command, f *fleet.Fleet, a fleet.App, stack string) error {
	connect := o.CredentialConnector
	if connect == nil {
		connect = func(t *targets.File) credforge.Connector { return forgecli.NewConnector(t) }
	}
	return func(c *cobra.Command, f *fleet.Fleet, a fleet.App, stack string) error {
		cf, err := filesOf(f, *fleetPath)
		if err != nil {
			return err
		}
		reg, err := credspec.LoadRegistry(cf.registry)
		if err != nil {
			return err
		}
		log, err := credspec.LoadLog(cf.events, reg)
		if err != nil {
			return err
		}
		tf, err := TargetsFor(f, cf.targets)
		if err != nil {
			return err
		}
		r, err := preflight.Check(c.Context(), &credforge.Checker{Connector: connect(tf)}, reg, log, a.SecretPrefixFor(stack))
		if err != nil {
			return err
		}
		if len(r.Unobserved) > 0 {
			if _, err := fmt.Fprintf(c.ErrOrStderr(), "preflight: no binding reaches %s, so they were not checked\n", strings.Join(r.Unobserved, ", ")); err != nil {
				return err
			}
		}
		if err := r.Err(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(c.OutOrStdout(), "%d secret(s) in place\n", r.Checked)
		return err
	}
}

// credentialsPageCmd writes an app stack's credentials page.
func credentialsPageCmd(files func() (*fleet.Fleet, credentialFiles, error), tool func() []string) *cobra.Command {
	var out, needs string
	cmd := &cobra.Command{
		Use:   "page <app> <stack>",
		Short: "Write an HTML page of what the app stack's credentials are, how each is made and how to set them up",
		Long: "page lists every credential the stack needs, how each is made, where it is stored and whether the\n" +
			"event log records it, then the runbook for setting them up, as setup builds it, so its step numbers\n" +
			"are setup's. It holds no value.",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			f, cf, err := files()
			if err != nil {
				return err
			}
			reg, err := credspec.LoadRegistry(cf.registry)
			if err != nil {
				return err
			}
			log, err := credspec.LoadLog(cf.events, reg)
			if err != nil {
				return err
			}
			var ns []credspec.Need
			for _, n := range strings.Split(needs, ",") {
				if n = strings.TrimSpace(n); n != "" {
					ns = append(ns, credspec.Need(n))
				}
			}
			page, err := credpage.Render(f, args[0], args[1], reg, log, credpage.Options{Needs: ns, Tool: tool()})
			if err != nil {
				return err
			}
			if out == "" {
				_, err = c.OutOrStdout().Write(page)
				return err
			}
			if err := os.WriteFile(out, page, 0o644); err != nil { //nolint:gosec // G306: the page is meant to be read and published
				return fmt.Errorf("credentials page: %w", err)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "wrote %s\n", out)
			return err
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "file to write; standard output when empty")
	cmd.Flags().StringVar(&needs, "need", "launch,later,optional", "the needs to show, comma-separated")
	return cmd
}

// program is how this program is run: its name when that name on PATH is
// this program, and otherwise the path it was run as.
func program() string {
	self := os.Args[0]
	base := filepath.Base(self)
	found, err := exec.LookPath(base)
	if err != nil {
		return self
	}
	a, errA := filepath.EvalSymlinks(found)
	b, errB := filepath.EvalSymlinks(self)
	if errA == nil && errB == nil && a == b {
		return base
	}
	return self
}
