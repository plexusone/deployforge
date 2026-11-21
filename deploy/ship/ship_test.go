package ship

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A release script that records where it ran and what it was given, and
// makes one archive.
const script = `#!/bin/sh
set -eu
[ "$1" = --out ] && out="$2"
mkdir -p "$out"
echo "$(cat VERSION) $DEPLOYFORGE $DEPLOYFORGE_FLEET" > "$out/site-0123abc.tgz"
`

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // G204: the test's own git commands
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBuildPackagesTheTagInAWorktree(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "--quiet")
	run(t, repo, "git", "config", "commit.gpgsign", "false")
	run(t, repo, "git", "config", "tag.gpgsign", "false")
	if err := os.MkdirAll(filepath.Join(repo, "ops/host"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ReleaseScript), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the script must run
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "VERSION"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "add", ".")
	run(t, repo, "git", "commit", "--quiet", "-m", "one")
	run(t, repo, "git", "tag", "v1.0.0")
	tagged := run(t, repo, "git", "rev-parse", "HEAD")
	// The operator's checkout has moved on, with uncommitted work.
	if err := os.WriteFile(filepath.Join(repo, "VERSION"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "commit", "--quiet", "-am", "two")
	if err := os.WriteFile(filepath.Join(repo, "VERSION"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := Build(context.Background(), Options{App: "site", Repo: repo, Tag: "v1.0.0", Deployforge: "/bin/deployforge", Fleet: "/fleet.json"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p.Archive)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one /bin/deployforge /fleet.json\n" || p.Commit != tagged {
		t.Errorf("archive %q, commit %s, want the tag's %s", got, p.Commit, tagged)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Archive); !os.IsNotExist(err) {
		t.Errorf("the archive is left: %v", err)
	}
	if wt := run(t, repo, "git", "worktree", "list"); strings.Count(wt, "\n") != 0 {
		t.Errorf("a worktree is left: %s", wt)
	}
	if v, err := os.ReadFile(filepath.Join(repo, "VERSION")); err != nil || string(v) != "dirty" { //nolint:gosec // G304: the test's repository
		t.Errorf("the checkout changed: %q %v", v, err)
	}

	for _, o := range []Options{
		{App: "site", Repo: repo, Tag: "v9.9.9", Deployforge: "/bin/deployforge", Fleet: "/fleet.json"},
		{App: "site", Repo: repo, Tag: "main", Deployforge: "/bin/deployforge", Fleet: "/fleet.json"},
		{App: "site", Repo: repo, Tag: "v1.0.0", Deployforge: "bin/deployforge", Fleet: "/fleet.json"},
	} {
		if _, err := Build(context.Background(), o); err == nil {
			t.Errorf("%+v was packaged", o)
		}
	}
}

// A release script that writes what it was given, the GOWORK it ran with,
// and that go.work, as its one archive.
const recordingScript = `#!/bin/sh
set -eu
args="$*"
out=""
while [ $# -gt 0 ]; do
  case "$1" in --out) out="$2"; shift 2 ;; *) shift ;; esac
done
mkdir -p "$out"
{
  echo "version $(cat VERSION)"
  echo "args $args"
  echo "gowork $GOWORK"
  if [ "$GOWORK" != off ]; then cat "$GOWORK"; fi
} > "$out/site-0123abc.tgz"
`

// gitRepo makes a repository with files committed on main and origin set to
// the URL given.
func gitRepo(t *testing.T, origin string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "--quiet", "--initial-branch=main")
	run(t, dir, "git", "config", "commit.gpgsign", "false")
	run(t, dir, "git", "config", "tag.gpgsign", "false")
	run(t, dir, "git", "remote", "add", "origin", origin)
	for name, body := range files {
		mode := os.FileMode(0o600)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o700
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "--quiet", "-m", "one")
	return dir
}

func siteRepo(t *testing.T) string {
	return gitRepo(t, "https://github.com/example/site.git", map[string]string{
		ReleaseScript: recordingScript,
		"VERSION":     "tagged",
		"go.mod":      "module example.com/site\n\ngo 1.25\n\nrequire example.com/lib v1.0.0\n",
	})
}

func libRepo(t *testing.T, origin string) string {
	return gitRepo(t, origin, map[string]string{"go.mod": "module example.com/lib\n\ngo 1.26.5\n"})
}

func archiveText(t *testing.T, p *Package) string {
	t.Helper()
	b, err := os.ReadFile(p.Archive)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestATagIsBuiltWithGOWORKOff(t *testing.T) {
	repo := siteRepo(t)
	run(t, repo, "git", "tag", "v1.0.0")
	// An operator's go.work must not choose a tagged release's inputs.
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "go.work"))
	p, err := Build(context.Background(), Options{App: "site", Repo: repo, Tag: "v1.0.0", Deployforge: "/bin/deployforge", Fleet: "/fleet.json"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	if got := archiveText(t, p); !strings.Contains(got, "gowork off\n") || strings.Contains(got, "--input") || strings.Contains(got, "--allow-untagged") {
		t.Errorf("archive:\n%s", got)
	}
}

func TestALocalBuildUsesLocalCommitsThroughAGeneratedWorkspace(t *testing.T) {
	repo, lib := siteRepo(t), libRepo(t, "git@github.com:example/lib.git")
	run(t, repo, "git", "tag", "v1.0.0")
	// Local main has moved on from the tag, and the checkout has uncommitted work.
	if err := os.WriteFile(filepath.Join(repo, "VERSION"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "git", "commit", "--quiet", "-am", "two")
	head := run(t, repo, "git", "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "VERSION"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	libHead := run(t, lib, "git", "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(lib, "dirty.go"), []byte("package lib\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := Build(context.Background(), Options{App: "site", Repo: repo, Local: true, Deployforge: "/bin/deployforge", Fleet: "/fleet.json",
		Upstreams: []Upstream{{Repo: "github.com/example/lib", Dir: lib}}})
	if err != nil {
		t.Fatal(err)
	}
	got := archiveText(t, p)
	for _, want := range []string{
		"version main\n", // the commit on main, not the tag or the working tree
		"--allow-untagged",
		"--input github.com/example/lib@" + libHead,
		"go 1.26.5\n", // the newest Go version of the modules used
		"\t./site\n",
		"\t./upstream/github.com_example_lib\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("archive lacks %q:\n%s", want, got)
		}
	}
	if p.Commit != head || len(p.Inputs) != 1 || p.Inputs[0].Commit != libHead {
		t.Errorf("commit %s, inputs %+v", p.Commit, p.Inputs)
	}
	upstream := filepath.Join(p.dir, "upstream", "github.com_example_lib")
	if _, err := os.Stat(filepath.Join(upstream, "dirty.go")); !os.IsNotExist(err) {
		t.Errorf("the upstream's uncommitted file reached the build: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{repo, lib} {
		if wt := run(t, r, "git", "worktree", "list"); strings.Count(wt, "\n") != 0 {
			t.Errorf("a worktree is left in %s: %s", r, wt)
		}
	}
}

func TestALocalBuildRefusesWhatItCannotStandBehind(t *testing.T) {
	repo := siteRepo(t)
	lib := libRepo(t, "https://github.com/example/lib.git")
	other := gitRepo(t, "https://github.com/example/other.git", map[string]string{"go.mod": "module example.com/other\n\ngo 1.25\n"})
	wrongOrigin := libRepo(t, "https://github.com/someone/lib.git")
	base := Options{App: "site", Repo: repo, Local: true, Deployforge: "/bin/deployforge", Fleet: "/fleet.json"}
	cases := map[string]func(o *Options){
		"a tag as well": func(o *Options) { o.Tag = "v1.0.0" },
		"upstreams without --local": func(o *Options) {
			o.Local = false
			o.Tag = "v1.0.0"
			o.Upstreams = []Upstream{{Repo: "github.com/example/lib", Dir: lib}}
		},
		"a module the site lacks":    func(o *Options) { o.Upstreams = []Upstream{{Repo: "github.com/example/other", Dir: other}} },
		"a checkout of another repo": func(o *Options) { o.Upstreams = []Upstream{{Repo: "github.com/example/lib", Dir: wrongOrigin}} },
		"an upstream twice": func(o *Options) {
			o.Upstreams = []Upstream{{Repo: "github.com/example/lib", Dir: lib}, {Repo: "github.com/example/lib", Dir: lib}}
		},
		"a ref that does not exist": func(o *Options) { o.Ref = "nowhere" },
	}
	for name, change := range cases {
		o := base
		change(&o)
		p, err := Build(context.Background(), o)
		if err == nil {
			t.Errorf("%s was packaged", name)
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		}
	}
	for _, r := range []string{repo, lib, other, wrongOrigin} {
		if wt := run(t, r, "git", "worktree", "list"); strings.Count(wt, "\n") != 0 {
			t.Errorf("a refused build left a worktree in %s: %s", r, wt)
		}
	}
}

func TestTheReleaseScriptCanBeNamedButStaysInTheRepository(t *testing.T) {
	repo := siteRepo(t)
	run(t, repo, "git", "tag", "v1.0.0")
	for _, script := range []string{"/etc/passwd", "../outside.sh"} {
		_, err := Build(context.Background(), Options{App: "site", Repo: repo, Tag: "v1.0.0", Deployforge: "/bin/deployforge", Fleet: "/fleet.json", Script: script})
		if err == nil || !strings.Contains(err.Error(), "inside the repository") {
			t.Errorf("%s: %v", script, err)
		}
	}
	_, err := Build(context.Background(), Options{App: "site", Repo: repo, Tag: "v1.0.0", Deployforge: "/bin/deployforge", Fleet: "/fleet.json", Script: "scripts/missing.sh"})
	if err == nil || !strings.Contains(err.Error(), "has no scripts/missing.sh") {
		t.Errorf("a missing script: %v", err)
	}
}
