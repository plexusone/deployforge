// Package deployer is the operator's half of a deploy: it checks the
// release, reads the stack's secrets into memory, uploads the archive and
// deployforge's own linux/amd64 build to the stack's host, runs that build as
// root with the request on standard input (package onhost), and records the
// result in the ledger.
//
// Nothing is sent until every local check passes, and secrets never appear
// on a command line or on the operator's disk.
package deployer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/plexusone/deployforge/deploy/archive"
	"github.com/plexusone/deployforge/deploy/onhost"
	"github.com/plexusone/deployforge/deploy/secrets"
	"github.com/plexusone/deployforge/deploy/systemd"
	"github.com/plexusone/deployforge/deploy/target"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/ledger"
	"github.com/plexusone/deployforge/release"
)

// Transport reaches a host.
type Transport interface {
	// TempDir creates a private directory on the host and returns its path.
	TempDir(ctx context.Context) (string, error)
	// Upload copies a local file to a path on the host.
	Upload(ctx context.Context, local, remote string) error
	// Sudo runs argv as root on the host, with stdin.
	Sudo(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// Dialer returns a Transport to one of the fleet's hosts.
type Dialer func(h fleet.Host) (Transport, error)

// Deployer deploys and rolls back app stacks.
type Deployer struct {
	Fleet *fleet.Fleet
	// LedgerDir holds the ledger files.
	LedgerDir string
	// Store reads secrets; a release needs it, a rollback does not.
	Store secrets.Store
	// Dial reaches the host a stack runs on.
	Dial Dialer
	// HostBinary is deployforge's own linux/amd64 build, which runs on the
	// host.
	HostBinary string
	// Operator is who is deploying, for the ledger.
	Operator string
	// HealthTimeout is how many seconds the host waits for /readyz.
	HealthTimeout int
	// Keep is how many releases stay on the host.
	Keep int
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Log receives progress, the host's included.
	Log io.Writer
}

// ReleaseOptions say what to deploy where.
type ReleaseOptions struct {
	App, Stack string
	// Archive is the release archive; Config the stack's config.yaml.
	Archive, Config string
	// SHA256 is the archive's checksum; empty to read <Archive>.sha256.
	SHA256 string
	// Tag is the app repository's release tag, for the ledger.
	Tag string
	// Selfcheck is enforce, report or off; an app runs it only when the
	// fleet document turns it on. Empty is off.
	Selfcheck string
}

// NotRecordedError means the change is live on the host but the ledger
// refused or failed to record it. Command records it by hand.
type NotRecordedError struct {
	Action  ledger.Action
	Command string
	Err     error
}

func (e *NotRecordedError) Error() string {
	return fmt.Sprintf("THE %s IS LIVE BUT NOT RECORDED (%v). Record it by hand, then commit the ledger:\n  %s",
		strings.ToUpper(string(e.Action)), e.Err, e.Command)
}

func (e *NotRecordedError) Unwrap() error { return e.Err }

var safeWord = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

func (d *Deployer) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d *Deployer) logf(format string, a ...any) {
	if d.Log != nil {
		_, _ = fmt.Fprintf(d.Log, "deploy: "+format+"\n", a...) // progress only; a lost line changes nothing
	}
}

func (d *Deployer) check() error {
	if d.Fleet == nil || d.Dial == nil {
		return errors.New("deployer: a fleet and a dialer are required")
	}
	if d.HealthTimeout < 1 || d.HealthTimeout > 600 {
		return fmt.Errorf("deployer: the health timeout %d must be 1 to 600 seconds", d.HealthTimeout)
	}
	if d.Keep < 2 {
		return fmt.Errorf("deployer: releases to keep, %d, must be at least 2", d.Keep)
	}
	if !onhost.ValidOperator(d.Operator) {
		return fmt.Errorf("deployer: say who is deploying, as 1 to 64 letters, digits and . _ @ -, not %q", d.Operator)
	}
	if err := release.CheckLinuxBinary(d.HostBinary); err != nil {
		return fmt.Errorf("deployer: the host runs %s: %w (build deployforge for linux/amd64)", d.HostBinary, err)
	}
	return nil
}

func (d *Deployer) live(t target.Target) (string, error) {
	a, ok := d.Fleet.App(t.App)
	if !ok {
		return "", fmt.Errorf("deployer: no app %q", t.App)
	}
	entries, err := ledger.Read(ledger.Path(d.LedgerDir, d.Fleet, a, t.Stack))
	if err != nil {
		return "", err
	}
	cur, _ := ledger.Current(entries) // an empty ledger has no live release
	return cur.Release, nil
}

// dial reaches the host the target runs on.
func (d *Deployer) dial(t target.Target) (Transport, error) {
	h, ok := d.Fleet.Host(t.Host)
	if !ok {
		return nil, fmt.Errorf("deployer: no host %q", t.Host)
	}
	tr, err := d.Dial(h)
	if err != nil {
		return nil, fmt.Errorf("deployer: reaching %s: %w", h.Name, err)
	}
	return tr, nil
}

// Release checks a release, ships it to the stack and records it.
func (d *Deployer) Release(ctx context.Context, o ReleaseOptions) (ledger.Entry, error) {
	if err := d.check(); err != nil {
		return ledger.Entry{}, err
	}
	if d.Store == nil {
		return ledger.Entry{}, errors.New("deployer: a release needs a secret store")
	}
	t, err := target.Resolve(d.Fleet, o.App, o.Stack)
	if err != nil {
		return ledger.Entry{}, err
	}
	config, err := os.ReadFile(o.Config)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("deployer: give --config, an existing configuration file: %w", err)
	}
	if len(bytes.TrimSpace(config)) == 0 {
		return ledger.Entry{}, fmt.Errorf("deployer: %s is empty", o.Config)
	}
	checked, err := archive.Check(o.Archive, t.App, o.SHA256)
	if err != nil {
		return ledger.Entry{}, err
	}
	previous, err := d.live(t)
	if err != nil {
		return ledger.Entry{}, err
	}
	id := d.now().Format("20060102T150405Z") + "-" + checked.Revision
	d.logf("%s/%s on %s: %s (previous: %s)", t.App, t.Stack, t.Host, id, orNone(previous))

	d.logf("reading secrets from %s", t.SecretPrefix)
	bundle, err := secrets.Load(ctx, d.Store, t.SecretPrefix)
	if err != nil {
		return ledger.Entry{}, err
	}
	d.logf("%d variables, %d migrate variables and %d files", len(bundle.Env), len(bundle.Migrate), len(bundle.Files))

	req := onhost.Request{
		Action: onhost.ActionRelease, Target: t, Operator: d.Operator, ReleaseID: id, Config: config, Secrets: bundle,
		HealthTimeout: d.HealthTimeout, Keep: d.Keep,
	}
	if req.Selfcheck, err = d.selfcheck(o); err != nil {
		return ledger.Entry{}, err
	}
	res, err := d.run(ctx, t, req, o.Archive)
	if err != nil {
		return ledger.Entry{}, err
	}
	if res.Release != id {
		return ledger.Entry{}, fmt.Errorf("deployer: the host reports %q live, not %q", res.Release, id)
	}
	if res.Previous != previous {
		d.logf("note: the host replaced %s, the ledger says %s was live", orNone(res.Previous), orNone(previous))
	}
	e := ledger.Entry{
		Time: d.now(), App: t.App, Stack: t.Stack, Action: ledger.Deploy, Release: id,
		Revision: checked.Revision, Tag: o.Tag, SHA256: checked.SHA256, Previous: previous, Operator: d.Operator,
		Inputs: checked.Inputs,
	}
	if sc := res.Selfcheck; sc != nil {
		e.Selfcheck = &ledger.Selfcheck{Mode: sc.Mode, Pre: sc.Pre, Post: sc.Post, Failed: sc.Failed}
		d.logf("self-check (%s): pre %s, post %s", sc.Mode, sc.Pre, orNone(sc.Post))
	}
	return e, d.record(e)
}

// selfcheck builds the request's self-check: none when the mode is off or
// the app has not turned it on, else the mode and the service's sandbox.
func (d *Deployer) selfcheck(o ReleaseOptions) (*onhost.SelfcheckRequest, error) {
	switch o.Selfcheck {
	case "", "off":
		return nil, nil
	case onhost.SelfcheckEnforce, onhost.SelfcheckReport:
	default:
		return nil, fmt.Errorf("deployer: self-check %q must be enforce, report or off", o.Selfcheck)
	}
	a, ok := d.Fleet.App(o.App)
	if !ok || !a.Selfcheck {
		return nil, nil
	}
	props, err := systemd.Sandbox(d.Fleet, o.App, o.Stack)
	if err != nil {
		return nil, err
	}
	return &onhost.SelfcheckRequest{Mode: o.Selfcheck, Properties: props}, nil
}

// Rollback switches a stack back to its previous release and records it.
func (d *Deployer) Rollback(ctx context.Context, app, stack string) (ledger.Entry, error) {
	if err := d.check(); err != nil {
		return ledger.Entry{}, err
	}
	t, err := target.Resolve(d.Fleet, app, stack)
	if err != nil {
		return ledger.Entry{}, err
	}
	req := onhost.Request{Action: onhost.ActionRollback, Target: t, Operator: d.Operator, HealthTimeout: d.HealthTimeout, Keep: d.Keep}
	res, err := d.run(ctx, t, req, "")
	if err != nil {
		return ledger.Entry{}, err
	}
	if res.Release == "" {
		return ledger.Entry{}, errors.New("deployer: the rollback did not report the release now live")
	}
	_, rev, _ := strings.Cut(res.Release, "-") // a release ID is <time>-<revision>
	e := ledger.Entry{
		Time: d.now(), App: t.App, Stack: t.Stack, Action: ledger.Rollback, Release: res.Release,
		Revision: rev, Previous: res.Previous, Operator: d.Operator,
	}
	return e, d.record(e)
}

// Unlock removes a stack's stale deploy lock, left by a deploy that was cut
// off. It changes nothing else and records nothing in the ledger; the host's
// audit log records it.
func (d *Deployer) Unlock(ctx context.Context, app, stack string) error {
	if err := d.check(); err != nil {
		return err
	}
	t, err := target.Resolve(d.Fleet, app, stack)
	if err != nil {
		return err
	}
	if _, err := d.run(ctx, t, onhost.Request{Action: onhost.ActionUnlock, Target: t, Operator: d.Operator}, ""); err != nil {
		return err
	}
	d.logf("unlocked %s/%s", t.App, t.Stack)
	return nil
}

// run uploads deployforge's linux build (and the archive) to the target's
// host, runs it as root with the request on standard input, and reads its
// result from standard output.
func (d *Deployer) run(ctx context.Context, t target.Target, req onhost.Request, archivePath string) (onhost.Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return onhost.Result{}, fmt.Errorf("deployer: %w", err)
	}
	tr, err := d.dial(t)
	if err != nil {
		return onhost.Result{}, err
	}
	work, err := tr.TempDir(ctx)
	if err != nil {
		return onhost.Result{}, fmt.Errorf("deployer: temporary directory on the host: %w", err)
	}
	if !safeWord.MatchString(work) || !strings.HasPrefix(work, "/") {
		return onhost.Result{}, fmt.Errorf("deployer: unexpected temporary directory on the host: %q", work)
	}
	bin := work + "/deployforge"
	if err := tr.Upload(ctx, d.HostBinary, bin); err != nil {
		return onhost.Result{}, fmt.Errorf("deployer: uploading deployforge: %w", err)
	}
	if archivePath != "" {
		d.logf("uploading %s", archivePath)
		if err := tr.Upload(ctx, archivePath, work+"/release.tgz"); err != nil {
			return onhost.Result{}, fmt.Errorf("deployer: uploading the archive: %w", err)
		}
	}
	var out bytes.Buffer
	err = tr.Sudo(ctx, []string{bin, "on-host", "--work", work}, bytes.NewReader(body), &out, d.Log)
	if err != nil {
		return onhost.Result{}, fmt.Errorf("deployer: the %s on the host failed: %w", req.Action, err)
	}
	var res onhost.Result
	dec := json.NewDecoder(&out)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return onhost.Result{}, fmt.Errorf("deployer: the host's result: %w", err)
	}
	return res, nil
}

func (d *Deployer) record(e ledger.Entry) error {
	if err := ledger.Record(d.Fleet, d.LedgerDir, e); err != nil {
		return &NotRecordedError{Action: e.Action, Command: recordCommand(d.LedgerDir, e), Err: err}
	}
	d.logf("recorded the %s of %s on %s/%s", e.Action, e.Release, e.App, e.Stack)
	return nil
}

func recordCommand(dir string, e ledger.Entry) string {
	args := []string{"deployforge", "ledger", "--dir", dir, "record", "--app", e.App, "--stack", e.Stack,
		"--action", string(e.Action), "--release", e.Release, "--revision", e.Revision,
		"--time", e.Time.Format(time.RFC3339), "--operator", e.Operator}
	if e.Previous != "" {
		args = append(args, "--previous", e.Previous)
	}
	if e.SHA256 != "" {
		args = append(args, "--sha256", e.SHA256)
	}
	if e.Tag != "" {
		args = append(args, "--tag", e.Tag)
	}
	if sc := e.Selfcheck; sc != nil {
		args = append(args, "--selfcheck", sc.Mode+":"+sc.Pre+":"+sc.Post+":"+strings.Join(sc.Failed, ","))
	}
	for _, in := range e.Inputs {
		args = append(args, "--input", in.String())
	}
	return strings.Join(args, " ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
