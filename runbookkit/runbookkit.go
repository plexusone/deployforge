// Package runbookkit holds the pieces runbooks for deploying to your own
// servers keep needing, built on systemspec-runbook's types: commands
// that reach a host over ssh, actions that install credentials from an
// issuer's console, the inputs a run checks before it starts, which steps
// need them, and fixes for the failures such runs meet.
//
// A tool's own runbook content, such as its phases and its host's key
// fingerprint, stays with the tool; everything here takes those as
// parameters.
package runbookkit

import (
	"slices"
	"strings"

	rb "github.com/plexusone/systemspec-runbook/runbookspec"
)

// RemoteQuote quotes an argument for the remote shell. ssh joins a remote
// command's arguments with spaces and the remote shell splits them again,
// so an argument with a space, such as a note, would arrive as several.
// One with no character the shell treats specially is left as it is, so a
// page shows commands as they would be typed; so is a lone {{variable}},
// whose value is filled in later.
func RemoteQuote(a string) string {
	if a == "" {
		return "''"
	}
	if !strings.ContainsAny(a, " \t\n'\"\\$`|&;<>()*?[]#~!{}") ||
		strings.HasPrefix(a, "{{") && strings.HasSuffix(a, "}}") && !strings.ContainsAny(a, " \t\n'\"") {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// SSH is a command that runs remote on a host: prefix is ssh and its
// arguments up to and including the destination, such as
// ssh -i {{sshKey}} ubuntu@{{address}}, and each remote argument is quoted
// for the remote shell.
func SSH(prefix []string, remote ...string) []string {
	out := slices.Clone(prefix)
	for _, a := range remote {
		out = append(out, RemoteQuote(a))
	}
	return out
}

// InstallFromConsole is the action that stores values an issuer's console
// shows, such as a sign-in client's ID and secret: one credentials install
// for each registry ID, run as tool (such as deployforge, or a tool
// embedding it) with env. Each install asks for the value hidden, checks
// it where it can, writes it where the registry says and records it; no
// value is an argument, shown or kept in a shell's history.
func InstallFromConsole(tool []string, env map[string]string, ids ...string) rb.Action {
	var ex []rb.Exec
	for _, id := range ids {
		args := append(slices.Clone(tool), "credentials", "install", id)
		ex = append(ex, rb.Exec{Args: args, Env: env})
	}
	return rb.Action{
		Text: "Install each value straight from the console: it is asked for hidden, written where the registry says and recorded; nothing is shown.",
		Exec: ex,
	}
}

// PermissionList is a Markdown list of permissions, one per line, indented
// by indent, sorted part by part (group · permission · access) without
// regard to case, so "Zone" comes before "Zone Settings" and each can be
// checked off against a console's form.
func PermissionList(perms []string, indent string) string {
	sorted := slices.Clone(perms)
	parts := func(p string) []string { return strings.Split(strings.ToLower(p), " · ") }
	slices.SortFunc(sorted, func(a, b string) int { return slices.Compare(parts(a), parts(b)) })
	var b strings.Builder
	for i, p := range sorted {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(indent + "- " + p)
	}
	return b.String()
}

// NeedRule names an input a step needs when one of its commands matches.
type NeedRule struct {
	// Input is the input's name.
	Input string
	// Match reports whether a command, its arguments and the arguments of
	// the command its standard input comes from, needs the input.
	Match func(args []string) bool
}

// UsesVariable matches a command that uses {{name}}, such as an SSH key.
func UsesVariable(name string) func([]string) bool {
	v := "{{" + name + "}}"
	return func(args []string) bool {
		return slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, v) })
	}
}

// Runs matches a command whose arguments start with prefix, such as a
// tool's subcommand that calls an API with a token.
func Runs(prefix ...string) func([]string) bool {
	return func(args []string) bool {
		return len(args) >= len(prefix) && slices.Equal(args[:len(prefix)], prefix)
	}
}

// AddNeeds adds to each step the inputs its commands need, by the rules,
// so a run checks them before the step starts and the page links them.
func AddNeeds(book *rb.Runbook, rules ...NeedRule) {
	for _, s := range book.Steps() {
		for _, a := range s.Actions {
			for _, e := range a.Exec {
				args := slices.Clone(e.Args)
				if e.Stdin != nil {
					args = append(args, e.Stdin.From...)
				}
				for _, r := range rules {
					if r.Match(args) && !slices.Contains(s.Needs, r.Input) {
						s.Needs = append(s.Needs, r.Input)
					}
				}
			}
		}
	}
}

// AgentKey is an input checking that the host's SSH key is loaded in the
// SSH agent, by its fingerprint as ssh-add -l lists it, such as
// SHA256:…. Commands that reach a host run ssh with BatchMode, so it
// cannot ask for the key's passphrase.
func AgentKey(name, fingerprint, keyPath string) rb.Input {
	return rb.Input{
		Name:        name,
		Description: "The host's SSH key, loaded in the SSH agent: commands that reach the host run ssh with BatchMode, so it cannot ask for the key's passphrase.",
		Check:       &rb.CommandCheck{Args: []string{"ssh-add", "-l"}, Contains: fingerprint},
		HowTo: "`ssh-add " + keyPath + "`, which asks for its passphrase once; on macOS,\n`ssh-add --apple-use-keychain " +
			keyPath + "` keeps it in the keychain.\n`ssh-add -l` lists the loaded keys.",
	}
}

// Token is a secret input read from an environment variable, such as an
// API token made for one session, with how to make it.
func Token(name, env, description, howTo string) rb.Input {
	return rb.Input{Name: name, Description: description, Env: env, Secret: true, HowTo: howTo}
}

// FixOptions are what the generic fixes point to.
type FixOptions struct {
	// Tool is how the fixes name the runbook command, such as
	// "deployforge runbook"; "runbook" when empty.
	Tool string
	// KeyPath is the host's SSH key, such as ~/.ssh/deploy.
	KeyPath string
	// AdminCIDRs says where the addresses SSH is open to are kept and how
	// they are changed, as Markdown, such as a link to a guide.
	AdminCIDRs string
	// TokenEnv is the environment variable a session's API token is read
	// from, such as CLOUDFLARE_API_TOKEN; no token fix when empty.
	TokenEnv string
}

// Fixes are fixes for failures runs that deploy to your own servers meet:
// SSH timing out or refusing the key, a release script without its
// executable bit, a value not given, credentials missing from the
// parameter store, no GitHub token, and no API token.
func Fixes(o FixOptions) []rb.Fix {
	tool := o.Tool
	if tool == "" {
		tool = "runbook"
	}
	key := o.KeyPath
	if key == "" {
		key = "<key>"
	}
	admin := o.AdminCIDRs
	if admin == "" {
		admin = "the host's firewall"
	}
	fixes := []rb.Fix{
		{Match: `ssh: connect to host .* port 22: Operation timed out`,
			Fix: "Port 22 is open only to the admin addresses, and yours may have changed. Find it with\n" +
				"`curl -s https://checkip.amazonaws.com`, add it as a `/32` to " + admin + ", and check the\n" +
				"firewall afterwards with `host ports`."},
		{Match: `Permission denied \(publickey\)`,
			Fix: "The commands run ssh with BatchMode, so it cannot ask for the key's passphrase: load the key\n" +
				"with `ssh-add " + key + "` (on macOS, `--apple-use-keychain` keeps it), then check `ssh-add -l`."},
		{Match: `fork/exec .*: permission denied`,
			Fix: "The script was committed without its executable bit, so a checkout of the tag cannot run it,\n" +
				"and `chmod` would leave the checkout differing from the tag. In the app's repository,\n" +
				"`git update-index --chmod=+x <script>` for it and each script it calls, commit, and tag again."},
		{Match: `no value for \w+`,
			Fix: "Set each variable named, with `--var name=value` or its environment variable, as Before You\n" +
				"Start lists. `" + tool + " prepare <phase>` lists every one a phase needs."},
		{Match: `credential\(s\) missing`,
			Fix: "A credential the registry expects is not in the parameter store yet: the lines marked\n" +
				"`missing` name it. Run the step that creates it, or list the stack's with\n" +
				"`credentials preflight <app> <stack> --include-planned`."},
		{Match: `github: no token`,
			Fix: "Sign in with the GitHub CLI, `gh auth login`, or set `GITHUB_TOKEN`."},
	}
	if o.TokenEnv != "" {
		fixes = append(fixes, rb.Fix{Match: `set ` + o.TokenEnv,
			Fix: "Export the token in this terminal, as Before You Start says: `export " + o.TokenEnv + "=…`."})
	}
	return fixes
}
