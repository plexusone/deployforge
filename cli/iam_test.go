package cli

import (
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
)

func TestIAMTargets(t *testing.T) {
	f, err := fleet.Load(testFleet)
	if err != nil {
		t.Fatal(err)
	}
	all, err := iamTargets(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].App != "example-app" || all[0].Stack != "prod" || all[0].Prefix != "/example/prod/example-app/" {
		t.Errorf("every live stack: %+v", all)
	}
	named, err := iamTargets(f, []string{"example-app", "staging"})
	if err != nil || len(named) != 1 || named[0].Prefix != "/example/staging/example-app/" {
		t.Errorf("a named stack: %+v, %v", named, err)
	}
	if _, err := iamTargets(f, []string{"example-app"}); err == nil {
		t.Error("an app without a stack")
	}
	if _, err := iamTargets(f, []string{"nowhere", "prod"}); err == nil {
		t.Error("an unknown app")
	}
}

func TestIAMNeedsARegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	if _, err := run(t, "iam", "plan"); err == nil || !strings.Contains(err.Error(), "no region") {
		t.Errorf("no region: %v", err)
	}
}
