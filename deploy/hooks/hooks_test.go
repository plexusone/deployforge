package hooks

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grokify/oscompat/testutil"
)

func write(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	testutil.RequirePOSIX(t, "the hooks are shell scripts run as programs")
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestRunPassesTheFactsAndSkipsMissingHooks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, AfterDeploy, "#!/bin/sh\necho \"$DEPLOYFORGE_HOOK $DEPLOYFORGE_APP/$DEPLOYFORGE_STACK $DEPLOYFORGE_RELEASE by $DEPLOYFORGE_OPERATOR\"\n", 0o755)
	var out bytes.Buffer
	r := &Runner{Dir: dir, Out: &out}
	f := Facts{App: "one-cloud", Stack: "prod", Operator: "tester", Release: "20261023T180000Z-abc1234"}
	if err := r.Run(context.Background(), BeforeDeploy, f); err != nil {
		t.Errorf("a missing hook: %v", err)
	}
	if err := r.Run(context.Background(), AfterDeploy, f); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "after-deploy one-cloud/prod 20261023T180000Z-abc1234 by tester\n" {
		t.Errorf("output = %q", got)
	}
}

func TestRunReportsAFailingOrUnusableHook(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, BeforeDeploy, "#!/bin/sh\nexit 3\n", 0o755)
	write(t, dir, AfterDeploy, "#!/bin/sh\n", 0o644)
	r := &Runner{Dir: dir}
	if err := r.Run(context.Background(), BeforeDeploy, Facts{}); err == nil || !strings.Contains(err.Error(), "before-deploy failed") {
		t.Errorf("a failing hook: %v", err)
	}
	if err := r.Run(context.Background(), AfterDeploy, Facts{}); err == nil || !strings.Contains(err.Error(), "not an executable") {
		t.Errorf("a hook that is not executable: %v", err)
	}
	if err := r.Run(context.Background(), "before-everything", Facts{}); err == nil {
		t.Error("an unknown hook")
	}
	var none *Runner
	if err := none.Run(context.Background(), BeforeDeploy, Facts{}); err != nil {
		t.Errorf("no runner: %v", err)
	}
}
