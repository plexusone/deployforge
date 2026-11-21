package systemd

import (
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
)

func testFleet(state bool) *fleet.Fleet {
	return &fleet.Fleet{
		FormatVersion: fleet.FormatVersion,
		PortRange:     fleet.PortRange{Min: 8101, Max: 8199},
		Hosts:         []fleet.Host{{Name: "web-1", Address: "203.0.113.10"}},
		Apps: []fleet.App{{
			Name: "one-cloud", Repo: "github.com/example/one-cloud", Domain: "one.example.com",
			SecretPrefix: "/example/{stack}/one-cloud/", StateDirectory: state,
			Stacks: map[string]fleet.Stack{
				"prod": {Host: "web-1", Hosts: []string{"one.example.com"}, Port: 8101, Status: fleet.Live,
					Limits: &fleet.Limits{MemoryMax: "768M", CPUQuota: "100%", ConnectionLimit: 20}},
				"staging": {Host: "web-1", Hosts: []string{"stage-app.one.example.com"}, Port: 8102, Status: fleet.Planned,
					Limits: &fleet.Limits{MemoryMax: "256M", CPUQuota: "25%", ConnectionLimit: 5}},
			},
		}},
	}
}

func TestEachStackGetsItsOwnUserPathsAndLimits(t *testing.T) {
	for _, tc := range []struct {
		stack string
		want  []string
	}{
		{"prod", []string{
			"User=onecloud", "Group=onecloud", "EnvironmentFile=/etc/one-cloud/env",
			"ExecStartPre=/opt/one-cloud/current/one-cloud validate -c /etc/one-cloud/config.yaml",
			"ExecStart=/opt/one-cloud/current/one-cloud serve -c /etc/one-cloud/config.yaml",
			"MemoryMax=768M", "CPUQuota=100%", "ReadOnlyPaths=/opt/one-cloud /etc/one-cloud",
		}},
		{"staging", []string{
			"User=onecloudstaging", "EnvironmentFile=/etc/one-cloud-staging/env",
			"ExecStart=/opt/one-cloud-staging/current/one-cloud serve -c /etc/one-cloud-staging/config.yaml",
			"MemoryMax=256M", "CPUQuota=25%", "ReadOnlyPaths=/opt/one-cloud-staging /etc/one-cloud-staging",
		}},
	} {
		out, err := Render(testFleet(false), "one-cloud", tc.stack)
		if err != nil {
			t.Fatalf("%s: %v", tc.stack, err)
		}
		got := string(out)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: unit lacks %q:\n%s", tc.stack, want, got)
			}
		}
		// Staging never reads prod's files or runs prod's binary directory.
		if tc.stack == "staging" && strings.Contains(got, "/opt/one-cloud/") {
			t.Errorf("staging refers to prod's directory:\n%s", got)
		}
	}
}

func TestTheSandboxIsTheHostingConventions(t *testing.T) {
	out, err := Render(testFleet(false), "one-cloud", "prod")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"NoNewPrivileges=true", "ProtectSystem=strict", "ProtectHome=true", "PrivateTmp=true", "PrivateDevices=true",
		"ProtectClock=true", "ProtectKernelTunables=true", "ProtectKernelModules=true", "ProtectKernelLogs=true",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX", "RestrictNamespaces=true", "MemoryDenyWriteExecute=true",
		"SystemCallFilter=@system-service", "CapabilityBoundingSet=\n", "AmbientCapabilities=\n",
		"LogsDirectory=one-cloud\n", "LogsDirectoryMode=0700",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the sandbox lacks %q", want)
		}
	}
}

func TestOnlyAnAppThatKeepsStateGetsAStateDirectory(t *testing.T) {
	without, err := Render(testFleet(false), "one-cloud", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "StateDirectory") {
		t.Errorf("a stateless app got a state directory:\n%s", without)
	}
	with, err := Render(testFleet(true), "one-cloud", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with), "StateDirectory=one-cloud-staging\n") || !strings.Contains(string(with), "StateDirectoryMode=0700") {
		t.Errorf("an app that keeps state lacks its own state directory:\n%s", with)
	}
}

func TestRenderRefusesWhatWouldMakeAnUnsafeUnit(t *testing.T) {
	f := testFleet(false)
	prod := f.Apps[0].Stacks["prod"]
	prod.Limits = nil
	f.Apps[0].Stacks["prod"] = prod
	for name, tc := range map[string]struct {
		f          *fleet.Fleet
		app, stack string
		want       string
	}{
		"no limits":     {f, "one-cloud", "prod", "no limits"},
		"unknown app":   {testFleet(false), "other", "prod", `no app "other"`},
		"unknown stack": {testFleet(false), "one-cloud", "qa", `no stack "qa"`},
	} {
		if _, err := Render(tc.f, tc.app, tc.stack); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tc.want)
		}
	}
}

func TestTheSelfChecksSandboxIsTheServices(t *testing.T) {
	for _, state := range []bool{false, true} {
		testSandboxMatches(t, testFleet(state))
	}
}

func testSandboxMatches(t *testing.T, f *fleet.Fleet) {
	t.Helper()
	for _, stack := range []string{"prod", "staging"} {
		unit, err := Render(f, "one-cloud", stack)
		if err != nil {
			t.Fatal(err)
		}
		props, err := Sandbox(f, "one-cloud", stack)
		if err != nil {
			t.Fatal(err)
		}
		lines := map[string]bool{}
		for _, l := range strings.Split(string(unit), "\n") {
			lines[l] = true
		}
		// Every property the self-check runs with is a line of the unit, and
		// every hardening line of the unit is a property: neither can drift.
		for _, p := range props {
			if !lines[p] {
				t.Errorf("%s: the self-check runs with %q, which the unit does not", stack, p)
			}
		}
		for _, h := range hardening {
			found := false
			for _, p := range props {
				found = found || p == h
			}
			if !found {
				t.Errorf("%s: the unit's %q is missing from the self-check's sandbox", stack, h)
			}
		}
	}
	if _, err := Sandbox(f, "nowhere", "prod"); err == nil {
		t.Error("an unknown app had a sandbox")
	}
}
