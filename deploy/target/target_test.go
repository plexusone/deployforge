package target

import (
	"slices"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
)

func load(t *testing.T) *fleet.Fleet {
	t.Helper()
	f, err := fleet.Load("../../fleet/testdata/fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestResolveNamesEverythingAStackHasOnTheHost(t *testing.T) {
	got, err := Resolve(load(t), "example-app", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if got.App != "example-app" || got.Stack != "staging" || got.Host != "web-1" || got.Unit != "example-app-staging" ||
		got.User != "exampleappstaging" || got.Listen != "127.0.0.1:8102" || got.SecretPrefix != "/example/staging/example-app/" ||
		!slices.Equal(got.HealthHosts, []string{"stage-app.example.com"}) {
		t.Errorf("got %+v", got)
	}
}

func TestResolveRefusesWhatTheFleetDoesNotHave(t *testing.T) {
	f := load(t)
	for _, c := range [][2]string{{"other", "prod"}, {"example-app", "qa"}} {
		if _, err := Resolve(f, c[0], c[1]); err == nil {
			t.Errorf("%s/%s: resolved", c[0], c[1])
		}
	}
}

func TestValidateRefusesValuesThatAreNotPlainNames(t *testing.T) {
	good, err := Resolve(load(t), "example-app", "prod")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Target){
		"a unit with a slash":     func(t *Target) { t.Unit = "../etc" },
		"a stack with a slash":    func(t *Target) { t.Stack = "a/b" },
		"a user with a space":     func(t *Target) { t.User = "root x" },
		"a public listen address": func(t *Target) { t.Listen = "0.0.0.0:8101" },
		"a privileged port":       func(t *Target) { t.Listen = "127.0.0.1:22" },
		"a host with a header":    func(t *Target) { t.HealthHosts = []string{"a.example.com\r\nX: y"} },
		"no health hosts":         func(t *Target) { t.HealthHosts = nil },
		"a prefix without slash":  func(t *Target) { t.SecretPrefix = "/example/prod" },
	} {
		bad := good
		bad.HealthHosts = slices.Clone(good.HealthHosts)
		mutate(&bad)
		if err := bad.Validate(); err == nil || !strings.HasPrefix(err.Error(), "target:") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
