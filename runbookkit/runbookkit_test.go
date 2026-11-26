package runbookkit_test

import (
	"regexp"
	"strings"
	"testing"

	rb "github.com/plexusone/systemspec-runbook/runbookspec"

	"github.com/plexusone/deployforge/runbookkit"
)

func TestRemoteQuote(t *testing.T) {
	for in, want := range map[string]string{
		"runs the service":         "'runs the service'",
		"it's":                     `'it'\''s'`,
		"--note":                   "--note",
		"/etc/example/config.yaml": "/etc/example/config.yaml",
		"--uid=exampleapp":         "--uid=exampleapp",
		"{{account}}":              "{{account}}",
		"":                         "''",
		"a;b":                      "'a;b'",
	} {
		if got := runbookkit.RemoteQuote(in); got != want {
			t.Errorf("RemoteQuote(%q) = %q; want %q", in, got, want)
		}
	}
	got := runbookkit.SSH([]string{"ssh", "ubuntu@{{address}}"}, "sudo", "app", "--note", "runs the service")
	if strings.Join(got, " ") != "ssh ubuntu@{{address}} sudo app --note 'runs the service'" {
		t.Errorf("SSH = %q", got)
	}
}

func TestInstallFromConsole(t *testing.T) {
	env := map[string]string{"AWS_PROFILE": "{{profile}}"}
	a := runbookkit.InstallFromConsole([]string{"deployforge"}, env, "app/prod/CLIENT_SECRET", "app/prod/CLIENT_ID")
	if len(a.Exec) != 2 || strings.Join(a.Exec[1].Args, " ") != "deployforge credentials install app/prod/CLIENT_ID" || a.Exec[0].Env["AWS_PROFILE"] != "{{profile}}" {
		t.Errorf("action %+v", a)
	}
}

func TestPermissionList(t *testing.T) {
	got := runbookkit.PermissionList([]string{"Zone · Zone WAF · Edit", "Zone · Zone · Read", "Account · Workers Scripts · Edit", "Zone · Zone Settings · Edit", "Zone · SSL and Certificates · Edit"}, "  ")
	want := "  - Account · Workers Scripts · Edit\n  - Zone · SSL and Certificates · Edit\n  - Zone · Zone · Read\n  - Zone · Zone Settings · Edit\n  - Zone · Zone WAF · Edit"
	if got != want {
		t.Errorf("PermissionList:\n%s\nwant\n%s", got, want)
	}
}

func TestAddNeeds(t *testing.T) {
	book := &rb.Runbook{Phases: []rb.Phase{{ID: "1", Steps: []rb.Step{
		{ID: "1.1", Actions: []rb.Action{{Exec: []rb.Exec{{Args: []string{"ssh", "-i", "{{sshKey}}", "host", "true"}}}}}},
		{ID: "1.2", Actions: []rb.Action{{Exec: []rb.Exec{{Args: []string{"tool", "zones", "harden", "apply"}}}}}},
		{ID: "1.3", Actions: []rb.Action{{Exec: []rb.Exec{{Args: []string{"tool", "zones", "verify"}}}}}},
	}}}}
	runbookkit.AddNeeds(book,
		runbookkit.NeedRule{Input: "agent-key", Match: runbookkit.UsesVariable("sshKey")},
		runbookkit.NeedRule{Input: "API_TOKEN", Match: runbookkit.Runs("tool", "zones", "harden")},
	)
	steps := book.Steps()
	if strings.Join(steps[0].Needs, ",") != "agent-key" || strings.Join(steps[1].Needs, ",") != "API_TOKEN" || len(steps[2].Needs) != 0 {
		t.Errorf("needs %v %v %v", steps[0].Needs, steps[1].Needs, steps[2].Needs)
	}
	runbookkit.AddNeeds(book, runbookkit.NeedRule{Input: "agent-key", Match: runbookkit.UsesVariable("sshKey")})
	if len(steps[0].Needs) != 1 {
		t.Error("a need was added twice")
	}
}

func TestInputs(t *testing.T) {
	k := runbookkit.AgentKey("agent-key", "SHA256:abc", "~/.ssh/deploy")
	if k.Check == nil || k.Check.Contains != "SHA256:abc" || !strings.Contains(k.HowTo, "ssh-add ~/.ssh/deploy") {
		t.Errorf("AgentKey %+v", k)
	}
	if tok := runbookkit.Token("API_TOKEN", "API_TOKEN", "the token", "make it"); !tok.Secret || tok.Env != "API_TOKEN" {
		t.Errorf("Token %+v", tok)
	}
}

// TestFixes checks that each failure message matches exactly one fix, and
// that every pattern compiles.
func TestFixes(t *testing.T) {
	fixes := runbookkit.Fixes(runbookkit.FixOptions{Tool: "deployforge runbook", KeyPath: "~/.ssh/deploy", TokenEnv: "API_TOKEN"})
	for _, msg := range []string{
		"ssh: connect to host 192.0.2.1 port 22: Operation timed out",
		"ubuntu@192.0.2.1: Permission denied (publickey).",
		"fork/exec /src/app/ops/release.sh: permission denied",
		"runbook: no value for sshKey; give each with --var",
		"credentials preflight: 2 credential(s) missing",
		"github: no token: set GITHUB_TOKEN or GH_TOKEN",
		"zoneharden: set API_TOKEN",
	} {
		n := 0
		for _, f := range fixes {
			if regexp.MustCompile(f.Match).MatchString(msg) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q matches %d fixes", msg, n)
		}
	}
	if len(runbookkit.Fixes(runbookkit.FixOptions{})) != len(fixes)-1 {
		t.Error("the token fix is there without TokenEnv")
	}
}
