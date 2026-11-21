package onhost

import (
	"context"
	"strings"
	"testing"
)

var sandbox = []string{"MemoryMax=512M", "ProtectSystem=strict", "SystemCallFilter=~@privileged @resources", "ReadOnlyPaths=/opt/one-cloud /etc/one-cloud"}

func (r *rig) checkedRelease(id, mode string) (Result, error) {
	r.t.Helper()
	req := request(id)
	req.Selfcheck = &SelfcheckRequest{Mode: mode, Properties: sandbox}
	return r.host.Run(context.Background(), req, r.work(true))
}

func failing(stage, check, detail string) stageReport {
	return stageReport{status: 1, report: `{"app":"host","check":"ok","stage":"` + stage + `","status":"pass"}` + "\n" +
		`{"app":"portfolio","check":"` + check + `","stage":"` + stage + `","status":"fail","detail":"` + detail + `"}` + "\n"}
}

func TestThePreStageRunsTheNewReleaseBeforeMigrateAndPostAfterTheSwitch(t *testing.T) {
	r := newRig(t)
	res, err := r.checkedRelease(idA, SelfcheckEnforce)
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	want := []string{
		"validate as onecloud with 1 env files",
		"selfcheck pre as onecloud with 1 env files, 45s, release " + idA,
		"migrate as onecloud with 2 env files",
		"daemon-reload",
		"restart one-cloud.service " + idA,
		"selfcheck post as onecloud with 1 env files, 45s, release " + idA,
	}
	if strings.Join(r.sys.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q\nwant %q", r.sys.calls, want)
	}
	if len(r.sys.sandboxes) != 2 || r.sys.sandboxes[0] != len(sandbox) {
		t.Errorf("sandboxes = %v", r.sys.sandboxes)
	}
	if sc := res.Selfcheck; sc == nil || sc.Pre != "pass" || sc.Post != "pass" || len(sc.Failed) != 0 || sc.Mode != SelfcheckEnforce {
		t.Errorf("selfcheck = %+v", res.Selfcheck)
	}
}

func TestAFailedPreStageKeepsTheRunningReleaseAndMigratesNothing(t *testing.T) {
	r := newRig(t)
	if _, err := r.release(idA); err != nil {
		t.Fatal(err)
	}
	r.sys.calls = nil
	r.sys.checks = map[string]stageReport{"pre": failing("pre", "timezone", "unknown time zone America/Los_Angeles")}
	_, err := r.checkedRelease(idB, SelfcheckEnforce)
	if err == nil || !strings.Contains(err.Error(), "portfolio/timezone") {
		t.Fatalf("err = %v", err)
	}
	if r.sys.live() != idA || r.etc("config.yaml") != "config for "+idA+"\n" {
		t.Errorf("current %s, config %q", r.sys.live(), r.etc("config.yaml"))
	}
	for _, c := range r.sys.calls {
		if strings.HasPrefix(c, "migrate") || strings.HasPrefix(c, "restart") {
			t.Errorf("after a failed pre stage: %s", c)
		}
	}
	// The detail is for the operator's log only.
	if !strings.Contains(r.log.String(), "FAIL portfolio/timezone: unknown time zone America/Los_Angeles") {
		t.Errorf("log:\n%s", r.log.String())
	}
	if strings.Contains(err.Error(), "America") {
		t.Errorf("the error carries the detail: %v", err)
	}
}

func TestAFailedPostStageRollsBackAsAFailedHealthCheckDoes(t *testing.T) {
	r := newRig(t)
	if _, err := r.release(idA); err != nil {
		t.Fatal(err)
	}
	r.sys.checks = map[string]stageReport{"post": failing("post", "routes", "GET / answered 500")}
	if _, err := r.checkedRelease(idB, SelfcheckEnforce); err == nil || !strings.Contains(err.Error(), "post stage") {
		t.Fatalf("err = %v", err)
	}
	if r.sys.live() != idA || r.etc("config.yaml") != "config for "+idA+"\n" {
		t.Errorf("not rolled back: current %s, config %q", r.sys.live(), r.etc("config.yaml"))
	}
}

func TestReportModeRecordsFailuresAndChangesNothing(t *testing.T) {
	r := newRig(t)
	r.sys.checks = map[string]stageReport{
		"pre":  failing("pre", "postgres", "role is a superuser"),
		"post": failing("post", "routes", "GET / answered 500"),
	}
	res, err := r.checkedRelease(idA, SelfcheckReport)
	if err != nil {
		t.Fatalf("%v\n%s", err, r.log.String())
	}
	if r.sys.live() != idA {
		t.Errorf("current = %s", r.sys.live())
	}
	sc := res.Selfcheck
	if sc == nil || sc.Pre != "fail" || sc.Post != "fail" || strings.Join(sc.Failed, ",") != "portfolio/postgres,portfolio/routes" {
		t.Errorf("selfcheck = %+v", sc)
	}
}

func TestASelfCheckThatCannotRunFails(t *testing.T) {
	cases := map[string]stageReport{
		"exit status 2":           {status: 2, report: "config: cannot read\n"},
		"exit 1 with no failures": {status: 1},
		"a report too long":       {status: 0, report: strings.Repeat(`{"app":"a","check":"b","status":"pass"}`+"\n", 2000)},
	}
	for name, rep := range cases {
		r := newRig(t)
		r.sys.checks = map[string]stageReport{"pre": rep}
		res, err := r.checkedRelease(idA, SelfcheckReport)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Selfcheck.Pre != "fail" || len(res.Selfcheck.Failed) == 0 || !strings.HasPrefix(res.Selfcheck.Failed[0], "selfcheck/") {
			t.Errorf("%s: %+v", name, res.Selfcheck)
		}
	}
}

func TestASelfCheckRequestIsChecked(t *testing.T) {
	bad := map[string]*SelfcheckRequest{
		"an unknown mode":        {Mode: "warn", Properties: sandbox},
		"no sandbox":             {Mode: SelfcheckEnforce},
		"a property with a line": {Mode: SelfcheckEnforce, Properties: []string{"ExecStartPre=/bin/sh\nUser=root"}},
		"not a property":         {Mode: SelfcheckEnforce, Properties: []string{"--uid=root"}},
	}
	for name, sc := range bad {
		req := request(idA)
		req.Selfcheck = sc
		if err := req.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	rb := Request{Action: ActionRollback, Target: tgt, Operator: "tester", HealthTimeout: 3, Keep: 2, Selfcheck: &SelfcheckRequest{Mode: SelfcheckEnforce, Properties: sandbox}}
	if err := rb.Validate(); err == nil {
		t.Error("a rollback with a self-check was accepted")
	}
}
