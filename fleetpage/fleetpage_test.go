package fleetpage_test

import (
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/fleetpage"
)

func TestInventory(t *testing.T) {
	f, err := fleet.Load("../fleet/testdata/fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Apps[0].Labels = map[string]string{"owner": "example-org", "data": "a|b"}
	page := fleetpage.Inventory(f, "fleet.json", "deployforge fleet inventory")
	for _, want := range []string{
		"# Fleet Inventory",
		"Generated from `fleet.json` by `deployforge fleet inventory`",
		"| `example-app` | prod | `web-1` | `app.example.com`, `auth.example.com` |",
		"| `example-app` | staging |",
		"`data`: a\\|b; `owner`: example-org",
		"| `example-app` | prod | `example-app` | `exampleapp` |",
		"| `example-app` | staging | `example-app-staging` | `exampleappstaging` |",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the inventory lacks %q:\n%s", want, page)
		}
	}
}

func TestFormat(t *testing.T) {
	page, err := fleetpage.Format("deployforge fleet format")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Fleet Document Format",
		"**Format version:** `" + fleet.FormatVersion + "`",
		"\n## Fleet\n",
		"\n## Host\n",
		"| `lightsailName` | string | no |",
		"| `apps` | array of [App](#app) | yes |",
		"| `labels` | map of string | no |",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the format page lacks %q", want)
		}
	}
}
