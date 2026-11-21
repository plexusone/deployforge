package onhost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plexusone/deployforge/deploy/secrets"
	"github.com/plexusone/deployforge/deploy/target"
)

var tgt = target.Target{
	App: "one-cloud", Stack: "prod", Host: "web-1", Unit: "one-cloud", User: "onecloud",
	Listen: "127.0.0.1:8101", HealthHosts: []string{"app.one.example.com", "auth.one.example.com"}, SecretPrefix: "/example/prod/one-cloud/",
}

// fakeSys records what a deploy asks of systemd and answers health probes for
// the release current points at.
type fakeSys struct {
	root  string
	calls []string
	fail  map[string]bool // "validate" or "migrate"
	// failMessage, when set, is the error a failing step returns.
	failMessage string
	unhealthy   map[string]bool // release names that never become ready
	// migrateEnv is the migrate environment file's contents when migrate ran.
	migrateEnv string
	owned      []string
	// checks is what each self-check stage reports: its exit status and
	// report; a stage not listed passes.
	checks map[string]stageReport
	// sandboxes is the number of properties each self-check ran with.
	sandboxes []int
}

type stageReport struct {
	status int
	report string
}

func (s *fakeSys) live() string {
	dest, err := os.Readlink(filepath.Join(s.root, "opt", tgt.Unit, "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(dest)
}

func (s *fakeSys) DaemonReload(context.Context) error {
	s.calls = append(s.calls, "daemon-reload")
	return nil
}
func (s *fakeSys) Restart(_ context.Context, unit string) error {
	s.calls = append(s.calls, "restart "+unit+" "+s.live())
	return nil
}
func (s *fakeSys) Stop(_ context.Context, unit string) error {
	s.calls = append(s.calls, "stop "+unit)
	return nil
}
func (s *fakeSys) RunAs(_ context.Context, user string, envFiles, argv []string, _, _ io.Writer) error {
	step := argv[1]
	s.calls = append(s.calls, fmt.Sprintf("%s as %s with %d env files", step, user, len(envFiles)))
	if step == "migrate" {
		data, err := os.ReadFile(envFiles[1])
		if err != nil {
			return err
		}
		s.migrateEnv = string(data)
	}
	if s.fail[step] {
		if s.failMessage != "" {
			return errors.New(s.failMessage)
		}
		return errors.New("exit status 1")
	}
	return nil
}
func (s *fakeSys) RunSandboxed(_ context.Context, user string, envFiles, props []string, maxSeconds int, argv []string, stdout, _ io.Writer) (int, error) {
	stage := argv[len(argv)-1]
	s.calls = append(s.calls, fmt.Sprintf("selfcheck %s as %s with %d env files, %ds, release %s", stage, user, len(envFiles), maxSeconds, filepath.Base(filepath.Dir(argv[0]))))
	s.sandboxes = append(s.sandboxes, len(props))
	r, ok := s.checks[stage]
	if !ok {
		r = stageReport{report: `{"app":"host","check":"zones","stage":"` + stage + `","status":"pass","ms":1}` + "\n"}
	}
	if _, err := io.WriteString(stdout, r.report); err != nil {
		return -1, err
	}
	return r.status, nil
}
func (s *fakeSys) Journal(context.Context, string, int) string { return "journal lines" }
func (s *fakeSys) Ready(context.Context, string, string) bool  { return !s.unhealthy[s.live()] }
func (s *fakeSys) Own(path, group string) error {
	if group != tgt.User {
		return fmt.Errorf("owned by %s", group)
	}
	s.owned = append(s.owned, filepath.Base(path))
	return nil
}

type rig struct {
	t    *testing.T
	root string
	sys  *fakeSys
	host *Host
	log  bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"opt/one-cloud/releases", "etc/one-cloud"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	r := &rig{t: t, root: root, sys: &fakeSys{root: root, fail: map[string]bool{}, unhealthy: map[string]bool{}}}
	r.host = &Host{Root: root, Sys: r.sys, Log: &r.log, Sleep: func(time.Duration) {}}
	return r
}

// work makes an upload directory holding a release archive.
func (r *rig) work(withBinary bool) string {
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
	add("REVISION", "one-cloud abc1234\n", 0o644)
	if withBinary {
		add("one-cloud", "#!/bin/sh\n", 0o755)
	}
	if err := tw.Close(); err != nil {
		r.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		r.t.Fatal(err)
	}
	dir := r.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "release.tgz"), buf.Bytes(), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return dir
}

func request(id string, files ...string) Request {
	b := secrets.Bundle{
		Env:     []secrets.Var{{Name: "SESSION_KEY", Value: "s3cr3t"}},
		Migrate: []secrets.Var{{Name: "ADMIN_DSN", Value: "postgres://owner@db/one"}},
	}
	for _, f := range files {
		b.Files = append(b.Files, secrets.File{Name: f, Data: []byte(f + " for " + id + "\n")})
	}
	return Request{
		Action: ActionRelease, Target: tgt, Operator: "tester", ReleaseID: id, Config: []byte("config for " + id + "\n"),
		Secrets: b, HealthTimeout: 3, Keep: 2,
	}
}

func (r *rig) etc(name string) string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.root, "etc", "one-cloud", name)) //nolint:gosec // G304: a path under the test's own temporary directory
	if err != nil {
		return "<missing>"
	}
	return string(data)
}

func (r *rig) release(id string, files ...string) (Result, error) {
	r.t.Helper()
	return r.host.Run(context.Background(), request(id, files...), r.work(true))
}

const (
	idA = "20261023T180000Z-aaaaaaa"
	idB = "20261024T090000Z-bbbbbbb"
	idC = "20261025T090000Z-ccccccc"
)

func TestAFirstReleaseIsInstalledValidatedMigratedAndSwitchedTo(t *testing.T) {
	r := newRig(t)
	res, err := r.release(idA, "signing-key.pem")
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	if res != (Result{Release: idA}) {
		t.Errorf("result = %+v", res)
	}
	if r.sys.live() != idA {
		t.Errorf("current = %q", r.sys.live())
	}
	want := []string{"validate as onecloud with 1 env files", "migrate as onecloud with 2 env files", "daemon-reload", "restart one-cloud.service " + idA}
	if strings.Join(r.sys.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q\nwant %q", r.sys.calls, want)
	}
	if r.etc("config.yaml") != "config for "+idA+"\n" || r.etc("env") != "SESSION_KEY='s3cr3t'\n" || r.etc(".managed") != "signing-key.pem\n" {
		t.Errorf("etc: config %q env %q managed %q", r.etc("config.yaml"), r.etc("env"), r.etc(".managed"))
	}
	st, err := os.Stat(filepath.Join(r.root, "etc", "one-cloud", "signing-key.pem"))
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("key file: %v %v", st, err)
	}
	if r.sys.migrateEnv != "ADMIN_DSN='postgres://owner@db/one'\n" {
		t.Errorf("migrate env = %q", r.sys.migrateEnv)
	}
	if len(r.sys.owned) != 4 {
		t.Errorf("owned = %v, want config.yaml, env, the key file and .managed", r.sys.owned)
	}
	// Nothing saved is left behind.
	matches, err := filepath.Glob(filepath.Join(r.root, "etc", "one-cloud", "*.prev"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("saved files left: %v", matches)
	}
}

func TestTheMigrateEnvironmentDoesNotStayOnTheHost(t *testing.T) {
	r := newRig(t)
	work := r.work(true)
	if _, err := r.host.Run(context.Background(), request(idA), work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "migrate.env")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("migrate.env stayed: %v", err)
	}
}

func TestASecondReleaseReplacesFilesAndDropsKeysNoLongerInTheStore(t *testing.T) {
	r := newRig(t)
	if _, err := r.release(idA, "old-key.pem", "signing-key.pem"); err != nil {
		t.Fatal(err)
	}
	res, err := r.release(idB, "signing-key.pem")
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	if res != (Result{Release: idB, Previous: idA}) {
		t.Errorf("result = %+v", res)
	}
	if r.etc("old-key.pem") != "<missing>" {
		t.Error("a key gone from the store stayed on the host")
	}
	if r.etc("signing-key.pem") != "signing-key.pem for "+idB+"\n" {
		t.Errorf("key = %q", r.etc("signing-key.pem"))
	}
}

func TestAFailedValidateKeepsTheRunningReleaseAndItsFiles(t *testing.T) {
	for _, step := range []string{"validate", "migrate"} {
		r := newRig(t)
		if _, err := r.release(idA, "old-key.pem"); err != nil {
			t.Fatal(err)
		}
		r.sys.fail[step] = true
		r.sys.calls = nil
		_, err := r.release(idB, "new-key.pem")
		if err == nil || !strings.Contains(err.Error(), step+" failed") {
			t.Fatalf("%s: err = %v", step, err)
		}
		if r.sys.live() != idA {
			t.Errorf("%s: current = %q", step, r.sys.live())
		}
		if _, err := os.Stat(filepath.Join(r.root, "opt", "one-cloud", "releases", idB)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the failed release stayed", step)
		}
		if r.etc("config.yaml") != "config for "+idA+"\n" || r.etc("old-key.pem") != "old-key.pem for "+idA+"\n" || r.etc("new-key.pem") != "<missing>" || r.etc(".managed") != "old-key.pem\n" {
			t.Errorf("%s: etc not restored: config %q old %q new %q managed %q", step, r.etc("config.yaml"), r.etc("old-key.pem"), r.etc("new-key.pem"), r.etc(".managed"))
		}
		for _, c := range r.sys.calls {
			if strings.HasPrefix(c, "restart") {
				t.Errorf("%s: restarted the service: %v", step, r.sys.calls)
			}
		}
	}
}

func TestAnUnhealthyReleaseIsRolledBackWithItsFiles(t *testing.T) {
	r := newRig(t)
	if _, err := r.release(idA); err != nil {
		t.Fatal(err)
	}
	r.sys.unhealthy[idB] = true
	r.sys.calls = nil
	_, err := r.release(idB, "new-key.pem")
	if err == nil || !strings.Contains(err.Error(), "failed its health check") {
		t.Fatalf("err = %v", err)
	}
	if r.sys.live() != idA {
		t.Errorf("current = %q", r.sys.live())
	}
	if r.etc("config.yaml") != "config for "+idA+"\n" || r.etc("new-key.pem") != "<missing>" {
		t.Errorf("etc not restored: config %q key %q", r.etc("config.yaml"), r.etc("new-key.pem"))
	}
	last := r.sys.calls[len(r.sys.calls)-1]
	if last != "restart one-cloud.service "+idA {
		t.Errorf("last call = %q", last)
	}
	if !strings.Contains(r.log.String(), "journal lines") {
		t.Errorf("the journal was not shown:\n%s", r.log.String())
	}
}

func TestAnUnhealthyFirstReleaseStopsTheService(t *testing.T) {
	r := newRig(t)
	r.sys.unhealthy[idA] = true
	if _, err := r.release(idA); err == nil {
		t.Fatal("an unhealthy first release succeeded")
	}
	if _, err := os.Lstat(filepath.Join(r.root, "opt", "one-cloud", "current")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("current stayed: %v", err)
	}
	if r.sys.calls[len(r.sys.calls)-1] != "stop one-cloud.service" {
		t.Errorf("calls = %v", r.sys.calls)
	}
}

func TestOldReleasesArePrunedButNotTheOneReplaced(t *testing.T) {
	r := newRig(t)
	for _, id := range []string{idA, idB, idC} {
		if _, err := r.release(id); err != nil {
			t.Fatal(err)
		}
	}
	names, err := releaseNames(filepath.Join(r.root, "opt", "one-cloud", "releases"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, " ") != idB+" "+idC {
		t.Errorf("releases = %v, want the two newest", names)
	}
}

func TestARollbackSwitchesToTheReleaseBeforeTheCurrentOne(t *testing.T) {
	r := newRig(t)
	for _, id := range []string{idA, idB} {
		if _, err := r.release(id); err != nil {
			t.Fatal(err)
		}
	}
	rb := Request{Action: ActionRollback, Target: tgt, Operator: "tester", HealthTimeout: 3, Keep: 2}
	res, err := r.host.Run(context.Background(), rb, "")
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Release: idA, Previous: idB}) || r.sys.live() != idA {
		t.Errorf("result = %+v, current = %s", res, r.sys.live())
	}
	// Nothing older than idA: refused, and nothing changes.
	if _, err := r.host.Run(context.Background(), rb, ""); err == nil || !strings.Contains(err.Error(), "no release older") {
		t.Errorf("err = %v", err)
	}
	if r.sys.live() != idA {
		t.Errorf("current = %s", r.sys.live())
	}
}

func TestAnUnhealthyRollbackRestoresTheCurrentRelease(t *testing.T) {
	r := newRig(t)
	for _, id := range []string{idA, idB} {
		if _, err := r.release(id); err != nil {
			t.Fatal(err)
		}
	}
	r.sys.unhealthy[idA] = true
	_, err := r.host.Run(context.Background(), Request{Action: ActionRollback, Target: tgt, Operator: "tester", HealthTimeout: 3, Keep: 2}, "")
	if err == nil {
		t.Fatal("an unhealthy rollback succeeded")
	}
	if r.sys.live() != idB {
		t.Errorf("current = %s, want %s restored", r.sys.live(), idB)
	}
	if _, err := r.host.Run(context.Background(), Request{Action: ActionRollback, Target: tgt, Operator: "tester", HealthTimeout: 3, Keep: 2}, ""); err == nil {
		t.Error("a second unhealthy rollback succeeded")
	}
	empty := newRig(t)
	if _, err := empty.host.Run(context.Background(), Request{Action: ActionRollback, Target: tgt, Operator: "tester", HealthTimeout: 3, Keep: 2}, ""); err == nil || !strings.Contains(err.Error(), "no current release") {
		t.Errorf("a stack never deployed: %v", err)
	}
}

func TestARequestIsCheckedBeforeAnythingChanges(t *testing.T) {
	r := newRig(t)
	work := r.work(true)
	for name, mutate := range map[string]func(*Request){
		"a bad release id":       func(q *Request) { q.ReleaseID = "../../etc" },
		"no configuration":       func(q *Request) { q.Config = nil },
		"no environment":         func(q *Request) { q.Secrets.Env = nil },
		"keep below two":         func(q *Request) { q.Keep = 1 },
		"no health timeout":      func(q *Request) { q.HealthTimeout = 0 },
		"an unknown action":      func(q *Request) { q.Action = "destroy" },
		"an unsafe unit":         func(q *Request) { q.Target.Unit = "../one" },
		"a public listen":        func(q *Request) { q.Target.Listen = "0.0.0.0:8101" },
		"an existing release id": func(q *Request) { q.ReleaseID = idA },
	} {
		if name == "an existing release id" {
			if _, err := r.release(idA); err != nil {
				t.Fatal(err)
			}
		}
		q := request(idB)
		mutate(&q)
		if _, err := r.host.Run(context.Background(), q, work); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	noBinary := newRig(t)
	if _, err := noBinary.host.Run(context.Background(), request(idA), noBinary.work(false)); err == nil || !strings.Contains(err.Error(), "not an executable") {
		t.Errorf("an archive without the binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(noBinary.root, "opt", "one-cloud", "releases", idA)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the release without a binary stayed")
	}
}
