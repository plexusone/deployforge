package deployrunbook_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/deployrunbook"
	"github.com/plexusone/deployforge/fleet"
)

func load(t *testing.T) *fleet.Fleet {
	t.Helper()
	f, err := fleet.Load("../fleet/testdata/fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestBuild(t *testing.T) {
	f := load(t)
	book, err := deployrunbook.Build(f, "example-app", "prod", deployrunbook.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !book.Recurring || book.ID != "deploy-example-app-prod" {
		t.Errorf("runbook %s, recurring %v", book.ID, book.Recurring)
	}
	steps := book.Steps()
	var ids []string
	for _, s := range steps {
		ids = append(ids, s.ID)
	}
	// Without a credential registry, there are no credential steps.
	if strings.Join(ids, ",") != "1.1,1.2,2.1,2.2" {
		t.Errorf("steps %v", ids)
	}
	ship := strings.Join(steps[2].Actions[0].Exec[0].Args, " ")
	if ship != "deployforge deploy ship example-app prod {{source}}" || len(steps[2].Checks) != 2 {
		t.Errorf("ship %q, checks %d", ship, len(steps[2].Checks))
	}

	f.Credentials = &fleet.Credentials{Registry: "creds.json"}
	book, err = deployrunbook.Build(f, "example-app", "prod", deployrunbook.Options{Tool: []string{"tool", "--fleet", "fleet.json"}})
	if err != nil {
		t.Fatal(err)
	}
	steps = book.Steps()
	if steps[1].Title != "The stack's secrets" || strings.Join(steps[1].Actions[0].Exec[0].Args, " ") != "tool --fleet fleet.json credentials preflight example-app prod --record" {
		t.Errorf("secrets step %+v", steps[1])
	}
	if got := strings.Join(steps[4].Actions[0].Exec[0].Args, " "); got != "tool --fleet fleet.json credentials record example-app/prod/* deployed --by deploy" {
		t.Errorf("record %q", got)
	}
	data, err := deployrunbook.Export(book)
	if err != nil || !json.Valid(data) || !strings.Contains(string(data), `"$schema"`) {
		t.Errorf("export: %v", err)
	}
}

func TestBuildRefusesAStackNotLive(t *testing.T) {
	if _, err := deployrunbook.Build(load(t), "example-app", "staging", deployrunbook.Options{}); err == nil {
		t.Error("a planned stack has a deploy runbook")
	}
	if _, err := deployrunbook.Build(load(t), "nowhere", "prod", deployrunbook.Options{}); err == nil {
		t.Error("an unknown app has a deploy runbook")
	}
	if got := deployrunbook.EventsPath("", "example-app", "prod"); got != "deployments/runbooks/example-app-prod.events.json" {
		t.Errorf("EventsPath %s", got)
	}
}
