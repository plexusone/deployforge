// Package deployrunbook builds an app stack's deploy runbook: a recurring
// runbook, one run per deploy, from the operator's shell to the recorded
// deploy. Its earlier runs stay in its event log, so the runbook's state
// says when the stack was last deployed and whether that deploy finished.
package deployrunbook

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	rb "github.com/plexusone/systemspec-runbook/runbookspec"
	"github.com/plexusone/systemspec-runbook/schema"

	"github.com/plexusone/deployforge/fleet"
)

// DefaultDir holds each stack's deploy runbook event log.
const DefaultDir = "deployments/runbooks"

// Options configure the runbook.
type Options struct {
	// Tool is how the runbook's commands run the tool, such as
	// []string{"deployforge"}, or a tool embedding it with its fleet flag;
	// "deployforge" when empty.
	Tool []string
	// RunbookCommand is how the page names this runbook's commands, such
	// as "deployforge deploy runbook example-app prod"; built from Tool
	// when empty.
	RunbookCommand string
	// SourceEnv is the environment variable holding what to deploy, such
	// as --tag=v1.2.3; DEPLOY_SOURCE when empty.
	SourceEnv string
	// CredentialPattern is the registry pattern of the stack's credentials,
	// recorded as deployed after a deploy; <app>/<stack>/* when empty.
	// Without a credential registry in the fleet document, the runbook has
	// no credential steps.
	CredentialPattern string
	// SetupHowTo is the operator's shell setup, as Markdown, such as which
	// file sets the AWS profile and the SSH key's path.
	SetupHowTo string
}

// EventsPath is the stack's deploy runbook event log under dir.
func EventsPath(dir, app, stack string) string {
	if dir == "" {
		dir = DefaultDir
	}
	return path.Join(dir, app+"-"+stack+".events.json")
}

// Build builds the stack's deploy runbook and validates it. Only a live
// stack has one.
func Build(f *fleet.Fleet, app, stack string, o Options) (*rb.Runbook, error) {
	a, s, err := f.Stack(app, stack)
	if err != nil {
		return nil, fmt.Errorf("deployrunbook: %w", err)
	}
	if s.Status != fleet.Live {
		return nil, fmt.Errorf("deployrunbook: %s's %s stack is %s; a deploy runbook is for a live stack", app, stack, s.Status)
	}
	book := build(a, s, stack, f.Credentials != nil, o)
	if err := book.Validate(rb.ValidateOptions{KnownCredential: func(string) bool { return false }, KnownNamedCheck: func(string) bool { return false }}); err != nil {
		return nil, err
	}
	return book, nil
}

// Export is the runbook as JSON, with its schema named for editors.
func Export(book *rb.Runbook) ([]byte, error) {
	data, err := json.Marshal(book)
	if err != nil {
		return nil, fmt.Errorf("deployrunbook: export: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("deployrunbook: export: %w", err)
	}
	doc["$schema"] = schema.RunbookID
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("deployrunbook: export: %w", err)
	}
	return append(out, '\n'), nil
}

func build(a fleet.App, s fleet.Stack, stack string, credentials bool, o Options) *rb.Runbook {
	tool := o.Tool
	if len(tool) == 0 {
		tool = []string{"deployforge"}
	}
	run := func(args ...string) []string { return append(slices.Clone(tool), args...) }
	sourceEnv := o.SourceEnv
	if sourceEnv == "" {
		sourceEnv = "DEPLOY_SOURCE"
	}
	runbook := o.RunbookCommand
	if runbook == "" {
		runbook = strings.Join(run("deploy", "runbook", a.Name, stack), " ")
	}
	pattern := o.CredentialPattern
	if pattern == "" {
		pattern = a.Name + "/" + stack + "/*"
	}
	ship := func(extra ...string) []string {
		return append(run("deploy", "ship", a.Name, stack, "{{source}}"), extra...)
	}
	var ready []rb.Check
	for i, h := range s.Hosts {
		ready = append(ready, rb.Check{ID: fmt.Sprintf("ready-%d", i+1), Description: h + " is ready from outside", Kind: rb.KindHTTP,
			HTTP: &rb.HTTPCheck{URL: "https://" + h + "/readyz", Status: 200}})
	}
	selfcheck := ""
	if a.Selfcheck {
		selfcheck = " It runs the app's self-check before and after the switch."
	}
	setup := o.SetupHowTo
	if setup == "" {
		setup = "set the AWS profile and region, and `DEPLOYFORGE_SSH_KEY` to the host's SSH key."
	}

	prepare := []rb.Step{{
		ID: "1.1", Title: "The operator's shell", Owner: rb.OwnerOperator,
		Summary: "A deploy reaches the host over SSH with its key, and reads the stack's secrets with the\n" +
			"operator's AWS profile. Neither is ever written to the repository.",
		Actions: []rb.Action{
			{Text: setup},
			{Text: "add the host's key to the SSH agent, with its passphrase: `ssh-add <key>`."},
		},
		Done: "the SSH agent holds the host's key and the AWS profile answers.",
		Checks: []rb.Check{
			{ID: "ssh-agent", Description: "the SSH agent holds a key", Kind: rb.KindCommand,
				Command: &rb.CommandCheck{Args: []string{"ssh-add", "-l"}}},
			{ID: "aws", Description: "the AWS profile answers", Kind: rb.KindCommand,
				Command: &rb.CommandCheck{Args: []string{"aws", "sts", "get-caller-identity", "--query", "Arn", "--output", "text"}, Contains: "arn:aws:"}},
		},
	}}
	if credentials {
		prepare = append(prepare, rb.Step{
			ID: "1.2", Title: "The stack's secrets", Owner: rb.OwnerAgent,
			Summary: fmt.Sprintf("Every secret the registry keeps under `%s` is in the parameter store, by name; a\n"+
				"value never leaves it. One present but never recorded is recorded as created, so the\n"+
				"credentials' state is right.", a.SecretPrefixFor(stack)),
			Actions: []rb.Action{
				{Text: "compare the stack's secret prefix with the registry, and record the unrecorded:",
					Exec: []rb.Exec{{Args: run("credentials", "preflight", a.Name, stack, "--record")}}},
			},
			Done: "no credential is missing.",
		})
	}
	prepare = append(prepare, rb.Step{
		ID: fmt.Sprintf("1.%d", len(prepare)+1), Title: "A dry run", Owner: rb.OwnerAgent,
		Actions: []rb.Action{
			{Text: "build and package the release; nothing is deployed:", Exec: []rb.Exec{{Args: ship("--dry-run")}}},
		},
		Done: "the release packages, and the dry run names its revision.",
	})
	var all []string
	for _, st := range prepare {
		all = append(all, st.ID)
	}

	record := []rb.Action{}
	if credentials {
		record = append(record, rb.Action{Text: "record that the stack's credentials are deployed; those not yet created are skipped:",
			Exec: []rb.Exec{{Args: run("credentials", "record", pattern, "deployed", "--by", "deploy")}}})
	}
	record = append(record, rb.Action{Text: fmt.Sprintf("commit the stack's ledger, `deployments/%s.jsonl`, with the event logs, and push.", a.Name)})

	return &rb.Runbook{
		ID:        "deploy-" + a.Name + "-" + stack,
		Title:     a.Name + " " + stack + " Deploy Runbook",
		Recurring: true,
		Summary: fmt.Sprintf("Deploying a release of `%[1]s` to its `%[2]s` stack (%[3]s), from the operator's shell\n"+
			"to the recorded deploy. Each deploy is one run of this runbook; earlier runs stay in its event\n"+
			"log, so `%[4]s state` says when the stack was last deployed and whether that deploy finished.",
			a.Name, stack, strings.Join(s.Hosts, ", "), runbook),
		Usage: fmt.Sprintf("**Begin each deploy after the first with a new run**, naming the release:\n"+
			"`%[1]s restart --note v1.2.3`. Every step is then ready again, and the deploy before stays in\n"+
			"the runbook's state.\n\n"+
			"**Set the release** in `%[2]s` for the shell that runs the steps: `--tag=v1.2.3` for a release\n"+
			"whose CI has passed, or `--local` for the pushed commit on the app's main branch.\n\n"+
			"**Run the deploy** with `%[1]s run 2`, or a step with `%[1]s run <step>`: each asks before every\n"+
			"command, records the step and runs its checks.", runbook, sourceEnv),
		Variables: []rb.Variable{{Name: "source", Env: sourceEnv, Example: "--tag=v1.2.3",
			Description: "what to deploy: --tag=<release tag>, or --local for the pushed commit on main"}},
		Phases: []rb.Phase{
			{ID: "1", Title: "Prepare", Gives: "the operator's access, the stack's secrets in place, and a release that packages", Steps: prepare},
			{ID: "2", Title: "Deploy", Gives: fmt.Sprintf("the release live on %s, and the deploy recorded", strings.Join(s.Hosts, ", ")), Steps: []rb.Step{
				{
					ID: "2.1", Title: "Ship it", Owner: rb.OwnerOperator, DependsOn: all,
					Summary: "`ship` snapshots the database server first when the fleet names one, deploys the archive\n" +
						"and checks every host name from outside." + selfcheck + " If the new release fails its health\n" +
						"check, the previous one is restored; a migration that completed stays applied, so a rollback\n" +
						"after it must work with the migrated schema.",
					Actions: []rb.Action{{Text: "deploy the release:", Exec: []rb.Exec{{Args: ship()}}}},
					Done:    "ship reports every host name ready.",
					Checks:  ready,
				},
				{
					ID: "2.2", Title: "Record the deploy", Owner: rb.OwnerAgent, DependsOn: []string{"2.1"},
					Actions: record,
					Done:    fmt.Sprintf("the ledger's latest entry for `%s` is this release, and the records are pushed.", stack),
					Checks:  ready,
				},
			}},
		},
	}
}
