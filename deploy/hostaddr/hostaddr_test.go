package hostaddr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
)

func TestResolve(t *testing.T) {
	given := fleet.Host{Name: "web-1", Address: "203.0.113.10"}
	if h, err := Resolve(context.Background(), given, nil); err != nil || h.Address != "203.0.113.10" {
		t.Errorf("given: %+v, %v", h, err)
	}
	src := fleet.Host{Name: "web-1", AddressFrom: &fleet.AddressSource{Pulumi: &fleet.PulumiOutput{Dir: "stack", Stack: "prod", Output: "staticIp"}}}
	var called []string
	run := func(_ context.Context, dir string, argv ...string) ([]byte, error) {
		called = append([]string{dir}, argv...)
		return []byte("203.0.113.20\n"), nil
	}
	h, err := Resolve(context.Background(), src, run)
	if err != nil || h.Address != "203.0.113.20" || h.AddressFrom != nil {
		t.Errorf("pulumi: %+v, %v", h, err)
	}
	if got := strings.Join(called, " "); got != "stack pulumi stack output staticIp --stack prod --non-interactive" {
		t.Errorf("ran %s", got)
	}
	for name, out := range map[string]string{"a shell word": "1.2.3.4; rm -rf /", "empty": ""} {
		bad := func(context.Context, string, ...string) ([]byte, error) { return []byte(out), nil }
		if _, err := Resolve(context.Background(), src, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	fail := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no stack") }
	if _, err := Resolve(context.Background(), src, fail); err == nil || !strings.Contains(err.Error(), "no stack") {
		t.Errorf("a failing pulumi: %v", err)
	}
}

func TestConfig(t *testing.T) {
	var argv []string
	run := func(_ context.Context, _ string, a ...string) ([]byte, error) {
		argv = a
		return []byte("s3cr3t\n"), nil
	}
	v, err := Config(context.Background(), fleet.PulumiConfig{Dir: "stack", Stack: "prod", Key: "fleet:dbMasterPassword"}, run)
	if err != nil || v != "s3cr3t" || strings.Join(argv, " ") != "pulumi config get fleet:dbMasterPassword --stack prod --non-interactive" {
		t.Errorf("Config = %q, %v, ran %v", v, err, argv)
	}
	empty := func(context.Context, string, ...string) ([]byte, error) { return []byte("\n"), nil }
	if _, err := Config(context.Background(), fleet.PulumiConfig{Dir: "stack", Stack: "prod", Key: "k"}, empty); err == nil {
		t.Error("an empty value")
	}
}
