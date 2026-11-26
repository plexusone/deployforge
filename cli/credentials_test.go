package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plexusone/credforge"
	"github.com/plexusone/credforge/targets"
	"github.com/plexusone/systemspec-credential/credspec"
	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/fleet"
)

// credentialFleet writes the sample fleet with a credentials section and a
// one-credential registry under creds/, and returns the fleet's path.
func credentialFleet(t *testing.T, section map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(testFleet)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["credentials"] = section
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	fleetPath := filepath.Join(dir, "fleet.json")
	if err := os.WriteFile(fleetPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "creds"), 0o750); err != nil {
		t.Fatal(err)
	}
	reg := credspec.NewRegistry(credspec.Credential{
		ID: "example-app/prod/SESSION_KEY", Name: "SESSION_KEY", System: "example-app", Environment: "prod",
		Kind: credspec.KindRandomSecret, Status: credspec.StatusActive, Owner: "ops",
		Issuer:     credspec.Issuer{Type: credspec.IssuerSelf},
		Generation: credspec.Generation{Method: credspec.MethodRandom},
		Stores:     []credspec.Store{{Type: credspec.StoreAWSSSM, Role: credspec.RolePrimary, Account: "hosting", Path: "/example/prod/example-app/SESSION_KEY"}},
		Rotation:   credspec.Rotation{Policy: credspec.PolicyOnDemand},
		Handling:   credspec.Handling{Generate: credspec.ActorHuman, Distribute: credspec.ActorSystem, Rotate: credspec.ActorHuman},
	})
	if err := reg.Save(filepath.Join(dir, "creds", "credentials.json")); err != nil {
		t.Fatal(err)
	}
	return fleetPath
}

func runFleet(t *testing.T, fleetPath string, args ...string) (string, error) {
	t.Helper()
	cmd := New(Options{Fleet: fleetPath})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestCredentialsUseTheFleetsFiles(t *testing.T) {
	fleetPath := credentialFleet(t, map[string]string{"registry": "creds/credentials.json"})
	if out, err := runFleet(t, fleetPath, "credentials", "validate"); err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	if out, err := runFleet(t, fleetPath, "credentials", "record", "example-app/prod/SESSION_KEY", "created", "--at", "2026-01-02"); err != nil {
		t.Fatalf("record: %v\n%s", err, out)
	}
	// The event log defaults to events.json beside the registry.
	reg, err := credspec.LoadRegistry(filepath.Join(filepath.Dir(fleetPath), "creds", "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := credspec.LoadLog(filepath.Join(filepath.Dir(fleetPath), "creds", "events.json"), reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Events) != 1 {
		t.Errorf("events beside the registry: %+v", log.Events)
	}
	// A flag on the command line wins.
	other := filepath.Join(t.TempDir(), "other.json")
	if out, err := runFleet(t, fleetPath, "credentials", "record", "example-app/prod/SESSION_KEY", "verified", "--events", other); err != nil {
		t.Fatalf("record --events: %v\n%s", err, out)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("--events was not used: %v", err)
	}
}

func TestCredentialsNeedASection(t *testing.T) {
	if _, err := run(t, "credentials", "validate"); err == nil || !strings.Contains(err.Error(), "no credentials section") {
		t.Errorf("a fleet without credentials: %v", err)
	}
}

func TestTargetsFor(t *testing.T) {
	f, err := fleet.Load(testFleet)
	if err != nil {
		t.Fatal(err)
	}
	tf, err := TargetsFor(f, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tf.Bindings) != 2 || !slices.Contains(tf.Scan.GitHubRepositories, "example/example-app") {
		t.Errorf("default targets %+v", tf)
	}
	file := filepath.Join(t.TempDir(), "targets.json")
	if err := os.WriteFile(file, []byte(`{"formatVersion":"1","bindings":[{"store":"aws-ssm","account":"hosting","aws":{"profile":"ops"}}],"scan":{"githubRepositories":["example/example-app","example/other"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tf, err = TargetsFor(f, file)
	if err != nil {
		t.Fatal(err)
	}
	if len(tf.Bindings) != 1 || len(tf.Scan.GitHubRepositories) != 2 {
		t.Errorf("a targets file, with the apps' repositories added once: %+v", tf)
	}
}

// emptyStore lists no parameters, so every registered one is missing.
type emptyStore struct{}

func (emptyStore) Observe(context.Context, credforge.Scope) ([]credforge.Observation, error) {
	return nil, nil
}

type emptyConnector struct{}

func (emptyConnector) Observer(string, string) (credforge.Observer, error) { return emptyStore{}, nil }
func (emptyConnector) References(string, string) (credforge.ReferenceFinder, error) {
	return nil, nil
}

func TestPreflightNamesTheMissingSecret(t *testing.T) {
	fleetPath := credentialFleet(t, map[string]string{"registry": "creds/credentials.json"})
	f, err := fleet.Load(fleetPath)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := f.App("example-app")
	check := preflightFor(Options{CredentialConnector: func(*targets.File) credforge.Connector { return emptyConnector{} }}, &fleetPath)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err = check(cmd, f, a, "prod")
	if err == nil || !strings.Contains(err.Error(), "example-app/prod/SESSION_KEY") {
		t.Errorf("a missing secret: %v", err)
	}
	// Staging's prefix holds nothing the registry lists.
	if err := check(cmd, f, a, "staging"); err != nil || !strings.Contains(out.String(), "0 secret(s) in place") {
		t.Errorf("staging: %v\n%s", err, out.String())
	}
}

// heldStore lists the session key under the prod prefix, and a name no
// credential lists.
type heldStore struct{}

func (heldStore) Observe(_ context.Context, s credforge.Scope) ([]credforge.Observation, error) {
	var out []credforge.Observation
	for _, p := range []string{"/example/prod/example-app/SESSION_KEY", "/example/prod/example-app/STRAY"} {
		k := credforge.Key{Type: s.Type, Account: s.Account, Path: p}
		if s.Contains(k) {
			out = append(out, credforge.Observation{Key: k, Modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
		}
	}
	return out, nil
}

type heldConnector struct{}

func (heldConnector) Observer(string, string) (credforge.Observer, error) { return heldStore{}, nil }
func (heldConnector) References(string, string) (credforge.ReferenceFinder, error) {
	return nil, nil
}

func TestCredentialsPreflight(t *testing.T) {
	fleetPath := credentialFleet(t, map[string]string{"registry": "creds/credentials.json"})
	runWith := func(connector credforge.Connector, args ...string) (string, error) {
		cmd := New(Options{Fleet: fleetPath, CredentialConnector: func(*targets.File) credforge.Connector { return connector }})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--fleet", fleetPath}, args...))
		err := cmd.Execute()
		return out.String(), err
	}

	out, err := runWith(emptyConnector{}, "credentials", "preflight", "example-app", "prod")
	if err == nil || !strings.Contains(out, "missing   example-app/prod/SESSION_KEY") {
		t.Errorf("missing: %v\n%s", err, out)
	}

	out, err = runWith(heldConnector{}, "credentials", "preflight", "example-app", "prod", "--require-recorded")
	if err == nil || !strings.Contains(out, "present   unrecorded example-app/prod/SESSION_KEY (last changed 2026-01-02)") ||
		!strings.Contains(out, "unlisted  /example/prod/example-app/STRAY") {
		t.Errorf("unrecorded: %v\n%s", err, out)
	}

	out, err = runWith(heldConnector{}, "credentials", "preflight", "example-app", "prod", "--record", "--require-recorded")
	if err != nil || !strings.Contains(out, "recorded 1 event(s)") {
		t.Fatalf("record: %v\n%s", err, out)
	}
	out, err = runWith(heldConnector{}, "credentials", "preflight", "example-app", "prod", "--require-recorded")
	if err != nil || !strings.Contains(out, "present   recorded   example-app/prod/SESSION_KEY") {
		t.Errorf("after record: %v\n%s", err, out)
	}
}
