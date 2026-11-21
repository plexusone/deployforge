package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const testFleet = "../fleet/testdata/fleet.json"

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := New(Options{Fleet: testFleet})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestValidateSchemaPortAndUnit(t *testing.T) {
	if out, err := run(t, "validate"); err != nil || out != "1 hosts, 1 apps and 2 stacks OK\n" {
		t.Errorf("validate: %q, %v", out, err)
	}
	out, err := run(t, "schema")
	if err != nil || !json.Valid([]byte(out)) || !strings.Contains(out, `"$id"`) {
		t.Errorf("schema: %v", err)
	}
	if out, err := run(t, "next-port", "web-1"); err != nil || out != "8103\n" {
		t.Errorf("next-port: %q, %v", out, err)
	}
	if _, err := run(t, "next-port", "nowhere"); err == nil {
		t.Error("next-port for an unknown host")
	}
	if out, err := run(t, "unit", "example-app", "prod"); err != nil || !strings.Contains(out, "User=exampleapp") {
		t.Errorf("unit: %v\n%s", err, out)
	}
	// Staging has no limits in the sample, so it gets no unit.
	if _, err := run(t, "unit", "example-app", "staging"); err == nil || !strings.Contains(err.Error(), "no limits") {
		t.Errorf("unit without limits: %v", err)
	}
}

func TestLedgerRecordShowAndCurrent(t *testing.T) {
	dir := t.TempDir()
	const rel = "20261023T180000Z-abc1234"
	sum := strings.Repeat("a", 64)
	if _, err := run(t, "ledger", "current", "--dir", dir, "example-app", "prod"); err == nil {
		t.Error("current before any deploy")
	}
	out, err := run(t, "ledger", "record", "--dir", dir, "--app", "example-app", "--stack", "prod",
		"--release", rel, "--revision", "abc1234", "--sha256", sum, "--tag", "v1.0.0", "--operator", "tester",
		"--time", "2026-10-23T18:00:00Z")
	if err != nil || !strings.Contains(out, "recorded deploy of "+rel+" on example-app/prod") {
		t.Fatalf("record: %q, %v", out, err)
	}
	if out, err := run(t, "ledger", "current", "--dir", dir, "example-app", "prod"); err != nil || out != rel+"\n" {
		t.Errorf("current: %q, %v", out, err)
	}
	if out, err := run(t, "ledger", "show", "--dir", dir, "example-app", "prod"); err != nil || !strings.Contains(out, "tag v1.0.0  by tester") {
		t.Errorf("show: %q, %v", out, err)
	}
	if _, err := run(t, "ledger", "show", "--dir", dir, "example-app", "qa"); err == nil {
		t.Error("show for an unknown stack")
	}
	if got, err := filepath.Glob(filepath.Join(dir, "*.jsonl")); err != nil || len(got) != 1 || filepath.Base(got[0]) != "example-app.jsonl" {
		t.Errorf("ledger files = %v, %v", got, err)
	}
}

func TestDeployNeedsItsFlagsAndOnHostItsWorkDirectory(t *testing.T) {
	if _, err := run(t, "deploy", "release", "example-app", "prod"); err == nil || !strings.Contains(err.Error(), `"archive"`) {
		t.Errorf("release without --archive: %v", err)
	}
	if _, err := run(t, "on-host", "--work", "/etc"); err == nil || !strings.Contains(err.Error(), "mktemp") {
		t.Errorf("on-host with a work directory mktemp did not make: %v", err)
	}
}
