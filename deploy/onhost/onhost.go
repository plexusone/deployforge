// Package onhost is the root-side half of a deploy: what runs on the host,
// as root, for one release or rollback of one stack. The operator's
// deployforge uploads its own linux/amd64 build and runs it once per deploy
// with a Request on standard input.
//
// A release unpacks the archive as /opt/<unit>/releases/<id>, installs the
// configuration, the environment file and the key files under /etc/<unit>,
// runs the new binary's validate and migrate as the stack's user, switches
// /opt/<unit>/current, restarts the unit and waits for /readyz. On any failure
// it restores the previous files and release. A rollback switches back to the
// newest release older than the current one.
//
// Everything outside the process goes through System, and every path is under
// Host.Root, so the steps are unit-tested in a temporary directory.
package onhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/plexusone/deployforge/deploy/archive"
	"github.com/plexusone/deployforge/deploy/secrets"
	"github.com/plexusone/deployforge/deploy/target"
)

// Actions a Request asks for.
const (
	ActionRelease  = "release"
	ActionRollback = "rollback"
	// ActionUnlock removes a stale deploy lock and changes nothing else.
	ActionUnlock = "unlock"
)

// Request is what the operator's side sends, as JSON on standard input.
type Request struct {
	Action string        `json:"action"`
	Target target.Target `json:"target"`
	// Operator is who is deploying, for the lock and the audit log.
	Operator string `json:"operator"`
	// ReleaseID names the new release directory: <UTC timestamp>-<revision>.
	ReleaseID string `json:"releaseId,omitempty"`
	// Config is the stack's config.yaml.
	Config []byte `json:"config,omitempty"`
	// Secrets are the stack's environment, migrate variables and key files.
	Secrets secrets.Bundle `json:"secrets"`
	// HealthTimeout is how many seconds to wait for /readyz after a restart.
	HealthTimeout int `json:"healthTimeout"`
	// Keep is how many releases stay on the host, at least 2.
	Keep int `json:"keep"`
	// Selfcheck runs the new release's selfcheck before and after the
	// switch; nil skips it.
	Selfcheck *SelfcheckRequest `json:"selfcheck,omitempty"`
}

// Self-check modes.
const (
	// SelfcheckEnforce stops a deploy on a failed pre stage and rolls one
	// back on a failed post stage.
	SelfcheckEnforce = "enforce"
	// SelfcheckReport runs both stages and reports them, changing nothing.
	SelfcheckReport = "report"
)

// SelfcheckRequest says how to run the self-check.
type SelfcheckRequest struct {
	Mode string `json:"mode"`
	// Properties are the service's sandbox as systemd-run properties
	// (systemd.Sandbox), so the check runs as the service would.
	Properties []string `json:"properties"`
}

// SelfcheckResult is each stage's outcome, pass or fail, and the names of
// the checks that failed, never their details.
type SelfcheckResult struct {
	Mode   string   `json:"mode"`
	Pre    string   `json:"pre"`
	Post   string   `json:"post,omitempty"`
	Failed []string `json:"failed,omitempty"`
}

// Result is what the host reports back, as JSON on standard output.
type Result struct {
	// Release is the release now live.
	Release string `json:"release"`
	// Previous is the release that was live before, empty for a first deploy.
	Previous string `json:"previous,omitempty"`
	// Selfcheck is the self-check's outcome, when it ran.
	Selfcheck *SelfcheckResult `json:"selfcheck,omitempty"`
	// Status is what a status request found.
	Status *Status `json:"status,omitempty"`
	// Failure is how a failed release or rollback failed; the request
	// fails too.
	Failure *Failure `json:"failure,omitempty"`
}

var (
	releaseID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{7,40}$`)
	property  = regexp.MustCompile(`^[A-Z][A-Za-z]+=[^\x00-\x1f]*$`)
	// operatorName is who is deploying, written to the lock and the audit log.
	operatorName = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
)

// ValidOperator reports whether name can be recorded as who is deploying:
// 1 to 64 letters, digits and . _ @ -.
func ValidOperator(name string) bool { return operatorName.MatchString(name) }

// Validate checks a request before anything on the host changes.
func (r Request) Validate() error {
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if !operatorName.MatchString(r.Operator) {
		return fmt.Errorf("onhost: operator %q must be 1 to 64 letters, digits and . _ @ -", r.Operator)
	}
	if r.Action == ActionUnlock || r.Action == ActionStatus {
		return nil
	}
	if r.HealthTimeout < 1 || r.HealthTimeout > 600 {
		return fmt.Errorf("onhost: health timeout %d must be 1 to 600 seconds", r.HealthTimeout)
	}
	if r.Keep < 2 {
		return fmt.Errorf("onhost: keep %d must be at least 2", r.Keep)
	}
	switch r.Action {
	case ActionRelease:
		if !releaseID.MatchString(r.ReleaseID) {
			return fmt.Errorf("onhost: release id %q must be <UTC timestamp>-<revision>", r.ReleaseID)
		}
		if len(r.Config) == 0 {
			return errors.New("onhost: a release needs the stack's configuration")
		}
		if len(r.Secrets.Env) == 0 {
			return errors.New("onhost: a release needs the stack's environment")
		}
		if sc := r.Selfcheck; sc != nil {
			if sc.Mode != SelfcheckEnforce && sc.Mode != SelfcheckReport {
				return fmt.Errorf("onhost: self-check mode %q must be %s or %s", sc.Mode, SelfcheckEnforce, SelfcheckReport)
			}
			if len(sc.Properties) == 0 || len(sc.Properties) > 64 {
				return fmt.Errorf("onhost: the self-check needs 1 to 64 sandbox properties, got %d", len(sc.Properties))
			}
			for _, p := range sc.Properties {
				if !property.MatchString(p) {
					return fmt.Errorf("onhost: %q is not a systemd property", p)
				}
			}
		}
	case ActionRollback:
		if r.Selfcheck != nil {
			return errors.New("onhost: a rollback runs no self-check")
		}
	default:
		return fmt.Errorf("onhost: action %q must be %s, %s, %s or %s", r.Action, ActionRelease, ActionRollback, ActionUnlock, ActionStatus)
	}
	return nil
}

// System is everything a deploy does outside its own files.
type System interface {
	// DaemonReload reloads systemd's units.
	DaemonReload(ctx context.Context) error
	// Restart restarts a unit.
	Restart(ctx context.Context, unit string) error
	// Stop stops a unit.
	Stop(ctx context.Context, unit string) error
	// RunAs runs argv as user, with the given environment files, the way systemd
	// parses them for the unit.
	RunAs(ctx context.Context, user string, envFiles, argv []string, stdout, stderr io.Writer) error
	// RunSandboxed is RunAs inside the given systemd properties, with a hard
	// limit of maxSeconds. It returns the process's exit status; err is for
	// a process that could not be run or waited for.
	RunSandboxed(ctx context.Context, user string, envFiles, props []string, maxSeconds int, argv []string, stdout, stderr io.Writer) (status int, err error)
	// Journal returns a unit's last lines of log, for the operator.
	Journal(ctx context.Context, unit string, lines int) string
	// Ready reports whether one probe of http://<listen>/readyz with Host: host
	// answered 2xx.
	Ready(ctx context.Context, listen, host string) bool
	// Own sets a path's owner to root and its group to group.
	Own(path, group string) error
}

// Host runs requests on one host.
type Host struct {
	// Root prefixes every path; empty on the host.
	Root string
	Sys  System
	// Log receives progress for the operator.
	Log io.Writer
	// Sleep waits between health probes; time.Sleep when nil.
	Sleep func(time.Duration)
	// Now is the clock for the lock and the audit log; time.Now when nil.
	Now func() time.Time
}

func (h *Host) logf(format string, a ...any) {
	if h.Log != nil {
		_, _ = fmt.Fprintf(h.Log, "deploy: "+format+"\n", a...) // progress only; a lost line changes nothing
	}
}

// path is an absolute path on the host, under Root when it is set.
// filepath.Join drops an empty first element, so an empty Root is "/", or
// every path would be relative to the deploy's working directory.
func (h *Host) path(parts ...string) string {
	root := h.Root
	if root == "" {
		root = string(filepath.Separator)
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// layout is one stack's paths.
type layout struct {
	base, releases, current, etc, managed string
}

func (h *Host) layout(t target.Target) layout {
	base := h.path("opt", t.Unit)
	etc := h.path("etc", t.Unit)
	return layout{
		base:     base,
		releases: filepath.Join(base, "releases"),
		current:  filepath.Join(base, "current"),
		etc:      etc,
		managed:  filepath.Join(etc, ".managed"),
	}
}

// Run carries out a request. work is the directory the operator uploaded to;
// a release's archive is work/release.tgz.
//
// A release or rollback holds the stack's deploy lock while it runs, and
// every request, an unlock included, is appended to the stack's audit log.
func (h *Host) Run(ctx context.Context, r Request, work string) (res Result, err error) {
	if err := r.Validate(); err != nil {
		return Result{}, err
	}
	l := h.layout(r.Target)
	if r.Action == ActionStatus {
		// A status changes nothing, so it is not audited.
		st, err := h.status(l)
		return Result{Release: statusLive(st), Status: st}, err
	}
	defer func() {
		// A request refused for a held lock changed nothing, but who tried is
		// worth knowing too.
		if aerr := h.audit(l, r, res, err); aerr != nil {
			// The change itself is done or reported; a lost audit line must
			// not turn a live release into a reported failure.
			h.logf("warning: %v", aerr)
		}
	}()
	if r.Action == ActionUnlock {
		return Result{}, h.unlock(l)
	}
	release, err := h.lock(l, r)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if uerr := release(); uerr != nil {
			// The release or rollback is done or reported; the stale lock
			// only blocks the next deploy, which says how to unlock it.
			h.logf("warning: %v; unlock the stack before the next deploy", uerr)
		}
	}()
	if r.Action == ActionRollback {
		return h.rollback(ctx, r)
	}
	return h.release(ctx, r, work)
}

func (h *Host) release(ctx context.Context, r Request, work string) (Result, error) {
	t, l := r.Target, h.layout(r.Target)
	rel := filepath.Join(l.releases, r.ReleaseID)
	if _, err := os.Lstat(rel); err == nil {
		return Result{}, fmt.Errorf("onhost: %s already exists", rel)
	}
	if err := archive.Extract(filepath.Join(work, "release.tgz"), rel); err != nil {
		_ = os.RemoveAll(rel) // a partial release must not stay; the extract error is the one to report
		return Result{}, failed(StepUnpack, Failure{Kept: true}, err)
	}
	bin := filepath.Join(rel, t.App)
	if st, err := os.Stat(bin); err != nil || !st.Mode().IsRegular() || st.Mode()&0o100 == 0 {
		_ = os.RemoveAll(rel) // the missing binary is the error to report
		return Result{}, failed(StepUnpack, Failure{Kept: true}, fmt.Errorf("onhost: %s is not an executable file", bin))
	}

	newNames := make([]string, 0, len(r.Secrets.Files))
	for _, f := range r.Secrets.Files {
		newNames = append(newNames, f.Name)
	}
	oldNames, err := readManaged(l.managed)
	if err != nil {
		_ = os.RemoveAll(rel) // nothing else has changed yet
		return Result{}, failed(StepInstall, Failure{Kept: true}, err)
	}
	etc := &etcFiles{h: h, dir: l.etc, group: t.User, old: oldNames, names: union([]string{"config.yaml", "env", ".managed"}, newNames, oldNames)}
	if err := etc.save(); err != nil {
		_ = os.RemoveAll(rel) // nothing was installed
		return Result{}, failed(StepInstall, Failure{Kept: true}, err)
	}
	// abort undoes a release that never switched: the running release and
	// its files stay; a migration that completed stays applied.
	migrated := false
	abort := func(step string, cause error) (Result, error) {
		h.logf("%v; keeping the running release", cause)
		if err := etc.restore(); err != nil {
			cause = errors.Join(cause, err)
		}
		if err := os.RemoveAll(rel); err != nil {
			cause = errors.Join(cause, err)
		}
		return Result{}, failed(step, Failure{Kept: true, Migrated: migrated}, cause)
	}

	if err := etc.install(r.Config, secrets.EnvFile(r.Secrets.Env), r.Secrets.Files); err != nil {
		return abort(StepInstall, err)
	}

	cfg := h.path("etc", t.Unit, "config.yaml")
	envFile := h.path("etc", t.Unit, "env")
	h.logf("validating %s", r.ReleaseID)
	if err := h.Sys.RunAs(ctx, t.User, []string{envFile}, []string{bin, "validate", "-c", cfg}, h.Log, h.Log); err != nil {
		return abort(StepValidate, fmt.Errorf("validate failed: %w", err))
	}
	var check *SelfcheckResult
	if sc := r.Selfcheck; sc != nil {
		check = &SelfcheckResult{Mode: sc.Mode}
		var failed []string
		check.Pre, failed = h.selfcheck(ctx, r, bin, cfg, envFile, "pre")
		check.Failed = append(check.Failed, failed...)
		if check.Pre != stagePass && sc.Mode == SelfcheckEnforce {
			return abort(StepSelfcheckPre, fmt.Errorf("the self-check's pre stage failed: %s", strings.Join(failed, ", ")))
		}
	}
	// Admin DSNs and role passwords exist on the host only for this step.
	migrateEnv := filepath.Join(work, "migrate.env")
	if err := os.WriteFile(migrateEnv, secrets.EnvFile(r.Secrets.Migrate), 0o600); err != nil {
		return abort(StepMigrate, err)
	}
	h.logf("migrating databases with %s", r.ReleaseID)
	err = h.Sys.RunAs(ctx, t.User, []string{envFile, migrateEnv}, []string{bin, "migrate", "-c", cfg}, h.Log, h.Log)
	if rmErr := os.Remove(migrateEnv); rmErr != nil {
		err = errors.Join(err, rmErr)
	}
	if err != nil {
		return abort(StepMigrate, fmt.Errorf("migrate failed (completed steps stay applied): %w", err))
	}
	migrated = true

	prev, err := liveRelease(l.current)
	if err != nil {
		return abort(StepSwitch, err)
	}
	if err := switchTo(l.current, rel); err != nil {
		return abort(StepSwitch, err)
	}
	unit := t.Unit + ".service"
	if err := h.Sys.DaemonReload(ctx); err != nil {
		h.logf("daemon-reload: %v", err)
	}
	restartErr := h.Sys.Restart(ctx, unit)
	var failure error
	failedStep := StepHealth
	if restartErr != nil || !h.healthy(ctx, r) {
		h.logf("%s failed its health check; rolling back", r.ReleaseID)
		h.logf("journal:\n%s", h.Sys.Journal(ctx, unit, 60))
		failure = fmt.Errorf("onhost: %s failed its health check", r.ReleaseID)
		if restartErr != nil {
			failure = errors.Join(failure, restartErr)
		}
	} else if check != nil {
		var failed []string
		check.Post, failed = h.selfcheck(ctx, r, bin, cfg, envFile, "post")
		check.Failed = append(check.Failed, failed...)
		if check.Post != stagePass && check.Mode == SelfcheckEnforce {
			failedStep = StepSelfcheckPost
			h.logf("%s failed the self-check's post stage; rolling back", r.ReleaseID)
			failure = fmt.Errorf("onhost: %s failed the self-check's post stage: %s", r.ReleaseID, strings.Join(failed, ", "))
		}
	}
	if failure != nil {
		f := Failure{Migrated: true}
		restored := true
		if err := etc.restore(); err != nil {
			failure, restored = errors.Join(failure, err), false
		}
		if prev != "" {
			if err := switchTo(l.current, prev); err != nil {
				return Result{}, failed(failedStep, f, errors.Join(failure, err))
			}
			if err := h.Sys.Restart(ctx, unit); err != nil {
				failure, restored = errors.Join(failure, err), false
			}
			h.logf("restored %s", filepath.Base(prev))
			f.Restored = restored
		} else {
			f.Stopped = true
			if err := os.Remove(l.current); err != nil {
				failure = errors.Join(failure, err)
			}
			if err := h.Sys.Stop(ctx, unit); err != nil {
				failure = errors.Join(failure, err)
			}
		}
		return Result{}, failed(failedStep, f, failure)
	}
	if err := etc.drop(); err != nil {
		h.logf("removing the saved files: %v", err)
	}
	if err := h.prune(l.releases, r.Keep, rel, prev); err != nil {
		h.logf("pruning old releases: %v", err)
	}
	h.logf("%s healthy", r.ReleaseID)
	res := Result{Release: r.ReleaseID, Selfcheck: check}
	if prev != "" {
		res.Previous = filepath.Base(prev)
	}
	return res, nil
}

func (h *Host) rollback(ctx context.Context, r Request) (Result, error) {
	l := h.layout(r.Target)
	cur, err := liveRelease(l.current)
	if err != nil {
		return Result{}, err
	}
	if cur == "" {
		return Result{}, errors.New("onhost: no current release")
	}
	names, err := releaseNames(l.releases)
	if err != nil {
		return Result{}, err
	}
	prev := ""
	for _, n := range names {
		if n < filepath.Base(cur) {
			prev = n
		}
	}
	if prev == "" {
		return Result{}, fmt.Errorf("onhost: no release older than %s", filepath.Base(cur))
	}
	h.logf("rolling back %s -> %s", filepath.Base(cur), prev)
	if err := switchTo(l.current, filepath.Join(l.releases, prev)); err != nil {
		return Result{}, failed(StepSwitch, Failure{Kept: true}, err)
	}
	unit := r.Target.Unit + ".service"
	restartErr := h.Sys.Restart(ctx, unit)
	if restartErr == nil && h.healthy(ctx, r) {
		h.logf("%s healthy", prev)
		return Result{Release: prev, Previous: filepath.Base(cur)}, nil
	}
	h.logf("%s failed its health check; restoring %s", prev, filepath.Base(cur))
	h.logf("journal:\n%s", h.Sys.Journal(ctx, unit, 40))
	failure := fmt.Errorf("onhost: %s failed its health check", prev)
	if restartErr != nil {
		failure = errors.Join(failure, restartErr)
	}
	if err := switchTo(l.current, cur); err != nil {
		return Result{}, failed(StepHealth, Failure{}, errors.Join(failure, err))
	}
	f := Failure{Restored: true}
	if err := h.Sys.Restart(ctx, unit); err != nil {
		failure, f.Restored = errors.Join(failure, err), false
	}
	return Result{}, failed(StepHealth, f, failure)
}

// healthy probes /readyz once a second, once per host name, until every host
// name answers or the timeout passes.
func (h *Host) healthy(ctx context.Context, r Request) bool {
	sleep := h.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for i := 0; i < r.HealthTimeout; i++ {
		ready := true
		for _, host := range r.Target.HealthHosts {
			ready = ready && h.Sys.Ready(ctx, r.Target.Listen, host)
		}
		if ready {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		sleep(time.Second)
	}
	return false
}

// prune removes the oldest releases beyond keep, never the live one or the one
// it replaced.
func (h *Host) prune(dir string, keep int, live, prev string) error {
	names, err := releaseNames(dir)
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var errs []error
	for i, n := range names {
		p := filepath.Join(dir, n)
		if i < keep || p == live || p == prev {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseNames lists the release directories, oldest first.
func releaseNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("onhost: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && releaseID.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// liveRelease is the release current points at, or "" when there is none.
func liveRelease(current string) (string, error) {
	dest, err := os.Readlink(current)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("onhost: %w", err)
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(current), dest)
	}
	if st, err := os.Stat(dest); err != nil || !st.IsDir() {
		return "", nil
	}
	return dest, nil
}

// switchTo points current at rel atomically: a new link, renamed over the old.
func switchTo(current, rel string) error {
	tmp := current + ".new"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("onhost: %w", err)
	}
	if err := os.Symlink(rel, tmp); err != nil {
		return fmt.Errorf("onhost: %w", err)
	}
	if err := os.Rename(tmp, current); err != nil {
		return fmt.Errorf("onhost: %w", err)
	}
	return nil
}

func readManaged(p string) ([]string, error) {
	data, err := os.ReadFile(p) //nolint:gosec // G304: a path under the stack's own configuration directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("onhost: %w", err)
	}
	return strings.Fields(string(data)), nil
}

func union(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, n := range l {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

func statusLive(st *Status) string {
	if st == nil {
		return ""
	}
	return st.Live
}
