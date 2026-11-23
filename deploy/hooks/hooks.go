// Package hooks runs the operator's hook scripts around a deploy: an
// executable named for the hook in the hooks directory, run on the
// operator's machine with the deploy's facts in its environment and never a
// secret value. A hook that does not exist is skipped.
//
// A failing before- hook stops what it precedes. An after- hook runs once
// the change is done, so its failure is reported without undoing it. The
// failed hook runs after a deploy, rollback or packaging fails.
package hooks

import (
	"context"
	"errors"
	"fmt"
	oscfs "github.com/grokify/oscompat/fs"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

// Hook names.
const (
	BeforePackage  = "before-package"
	AfterPackage   = "after-package"
	BeforeDeploy   = "before-deploy"
	AfterDeploy    = "after-deploy"
	BeforeRollback = "before-rollback"
	AfterRollback  = "after-rollback"
	Failed         = "failed"
)

// Names lists every hook, in the order a deploy can run them.
func Names() []string {
	return []string{BeforePackage, AfterPackage, BeforeDeploy, AfterDeploy, BeforeRollback, AfterRollback, Failed}
}

// DefaultDir is where hooks are kept, relative to the operator's repository.
const DefaultDir = ".deployforge/hooks"

// Facts are what a hook is told, as DEPLOYFORGE_* environment variables.
type Facts struct {
	App, Stack, Host, Operator string
	// Release and Revision are the release a deploy makes or a rollback
	// returns to, once known; Tag the app's release tag.
	Release, Revision, Tag string
	// Error is why a deploy failed, for the failed hook.
	Error string
}

func (f Facts) env(hook string) []string {
	return []string{
		"DEPLOYFORGE_HOOK=" + hook,
		"DEPLOYFORGE_APP=" + f.App,
		"DEPLOYFORGE_STACK=" + f.Stack,
		"DEPLOYFORGE_HOST=" + f.Host,
		"DEPLOYFORGE_OPERATOR=" + f.Operator,
		"DEPLOYFORGE_RELEASE=" + f.Release,
		"DEPLOYFORGE_REVISION=" + f.Revision,
		"DEPLOYFORGE_TAG=" + f.Tag,
		"DEPLOYFORGE_ERROR=" + f.Error,
	}
}

// Runner runs hooks from a directory.
type Runner struct {
	// Dir holds the hooks; a missing directory has none.
	Dir string
	// Out receives the hooks' output.
	Out io.Writer
}

// Run runs the hook if it exists. It must be an executable file.
func (r *Runner) Run(ctx context.Context, hook string, f Facts) error {
	if r == nil || r.Dir == "" {
		return nil
	}
	if !slices.Contains(Names(), hook) {
		return fmt.Errorf("hooks: unknown hook %q", hook)
	}
	path := filepath.Join(r.Dir, hook)
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hooks: %s: %w", hook, err)
	}
	if !st.Mode().IsRegular() || !oscfs.IsExecutableInfo(st, path) {
		return fmt.Errorf("hooks: %s is not an executable file; make it executable or remove it", path)
	}
	out := r.Out
	if out == nil {
		out = io.Discard
	}
	cmd := exec.CommandContext(ctx, path) //nolint:gosec // G204: the operator's own hook, from the hooks directory
	cmd.Env = append(os.Environ(), f.env(hook)...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hooks: %s failed: %w", hook, err)
	}
	return nil
}
