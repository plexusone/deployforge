package deployer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grokify/oscompat/testutil"

	"github.com/plexusone/deployforge/deploy/archive"
	"github.com/plexusone/deployforge/deploy/hooks"
	"github.com/plexusone/deployforge/deploy/onhost"
	"github.com/plexusone/deployforge/deploy/secrets"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/ledger"
)

const fleetJSON = `{
  "formatVersion": "1",
  "portRange": {"min": 8101, "max": 8199},
  "hosts": [{"name": "web-1", "address": "203.0.113.10"}],
  "apps": [{
    "name": "one-cloud", "repo": "github.com/example/one-cloud", "domain": "one.example.com",
    "secretPrefix": "/example/{stack}/one-cloud/",
    "stacks": {
      "prod": {"host": "web-1", "hosts": ["app.one.example.com", "auth.one.example.com"], "port": 8101, "databases": [{"name": "one", "appRole": true}], "status": "planned"},
      "staging": {"host": "web-1", "hosts": ["stage-app.one.example.com"], "port": 8102, "status": "planned"}
    }
  }]
}`

// secretValue must never appear on a command line or in an upload.
const secretValue = "s3cr3t-value-do-not-leak"

// hostBinary is a linux/amd64 build standing in for deployforge's, made once
// for all the tests.
var hostBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "deployer-test")
	if err != nil {
		panic(err)
	}
	hostBinary = filepath.Join(dir, "deployforge-linux-amd64")
	cmd := exec.Command("go", "build", "-o", hostBinary, "./testdata/hostbin") //nolint:gosec // G204: builds a test program in this repository
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir) // the build error is the one to report
		panic("build the host binary for linux: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir) // a scratch directory
	os.Exit(code)
}

// store is a parameter store in memory.
type store map[string]string

func (s store) ByPath(_ context.Context, path string) ([]secrets.Param, error) {
	var out []secrets.Param
	for name, value := range s {
		if rest, ok := strings.CutPrefix(name, path); ok && !strings.Contains(rest, "/") {
			out = append(out, secrets.Param{Name: name, Value: value})
		}
	}
	return out, nil
}

// sys is the host's systemd, as onhost sees it.
type sys struct {
	root       string
	fail       bool
	unhealthy  map[string]bool
	selfchecks []string // "<stage> <number of sandbox properties>"
}

func (s *sys) live() string {
	dest, err := os.Readlink(filepath.Join(s.root, "opt", "one-cloud", "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(dest)
}
func (s *sys) DaemonReload(context.Context) error          { return nil }
func (s *sys) Restart(context.Context, string) error       { return nil }
func (s *sys) Stop(context.Context, string) error          { return nil }
func (s *sys) Journal(context.Context, string, int) string { return "" }
func (s *sys) Ready(context.Context, string, string) bool  { return !s.unhealthy[s.live()] }
func (s *sys) Own(string, string) error                    { return nil }
func (s *sys) RunSandboxed(_ context.Context, _ string, _, props []string, _ int, argv []string, stdout, _ io.Writer) (int, error) {
	s.selfchecks = append(s.selfchecks, fmt.Sprintf("%s %d", argv[len(argv)-1], len(props)))
	_, err := fmt.Fprintf(stdout, `{"app":"host","check":"zones","stage":%q,"status":"pass"}`+"\n", argv[len(argv)-1])
	return 0, err
}
func (s *sys) RunAs(_ context.Context, _ string, _, argv []string, _, _ io.Writer) error {
	if s.fail && argv[1] == "validate" {
		return errors.New("exit status 1")
	}
	return nil
}

// transport is the host: uploads land in local directories, and Sudo runs
// the real onhost code against a scratch root, as deployforge on-host would.
type transport struct {
	t       *testing.T
	sys     *sys
	calls   []string
	uploads []string
}

func (tr *transport) TempDir(context.Context) (string, error) {
	tr.calls = append(tr.calls, "mktemp")
	return tr.t.TempDir(), nil
}

func (tr *transport) Upload(_ context.Context, local, remote string) error {
	tr.calls = append(tr.calls, "upload "+filepath.Base(local)+" "+filepath.Base(remote))
	data, err := os.ReadFile(local) //nolint:gosec // G304: a path under the test's own temporary directory
	if err != nil {
		return err
	}
	tr.uploads = append(tr.uploads, string(data))
	return os.WriteFile(remote, data, 0o600) //nolint:gosec // G703: remote is a path under the test's own temporary directory
}

func (tr *transport) Sudo(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	tr.calls = append(tr.calls, "sudo "+strings.Join(argv, " "))
	if len(argv) != 4 || argv[1] != "on-host" || argv[2] != "--work" || argv[0] != argv[3]+"/deployforge" {
		return fmt.Errorf("unexpected command %v", argv)
	}
	dec := json.NewDecoder(stdin)
	dec.DisallowUnknownFields()
	var req onhost.Request
	if err := dec.Decode(&req); err != nil {
		return err
	}
	h := &onhost.Host{Root: tr.sys.root, Sys: tr.sys, Log: stderr, Sleep: func(time.Duration) {}}
	res, err := h.Run(ctx, req, argv[3])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err) // as deployforge on-host reports it
		return errors.New("exit status 1")
	}
	return json.NewEncoder(stdout).Encode(res)
}

type rig struct {
	t       *testing.T
	dir     string
	d       *Deployer
	tr      *transport
	store   store
	archive string
	sum     string
	config  string
	log     bytes.Buffer
	clock   time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	testutil.RequirePOSIX(t, "its fake host runs shell scripts as programs and answers with Linux paths")
	f, err := fleet.Parse([]byte(fleetJSON))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "host")
	for _, d := range []string{"opt/one-cloud/releases", "etc/one-cloud"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	r := &rig{t: t, dir: dir, clock: time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)}
	r.tr = &transport{t: t, sys: &sys{root: root, unhealthy: map[string]bool{}}}
	r.store = store{
		"/example/prod/one-cloud/SESSION_KEY":           secretValue,
		"/example/prod/one-cloud/migrate/ADMIN_DSN":     "postgres://owner@db/one",
		"/example/prod/one-cloud/files/signing-key.pem": "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n",
	}
	r.archive, r.sum = r.build("one-cloud abc1234def56\n", true)
	r.config = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(r.config, []byte("listen:\n  addr: 127.0.0.1:8101\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.d = &Deployer{
		Fleet: f, LedgerDir: filepath.Join(dir, "deployments"), Store: r.store, Dial: func(fleet.Host) (Transport, error) { return r.tr, nil },
		HostBinary: hostBinary, Operator: "tester", HealthTimeout: 3, Keep: 5,
		Now: func() time.Time { return r.clock }, Log: &r.log,
	}
	return r
}

func (r *rig) build(revision string, withBinary bool) (string, string) {
	r.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string, mode int64) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			r.t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			r.t.Fatal(err)
		}
	}
	add("REVISION", revision, 0o644)
	if withBinary {
		add("one-cloud", "#!/bin/sh\n", 0o755)
	}
	if err := tw.Close(); err != nil {
		r.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		r.t.Fatal(err)
	}
	p := filepath.Join(r.t.TempDir(), "one-cloud.tgz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		r.t.Fatal(err)
	}
	sum, err := archive.Sum(p)
	if err != nil {
		r.t.Fatal(err)
	}
	return p, sum
}

func (r *rig) opts() ReleaseOptions {
	return ReleaseOptions{App: "one-cloud", Stack: "prod", Archive: r.archive, Config: r.config, SHA256: r.sum, Tag: "v1.0.0"}
}

func (r *rig) ledger() []ledger.Entry {
	r.t.Helper()
	entries, err := ledger.Read(filepath.Join(r.d.LedgerDir, "one-cloud.jsonl"))
	if err != nil {
		r.t.Fatal(err)
	}
	return entries
}

func TestAReleaseIsCheckedShippedAndRecorded(t *testing.T) {
	r := newRig(t)
	e, err := r.d.Release(context.Background(), r.opts())
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	const id = "20261023T180000Z-abc1234def56"
	if r.tr.sys.live() != id {
		t.Errorf("current = %q", r.tr.sys.live())
	}
	got := r.ledger()
	if len(got) != 1 || !reflect.DeepEqual(got[0], e) {
		t.Fatalf("ledger = %+v, returned %+v", got, e)
	}
	if e.Action != ledger.Deploy || e.Release != id || e.Revision != "abc1234def56" || e.SHA256 != r.sum || e.Tag != "v1.0.0" || e.Operator != "tester" || e.Previous != "" {
		t.Errorf("entry = %+v", e)
	}
	want := []string{"mktemp", "upload deployforge-linux-amd64 deployforge", "upload one-cloud.tgz release.tgz"}
	if strings.Join(r.tr.calls[:3], "|") != strings.Join(want, "|") || !strings.HasPrefix(r.tr.calls[3], "sudo ") {
		t.Errorf("calls = %q", r.tr.calls)
	}
	// Secrets travel on standard input only: never in an argument or an upload.
	for _, c := range r.tr.calls {
		if strings.Contains(c, secretValue) || strings.Contains(c, "postgres://") {
			t.Errorf("a secret on a command line: %q", c)
		}
	}
	for _, u := range r.tr.uploads {
		if strings.Contains(u, secretValue) {
			t.Error("a secret in an uploaded file")
		}
	}
	if strings.Contains(r.log.String(), secretValue) {
		t.Errorf("a secret in the log:\n%s", r.log.String())
	}
	env, err := os.ReadFile(filepath.Join(r.tr.sys.root, "etc", "one-cloud", "env"))
	if err != nil || string(env) != "SESSION_KEY='"+secretValue+"'\n" {
		t.Errorf("env on the host = %q, %v", env, err)
	}

	// A second release names the first as previous.
	r.clock = r.clock.Add(24 * time.Hour)
	e2, err := r.d.Release(context.Background(), r.opts())
	if err != nil {
		t.Fatal(err)
	}
	if e2.Previous != id {
		t.Errorf("previous = %q", e2.Previous)
	}
}

func TestNothingIsSentUntilEveryLocalCheckPasses(t *testing.T) {
	r := newRig(t)
	noBinary, noBinarySum := r.build("one-cloud abc1234\n", false)
	notELF := filepath.Join(r.dir, "deployforge-script")
	if err := os.WriteFile(notELF, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		mutate func(*ReleaseOptions, *Deployer)
	}{
		"no checksum":           {func(o *ReleaseOptions, _ *Deployer) { o.SHA256 = "" }},
		"a wrong checksum":      {func(o *ReleaseOptions, _ *Deployer) { o.SHA256 = strings.Repeat("0", 64) }},
		"a missing archive":     {func(o *ReleaseOptions, _ *Deployer) { o.Archive = filepath.Join(r.dir, "nope.tgz") }},
		"a missing config":      {func(o *ReleaseOptions, _ *Deployer) { o.Config = filepath.Join(r.dir, "nope.yaml") }},
		"an unknown app":        {func(o *ReleaseOptions, _ *Deployer) { o.App = "other" }},
		"an unknown stack":      {func(o *ReleaseOptions, _ *Deployer) { o.Stack = "qa" }},
		"an archive no binary":  {func(o *ReleaseOptions, _ *Deployer) { o.Archive, o.SHA256 = noBinary, noBinarySum }},
		"a host binary not ELF": {func(_ *ReleaseOptions, d *Deployer) { d.HostBinary = notELF }},
		"keep below two":        {func(_ *ReleaseOptions, d *Deployer) { d.Keep = 1 }},
		"no operator":           {func(_ *ReleaseOptions, d *Deployer) { d.Operator = " " }},
		"no environment in SSM": {func(_ *ReleaseOptions, d *Deployer) { d.Store = store{} }},
	} {
		r := newRig(t)
		o := r.opts()
		c.mutate(&o, r.d)
		if _, err := r.d.Release(context.Background(), o); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(r.tr.calls) != 0 {
			t.Errorf("%s: reached the host: %v", name, r.tr.calls)
		}
		if _, err := os.Stat(r.d.LedgerDir); err == nil {
			t.Errorf("%s: touched the ledger", name)
		}
	}
	// A checksum in <archive>.sha256 is accepted.
	if err := os.WriteFile(r.archive+".sha256", []byte(r.sum+"  one-cloud.tgz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := r.opts()
	o.SHA256 = ""
	if _, err := r.d.Release(context.Background(), o); err != nil {
		t.Errorf("a .sha256 file beside the archive: %v", err)
	}
}

func TestAFailedDeployIsNotRecorded(t *testing.T) {
	r := newRig(t)
	r.tr.sys.fail = true
	if _, err := r.d.Release(context.Background(), r.opts()); err == nil {
		t.Fatal("a deploy whose validate failed succeeded")
	}
	if len(r.ledger()) != 0 {
		t.Error("a failed deploy was recorded")
	}
	if !strings.Contains(r.log.String(), "validate failed") {
		t.Errorf("the host's error was not shown:\n%s", r.log.String())
	}
}

func TestARollbackIsRecordedAfterItHappens(t *testing.T) {
	r := newRig(t)
	a, err := r.d.Release(context.Background(), r.opts())
	if err != nil {
		t.Fatal(err)
	}
	r.clock = r.clock.Add(24 * time.Hour)
	b, err := r.d.Release(context.Background(), r.opts())
	if err != nil {
		t.Fatal(err)
	}
	r.clock = r.clock.Add(time.Hour)
	before := len(r.tr.calls)
	e, err := r.d.Rollback(context.Background(), "one-cloud", "prod")
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	if e.Action != ledger.Rollback || e.Release != a.Release || e.Previous != b.Release || e.Revision != "abc1234def56" {
		t.Errorf("entry = %+v", e)
	}
	if got := r.ledger(); len(got) != 3 {
		t.Errorf("ledger has %d entries, want 3", len(got))
	}
	if r.tr.sys.live() != a.Release {
		t.Errorf("current = %s", r.tr.sys.live())
	}
	if rb := strings.Join(r.tr.calls[before:], "|"); strings.Contains(rb, "release.tgz") || !strings.Contains(rb, "upload deployforge-linux-amd64 deployforge") {
		t.Errorf("a rollback uploads deployforge and no archive: %s", rb)
	}
}

func TestARollbackTheLedgerRefusesSaysHowToRecordIt(t *testing.T) {
	r := newRig(t)
	if _, err := r.d.Release(context.Background(), r.opts()); err != nil {
		t.Fatal(err)
	}
	// A release on the host the ledger never saw.
	if err := os.MkdirAll(filepath.Join(r.tr.sys.root, "opt", "one-cloud", "releases", "20200101T000000Z-aaaaaaa"), 0o750); err != nil {
		t.Fatal(err)
	}
	r.clock = r.clock.Add(time.Hour)
	_, err := r.d.Rollback(context.Background(), "one-cloud", "prod")
	var nr *NotRecordedError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "THE ROLLBACK IS LIVE BUT NOT RECORDED") || !strings.Contains(nr.Command, "ledger --dir") || !strings.Contains(nr.Command, "--action rollback") {
		t.Errorf("err = %v", err)
	}
}

func TestALocalBuildsInputsReachTheLedger(t *testing.T) {
	r := newRig(t)
	r.archive, r.sum = r.build("one-cloud abc1234def56\ninput github.com/plexusone/systemforge 0123456789abcdef0123456789abcdef01234567\n", true)
	e, err := r.d.Release(context.Background(), r.opts())
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	if len(e.Inputs) != 1 || e.Inputs[0].Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("entry inputs = %+v", e.Inputs)
	}
	if got := r.ledger(); len(got) != 1 || !reflect.DeepEqual(got[0].Inputs, e.Inputs) {
		t.Fatalf("ledger = %+v", got)
	}
	// Recording by hand, when the ledger could not be written, keeps them.
	if cmd := recordCommand("deployments", e); !strings.Contains(cmd, "--input github.com/plexusone/systemforge@0123456789abcdef0123456789abcdef01234567") {
		t.Errorf("record command = %s", cmd)
	}
}

func TestAnAppsSelfCheckReachesTheLedgerOnlyWhenTurnedOn(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	o.Selfcheck = "report"
	// the fleet document has not turned it on: no self-check runs.
	e, err := r.d.Release(context.Background(), o)
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	if e.Selfcheck != nil || len(r.tr.sys.selfchecks) != 0 {
		t.Fatalf("ran without the app's opt-in: %+v %v", e.Selfcheck, r.tr.sys.selfchecks)
	}

	r.d.Fleet.Apps[0].Selfcheck = true
	prod := r.d.Fleet.Apps[0].Stacks["prod"]
	prod.Limits = &fleet.Limits{MemoryMax: "512M", CPUQuota: "50%", ConnectionLimit: 10}
	r.d.Fleet.Apps[0].Stacks["prod"] = prod
	r.clock = r.clock.Add(time.Hour)
	r.archive, r.sum = r.build("one-cloud 0123456789ab\n", true)
	o = r.opts()
	o.Selfcheck = "report"
	if e, err = r.d.Release(context.Background(), o); err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	if sc := e.Selfcheck; sc == nil || sc.Mode != "report" || sc.Pre != "pass" || sc.Post != "pass" {
		t.Fatalf("entry selfcheck = %+v", e.Selfcheck)
	}
	if len(r.tr.sys.selfchecks) != 2 || !strings.HasPrefix(r.tr.sys.selfchecks[0], "pre ") || strings.HasSuffix(r.tr.sys.selfchecks[0], " 0") {
		t.Errorf("stages run = %v", r.tr.sys.selfchecks)
	}
	if got := r.ledger(); got[len(got)-1].Selfcheck == nil {
		t.Errorf("the ledger lacks the self-check: %+v", got[len(got)-1])
	}
	o.Selfcheck = "sometimes"
	if _, err := r.d.Release(context.Background(), o); err == nil {
		t.Error("an unknown self-check mode was accepted")
	}
}

func TestUnlockClearsAStaleLock(t *testing.T) {
	r := newRig(t)
	lock := filepath.Join(r.tr.sys.root, "opt", "one-cloud", "deploy.lock")
	if err := os.WriteFile(lock, []byte(`{"operator":"alice","action":"release","since":"2026-10-23T18:00:00Z","pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The host's reason reaches the operator on the log, as every on-host
	// error does.
	if _, err := r.d.Release(context.Background(), r.opts()); err == nil || !strings.Contains(r.log.String(), "unlock the stack") {
		t.Fatalf("a locked stack deployed: %v\n%s", err, r.log.String())
	}
	if err := r.d.Unlock(context.Background(), "one-cloud", "prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Release(context.Background(), r.opts()); err != nil {
		t.Fatalf("after unlock: %v\n%s", err, r.log.String())
	}
	if got := r.ledger(); len(got) != 1 {
		t.Errorf("an unlock reached the ledger: %+v", got)
	}
}

func TestHooksRunAroundAReleaseAndARollback(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	logFile := filepath.Join(dir, "hooks.log")
	for _, h := range hooks.Names() {
		body := "#!/bin/sh\necho \"$DEPLOYFORGE_HOOK $DEPLOYFORGE_RELEASE $DEPLOYFORGE_ERROR\" >> " + logFile + "\n"
		if err := os.WriteFile(filepath.Join(dir, h), []byte(body), 0o755); err != nil { //nolint:gosec // G306: a hook must be executable
			t.Fatal(err)
		}
	}
	r.d.Hooks = &hooks.Runner{Dir: dir}
	e, err := r.d.Release(context.Background(), r.opts())
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	r.clock = r.clock.Add(time.Hour)
	r.archive, r.sum = r.build("one-cloud 0123456789ab\n", true)
	if _, err := r.d.Release(context.Background(), r.opts()); err != nil {
		t.Fatal(err)
	}
	r.clock = r.clock.Add(time.Hour)
	if _, err := r.d.Rollback(context.Background(), "one-cloud", "prod"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logFile) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if len(lines) != 6 || lines[0] != "before-deploy "+e.Release || lines[1] != "after-deploy "+e.Release ||
		lines[4] != "before-rollback" || lines[5] != "after-rollback "+e.Release {
		t.Errorf("hooks ran:\n%s", data)
	}
}

func TestAFailingBeforeHookStopsTheDeployAndAFailureRunsTheFailedHook(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, hooks.BeforeDeploy), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil { //nolint:gosec // G306: a hook must be executable
		t.Fatal(err)
	}
	r.d.Hooks = &hooks.Runner{Dir: dir}
	if _, err := r.d.Release(context.Background(), r.opts()); err == nil || !strings.Contains(err.Error(), "before-deploy failed") {
		t.Fatalf("err = %v", err)
	}
	if len(r.tr.calls) != 0 {
		t.Errorf("the host was reached: %q", r.tr.calls)
	}

	marker := filepath.Join(dir, "failed.txt")
	if err := os.Remove(filepath.Join(dir, hooks.BeforeDeploy)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hooks.Failed), []byte("#!/bin/sh\necho \"$DEPLOYFORGE_ERROR\" > "+marker+"\n"), 0o755); err != nil { //nolint:gosec // G306: a hook must be executable
		t.Fatal(err)
	}
	// The release this deploy makes never becomes ready.
	r.tr.sys.unhealthy[r.clock.Format("20060102T150405Z")+"-abc1234def56"] = true
	if _, err := r.d.Release(context.Background(), r.opts()); err == nil {
		t.Fatal("an unhealthy release succeeded")
	}
	if data, err := os.ReadFile(marker); err != nil || !strings.Contains(string(data), "failed") { //nolint:gosec // G304: the test's own file
		t.Errorf("the failed hook: %q, %v", data, err)
	}
}
