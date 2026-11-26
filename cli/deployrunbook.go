package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	rbcli "github.com/plexusone/systemspec-runbook/cli"
	"github.com/plexusone/systemspec-runbook/runbookspec"
	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deployrunbook"
	"github.com/plexusone/deployforge/fleet"
)

// deployRunbookCmd mounts systemspec-runbook's commands (list, show,
// prepare, run, record, verify, restart, state, doc) over an app stack's
// deploy runbook, chosen with --app and --stack, and adds export. Each
// stack keeps its own event log under the runbook directory.
func deployRunbookCmd(load func() (*fleet.Fleet, error), fleetPath *string, o Options) *cobra.Command {
	var app, stack string
	dir := o.RunbookDir
	if dir == "" {
		dir = deployrunbook.DefaultDir
	}
	options := func() deployrunbook.Options {
		tool := o.RunbookTool
		if len(tool) == 0 {
			tool = []string{program(), "--fleet", *fleetPath}
		}
		runbook := strings.Join(append(append([]string{}, tool...), "deploy", "runbook", "--app", app, "--stack", stack), " ")
		return deployrunbook.Options{Tool: tool, RunbookCommand: runbook}
	}
	build := func() (*runbookspec.Runbook, error) {
		f, err := load()
		if err != nil {
			return nil, err
		}
		return deployrunbook.Build(f, app, stack, options())
	}
	cmd := rbcli.New(rbcli.Options{
		Use:   "runbook",
		Short: "List, show, prepare, run, record, verify and restart an app stack's deploy runbook, show its state, and write its page",
		Load:  build,
		Render: func() (runbookspec.RenderOptions, error) {
			return runbookspec.RenderOptions{
				Resolve:       func(key string) (string, error) { return "", fmt.Errorf("unknown link key %q", key) },
				VerifyCommand: options().RunbookCommand + " verify {step}",
				Generator:     program() + " deploy runbook doc",
			}, nil
		},
		Root: ".",
	})
	cmd.Long = "A live stack's deploy runbook is recurring: begin each deploy with restart --note <release>, then\n" +
		"run its steps, or its deploy phase with run 2. state shows every deploy's run: when it began, finished\n" +
		"or failed. Its event log is <app>-<stack>.events.json in " + dir + "."
	cmd.PersistentFlags().StringVar(&app, "app", "", "the app (required)")
	cmd.PersistentFlags().StringVar(&stack, "stack", "", "the stack (required)")
	cmd.PersistentPreRunE = func(c *cobra.Command, args []string) error {
		// cobra runs only the nearest hook, so run the one above this.
		for p := cmd.Parent(); p != nil; p = p.Parent() {
			if p.PersistentPreRunE != nil {
				if err := p.PersistentPreRunE(c, args); err != nil {
					return err
				}
				break
			}
		}
		if app == "" || stack == "" {
			return errors.New("deploy runbook: --app and --stack are required")
		}
		if f := c.Flag("events"); f != nil && !f.Changed {
			return f.Value.Set(deployrunbook.EventsPath(dir, app, stack))
		}
		return nil
	}
	var out string
	export := &cobra.Command{
		Use:   "export",
		Short: "Write the deploy runbook as JSON, which runbookspec reads",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			book, err := build()
			if err != nil {
				return err
			}
			data, err := deployrunbook.Export(book)
			if err != nil {
				return err
			}
			if out == "" {
				_, err = c.OutOrStdout().Write(data)
				return err
			}
			return os.WriteFile(out, data, 0o644) //nolint:gosec // G306: a runbook, committed and public to the repository
		},
	}
	export.Flags().StringVarP(&out, "output", "o", "", "file to write; standard output when empty")
	cmd.AddCommand(export)
	return cmd
}
