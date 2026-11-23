package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/deploy/onhost"
	"github.com/plexusone/deployforge/deploy/report"
	"github.com/plexusone/deployforge/ledger"
)

func decodeReport(t *testing.T, s string) report.Report {
	t.Helper()
	var r report.Report
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("not one JSON report: %v\n%s", err, s)
	}
	if dec.More() {
		t.Fatalf("more than the report on standard output:\n%s", s)
	}
	return r
}

func TestFinishReportsAFailedStepAsJSON(t *testing.T) {
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	hostErr := &onhost.StepError{
		Failure: onhost.Failure{Step: onhost.StepHealth, Restored: true, Migrated: true, Error: "unhealthy"},
		Err:     errors.New("unhealthy"),
	}
	err := finish(c, formatJSON, &report.Report{Command: "release", App: "example-app", Stack: "prod"}, ledger.Entry{}, fmt.Errorf("deployer: %w", hostErr))
	if err == nil {
		t.Fatal("a failed release succeeded")
	}
	r := decodeReport(t, out.String())
	if r.OK || r.Phase != report.PhaseDeploy || r.Failure == nil || r.Failure.Step != onhost.StepHealth || !r.Failure.Restored || !r.Failure.Migrated {
		t.Errorf("report %+v", r)
	}
}

func TestFinishReportsTheEntryAsJSON(t *testing.T) {
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	e := ledger.Entry{App: "example-app", Stack: "prod", Action: ledger.Rollback, Release: "20260101T000000Z-abc"}
	if err := finish(c, formatJSON, &report.Report{Command: "rollback", App: "example-app", Stack: "prod"}, e, nil); err != nil {
		t.Fatal(err)
	}
	r := decodeReport(t, out.String())
	if !r.OK || r.Phase != "" || r.Entry == nil || r.Entry.Release != e.Release {
		t.Errorf("report %+v", r)
	}

	out.Reset()
	if err := finish(c, formatText, &report.Report{Command: "rollback"}, e, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "example-app/prod now runs 20260101T000000Z-abc\n" {
		t.Errorf("text %q", out.String())
	}
}

func TestShipReportsARefusalAsJSON(t *testing.T) {
	cmd := New(Options{Fleet: testFleet})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"deploy", "ship", "example-app", "staging", "--tag", "v1.0.0", "--format", "json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("shipped a planned stack")
	}
	r := decodeReport(t, out.String())
	if r.OK || r.Command != "ship" || r.Phase != report.PhasePreflight || !strings.Contains(r.Error, `is "planned"`) {
		t.Errorf("report %+v", r)
	}
}

func TestDeployRefusesAnUnknownFormat(t *testing.T) {
	if _, err := run(t, "deploy", "status", "example-app", "prod", "--format", "yaml"); err == nil || !strings.Contains(err.Error(), `--format "yaml"`) {
		t.Errorf("err = %v", err)
	}
}
