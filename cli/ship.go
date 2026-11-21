package cli

import (
	"errors"
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deploy/deployer"
	"github.com/plexusone/deployforge/deploy/readiness"
	"github.com/plexusone/deployforge/deploy/secrets"
	"github.com/plexusone/deployforge/deploy/ship"
	"github.com/plexusone/deployforge/fleet"
)

// shipCmd is a whole routine deploy in one command: package the tag in a
// temporary worktree, deploy it, and check every host name from outside.
func shipCmd(load func() (*fleet.Fleet, error), fleetPath *string, newDeployer func(*cobra.Command) (*deployer.Deployer, error),
	openStore func(*cobra.Command) (secrets.Store, error)) *cobra.Command {
	var tag, repo, config, ref, script, selfcheck string
	var with, allowUnpushed []string
	var noFetch, local, skipPublic bool
	cmd := &cobra.Command{
		Use:   "ship <app> <stack> (--tag vX.Y.Z | --local [--with <repo>[@ref]]...)",
		Short: "Package the app in a temporary worktree, deploy it, and check each host name",
		Long: "ship is a routine deploy in one command. It packages the app with its release script in a\n" +
			"temporary git worktree, so the checkout is left as it is, deploys it as deploy release does with\n" +
			"the stack's config (default stacks/<app>/<stack>/config.yaml), and checks https://<host>/readyz for\n" +
			"each of the stack's host names from outside. Commit the ledger entry afterwards.\n\n" +
			"With --tag it fetches and packages the app's release tag, with GOWORK off. With --local it packages\n" +
			"the app's local --ref (default main) instead, untagged, and each --with repository from its own\n" +
			"checkout's local commit ($GOPATH/src/<repo>; main unless @ref is given) through a generated\n" +
			"go.work. The archive and the ledger name every upstream commit used. Every commit a local build\n" +
			"uses must be on a remote branch, except --allow-unpushed repositories.",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) (err error) {
			app, stack := args[0], args[1]
			f, err := load()
			if err != nil {
				return err
			}
			a, s, err := f.Stack(app, stack)
			if err != nil {
				return err
			}
			if s.Status != fleet.Live {
				return fmt.Errorf("ship: %s/%s is %q; set it live in the fleet document once its host is ready", app, stack, s.Status)
			}
			if local == (tag != "") {
				return errors.New("ship: give either --tag or --local")
			}
			if !local && len(with) > 0 {
				return errors.New("ship: --with needs --local")
			}
			if config == "" {
				config = filepath.Join("stacks", app, stack, "config.yaml")
			}
			if _, err := os.Stat(config); err != nil {
				return fmt.Errorf("ship: the stack's config: %w", err)
			}
			if repo == "" {
				repo = filepath.Join(gopathSrc(), a.Repo)
			}
			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("ship: finding deployforge's own path: %w", err)
			}
			fleetAbs, err := filepath.Abs(*fleetPath)
			if err != nil {
				return err
			}
			ups, err := upstreams(with)
			if err != nil {
				return err
			}
			allow := map[string]bool{}
			for _, r := range allowUnpushed {
				allow[r] = true
			}
			say := func(format string, args ...any) error {
				_, err := fmt.Fprintf(c.OutOrStdout(), format+"\n", args...)
				return err
			}
			if err := say("packaging %s in a temporary worktree", app); err != nil {
				return err
			}
			p, err := ship.Build(c.Context(), ship.Options{
				App: app, Repo: repo, Tag: tag, Deployforge: self, Fleet: fleetAbs, Script: script,
				Fetch: !noFetch, Local: local, Ref: ref, Upstreams: ups,
				RequirePushed: local, AllowUnpushed: allow, Log: c.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			defer func() {
				if cerr := p.Close(); cerr != nil {
					// Whatever happened to the deploy is reported beside it.
					err = errors.Join(err, fmt.Errorf("ship: cleaning up the temporary worktree: %w", cerr))
				}
			}()
			d, err := newDeployer(c)
			if err != nil {
				return err
			}
			if d.Store, err = openStore(c); err != nil {
				return err
			}
			e, err := d.Release(c.Context(), deployer.ReleaseOptions{
				App: app, Stack: stack, Archive: p.Archive, Config: config, Tag: tag, Selfcheck: selfcheck,
			})
			if err != nil {
				return err
			}
			if err := say("%s/%s now runs %s", app, stack, e.Release); err != nil {
				return err
			}
			if skipPublic {
				return nil
			}
			if err := say("checking %s from outside", strings.Join(s.Hosts, ", ")); err != nil {
				return err
			}
			if err := (readiness.Checker{}).Check(c.Context(), s.Hosts); err != nil {
				return fmt.Errorf("ship: deployed and recorded, but not ready from outside: %w", err)
			}
			return say("every host name is ready; commit the ledger entry")
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&tag, "tag", "", "the app's release tag to package")
	fl.BoolVar(&local, "local", false, "package a local commit, untagged, instead of a tag")
	fl.StringVar(&ref, "ref", "", "with --local, the app's branch or commit; default main")
	fl.StringArrayVar(&with, "with", nil, "with --local, an upstream module's repository, as host/org/repo[@ref], built from its local checkout; repeat for each")
	fl.StringArrayVar(&allowUnpushed, "allow-unpushed", nil, "an upstream repository a local build may take from an unpushed commit; repeat for each")
	fl.StringVar(&repo, "repo", "", "the app's checkout; default $GOPATH/src/<repo>")
	fl.StringVar(&config, "config", "", "the stack's config.yaml; default stacks/<app>/<stack>/config.yaml")
	fl.StringVar(&script, "script", "", "the release script, relative to the repository; default "+ship.ReleaseScript)
	fl.StringVar(&selfcheck, "selfcheck", "report", "enforce, report or off: run the app's self-check, if the fleet document turns it on")
	fl.BoolVar(&noFetch, "no-fetch", false, "do not fetch the tag from origin first")
	fl.BoolVar(&skipPublic, "skip-public-check", false, "do not check the host names from outside afterwards")
	return cmd
}

// gopathSrc is $GOPATH/src, where checkouts are found by repository path.
func gopathSrc() string {
	return filepath.Join(build.Default.GOPATH, "src")
}

// upstreams reads each host/org/repo[@ref] as a checkout under $GOPATH/src.
func upstreams(specs []string) ([]ship.Upstream, error) {
	var out []ship.Upstream
	for _, s := range specs {
		repo, ref, _ := strings.Cut(s, "@") // no @ means the default ref
		if strings.Count(repo, "/") != 2 {
			return nil, fmt.Errorf("ship: --with %q must be host/org/repo[@ref]", s)
		}
		out = append(out, ship.Upstream{Repo: repo, Dir: filepath.Join(gopathSrc(), repo), Ref: ref})
	}
	return out, nil
}
