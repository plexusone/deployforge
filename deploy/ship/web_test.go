package ship

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePnpm puts a pnpm on PATH that logs each call as "<dir> <args>" and
// does the part of each call the build relies on: pack writes a tarball
// named for the package's directory, and install --lockfile-only changes
// the lockfile.
func fakePnpm(t *testing.T) (logFile string) {
	t.Helper()
	bin := t.TempDir()
	logFile = filepath.Join(t.TempDir(), "pnpm.log")
	script := `#!/bin/sh
set -eu
echo "$PWD $*" >> "` + logFile + `"
case "$*" in
  "pack --pack-destination "*) touch "$3/pkg-$(basename "$PWD").tgz" ;;
  "install --lockfile-only") echo "# relocked" >> pnpm-lock.yaml ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "pnpm"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake must run
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

// A release script that writes the front end's workspace file, its
// lockfile and git's view of the worktree as the archive.
const webScript = `#!/bin/sh
set -eu
out=""
while [ $# -gt 0 ]; do
  case "$1" in --out) out="$2"; shift 2 ;; *) shift ;; esac
done
mkdir -p "$out"
{
  echo "status [$(git status --porcelain --untracked-files=all)]"
  cat web/pnpm-workspace.yaml
  cat web/pnpm-lock.yaml
} > "$out/site-0123abc.tgz"
`

func webSite(t *testing.T) string {
	return gitRepo(t, "https://github.com/example/site.git", map[string]string{
		ReleaseScript:             webScript,
		"go.mod":                  "module example.com/site\n\ngo 1.25\n",
		"web/package.json":        `{"name":"site-web","dependencies":{"@ex/ui":"^1.0.0","left-pad":"1.3.0"}}`,
		"web/pnpm-lock.yaml":      "lockfileVersion: '9.0'\n",
		"web/pnpm-workspace.yaml": "onlyBuiltDependencies:\n  - esbuild\n",
		"tools/package.json":      `{"name":"tools","dependencies":{"@ex/other":"1"}}`, // no lockfile: not a front end
	})
}

func uiRepo(t *testing.T, origin string) string {
	return gitRepo(t, origin, map[string]string{
		"package.json":                `{"name":"ui-root","private":true}`,
		"pnpm-lock.yaml":              "lockfileVersion: '9.0'\n",
		"packages/ui/package.json":    `{"name":"@ex/ui","scripts":{"build":"tsup"}}`,
		"packages/other/package.json": `{"name":"@ex/other","scripts":{"build":"tsup"}}`,
		"packages/app/package.json":   `{"name":"@ex/app","scripts":{"prepare":"tsup"}}`,
	})
}

func TestALocalBuildPacksTheWebPackagesTheFrontEndUses(t *testing.T) {
	log := fakePnpm(t)
	repo, ui := webSite(t), uiRepo(t, "https://github.com/example/ui.git")
	uiHead := run(t, ui, "git", "rev-parse", "HEAD")
	p, err := Build(context.Background(), Options{App: "site", Repo: repo, Local: true, Deployforge: "/bin/deployforge", Fleet: "/fleet.json",
		WebUpstreams: []Upstream{{Repo: "github.com/example/ui", Dir: ui}}})
	if err != nil {
		t.Fatal(err)
	}
	got := archiveText(t, p)
	packs := filepath.Join(p.dir, "packs")
	for _, want := range []string{
		"status []\n", // the release script's clean-checkout check passes
		"onlyBuiltDependencies:\n  - esbuild\n",
		"overrides:\n  '@ex/ui': 'file:" + filepath.Join(packs, "pkg-ui.tgz") + "'\n",
		"# relocked\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("archive lacks %q:\n%s", want, got)
		}
	}
	// Only what the front end uses is packed; a package the site's tools use
	// without a front end is not.
	if strings.Contains(got, "@ex/other") || strings.Contains(got, "@ex/app") {
		t.Errorf("packed a package the front end does not use:\n%s", got)
	}
	if len(p.Inputs) != 1 || p.Inputs[0].Repo != "github.com/example/ui" || p.Inputs[0].Commit != uiHead {
		t.Errorf("inputs = %+v", p.Inputs)
	}
	calls, err := os.ReadFile(log) //nolint:gosec // G304: the fake's own log
	if err != nil {
		t.Fatal(err)
	}
	upstream := filepath.Join(p.dir, "web", "github.com_example_ui")
	for _, want := range []string{
		upstream + " install --frozen-lockfile\n",
		upstream + " --filter @ex/ui... run --if-present build\n", // no prepare script, so built with its dependencies
		filepath.Join(upstream, "packages", "ui") + " pack --pack-destination " + packs + "\n",
		filepath.Join(p.tree, "web") + " install --lockfile-only\n",
	} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("pnpm was not called as %q:\n%s", want, calls)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	// The operator's checkouts are untouched.
	for _, r := range []string{repo, ui} {
		if st := run(t, r, "git", "status", "--porcelain"); st != "" {
			t.Errorf("%s changed: %s", r, st)
		}
	}
}

func TestAWebUpstreamMustProvideWhatTheFrontEndUses(t *testing.T) {
	fakePnpm(t)
	repo := webSite(t)
	other := gitRepo(t, "https://github.com/example/other.git", map[string]string{
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
		"package.json":   `{"name":"@ex/unused"}`,
	})
	if p, err := Build(context.Background(), Options{App: "site", Repo: repo, Local: true, Deployforge: "/bin/deployforge", Fleet: "/fleet.json",
		WebUpstreams: []Upstream{{Repo: "github.com/example/other", Dir: other}}}); err == nil || !strings.Contains(err.Error(), "provides no package") {
		t.Errorf("an upstream with nothing the site uses: %v", err)
		if p != nil {
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		}
	}
}

// pushedRepo is a checkout whose origin names a GitHub repository but
// fetches from a local bare repository, with main pushed and one more
// commit on local main only. It returns the checkout and the pushed commit.
func pushedRepo(t *testing.T, githubURL string, files map[string]string) (dir, pushed string) {
	t.Helper()
	dir = gitRepo(t, githubURL, files)
	bare := filepath.Join(t.TempDir(), "origin.git")
	run(t, t.TempDir(), "git", "init", "--quiet", "--bare", bare)
	run(t, dir, "git", "config", "url."+bare+".insteadOf", githubURL)
	run(t, dir, "git", "push", "--quiet", "origin", "main")
	pushed = run(t, dir, "git", "rev-parse", "HEAD")
	run(t, dir, "git", "commit", "--quiet", "--allow-empty", "-m", "local only")
	return dir, pushed
}

func TestALocalBuildNeedsItsCommitsOnARemote(t *testing.T) {
	fakePnpm(t)
	repo, sitePushed := pushedRepo(t, "https://github.com/example/site.git", map[string]string{
		ReleaseScript: recordingScript, "VERSION": "v", "go.mod": "module example.com/site\n\ngo 1.25\n\nrequire example.com/lib v1.0.0\n",
	})
	lib, _ := pushedRepo(t, "https://github.com/example/lib.git", map[string]string{"go.mod": "module example.com/lib\n\ngo 1.25\n"})
	base := Options{App: "site", Repo: repo, Local: true, Deployforge: "/bin/deployforge", Fleet: "/fleet.json", RequirePushed: true,
		Upstreams: []Upstream{{Repo: "github.com/example/lib", Dir: lib}}}

	// Local main of both is ahead of GitHub: refused, naming both.
	if _, err := Build(context.Background(), base); err == nil || !strings.Contains(err.Error(), "the app at") || !strings.Contains(err.Error(), "github.com/example/lib at") {
		t.Fatalf("unpushed commits: %v", err)
	}
	// The site's pushed commit, and an upstream the operator allows unpushed.
	o := base
	o.Ref = sitePushed
	o.AllowUnpushed = map[string]bool{"github.com/example/lib": true}
	p, err := Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	// Pushed to any branch is enough, without waiting for anything else.
	run(t, lib, "git", "push", "--quiet", "origin", "main:feature")
	o.AllowUnpushed = nil
	if p, err = Build(context.Background(), o); err != nil {
		t.Fatalf("an upstream pushed to a feature branch: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
