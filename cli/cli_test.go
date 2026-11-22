package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
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

func TestShipRefusesWhatItCannotShip(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a planned stack":     {[]string{"deploy", "ship", "example-app", "staging", "--tag", "v1.0.0"}, `is "planned"`},
		"no tag and no local": {[]string{"deploy", "ship", "example-app", "prod"}, "either --tag or --local"},
		"both":                {[]string{"deploy", "ship", "example-app", "prod", "--tag", "v1.0.0", "--local"}, "either --tag or --local"},
		"with without local":  {[]string{"deploy", "ship", "example-app", "prod", "--tag", "v1.0.0", "--with", "github.com/a/b"}, "need --local"},
		"web without local":   {[]string{"deploy", "ship", "example-app", "prod", "--tag", "v1.0.0", "--with-web", "github.com/a/b"}, "need --local"},
		"no config":           {[]string{"deploy", "ship", "example-app", "prod", "--tag", "v1.0.0", "--config", "/nonexistent/config.yaml"}, "the stack's config"},
		"an unknown app":      {[]string{"deploy", "ship", "other", "prod", "--tag", "v1.0.0"}, `no app "other"`},
	} {
		if _, err := run(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestUpstreams(t *testing.T) {
	ups, err := upstreams([]string{"github.com/example/lib@feature", "github.com/example/other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].Ref != "feature" || ups[1].Ref != "" || !strings.HasSuffix(ups[1].Dir, filepath.Join("src", "github.com", "example", "other")) {
		t.Errorf("upstreams = %+v", ups)
	}
	if _, err := upstreams([]string{"lib"}); err == nil {
		t.Error("a repository without host and org")
	}
}

func TestTakesSnapshotOnlyForThePrimaryStackWithDatabasesOnLightsail(t *testing.T) {
	f, err := fleet.Load(testFleet)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := f.App("example-app") // the sample has it
	if takesSnapshot(f, a, "prod") {
		t.Error("a snapshot without a database server")
	}
	f.DatabaseServer = &fleet.DatabaseServer{LightsailName: "fleet-prod-pg"}
	if !takesSnapshot(f, a, "prod") {
		t.Error("no snapshot for prod, which has databases")
	}
	if takesSnapshot(f, a, "staging") {
		t.Error("a snapshot for staging, which is not the primary stack")
	}
	f.DatabaseServer.LightsailName = ""
	if takesSnapshot(f, a, "prod") {
		t.Error("a snapshot of a server that is not on Lightsail")
	}
	if _, err := run(t, "db", "snapshot", "list"); err == nil || !strings.Contains(err.Error(), "no lightsailName") {
		t.Errorf("list without a server: %v", err)
	}
}

func TestZonesValidateSchemaPageAndHardenNeedsAToken(t *testing.T) {
	const doc = "../zones/testdata/zones.json"
	if out, err := run(t, "zones", "--zones", doc, "validate"); err != nil || !strings.Contains(out, "1 zones, 4 records OK") {
		t.Errorf("validate: %q, %v", out, err)
	}
	if out, err := run(t, "zones", "schema"); err != nil || !strings.Contains(out, `"$id"`) {
		t.Errorf("schema: %v", err)
	}
	if out, err := run(t, "zones", "--zones", doc, "page"); err != nil || !strings.Contains(out, "app.example.com") {
		t.Errorf("page: %v\n%s", err, out)
	}
	t.Setenv(TokenEnv, "")
	if _, err := run(t, "zones", "--zones", doc, "harden", "plan"); err == nil || !strings.Contains(err.Error(), TokenEnv) {
		t.Errorf("harden without a token: %v", err)
	}
}
