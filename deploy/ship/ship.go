// Package ship packages an app's release without touching the operator's
// checkout: it adds a temporary git worktree at the commit to ship, runs the
// app's release script there, and returns the archive. deployforge deploy
// ship then deploys it and checks each host name.
//
// A release is built either from the app's tag, with GOWORK off so only the
// versions its go.mod requires are used, or, for a local build, from a local
// branch's commit with chosen upstream modules taken from their own local
// commits through a generated go.work.
//
// The release script runs in the worktree with --out <dir>, and for a local
// build --allow-untagged and an --input for each upstream commit, which it
// passes to deployforge release package. Its environment names the
// deployforge binary (DEPLOYFORGE) and the fleet document
// (DEPLOYFORGE_FLEET).
package ship

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/plexusone/deployforge/provenance"
)

// ReleaseScript is where an app's repository keeps its build-and-package
// script, relative to its root, unless Options.Script names another.
const ReleaseScript = "ops/host/release.sh"

var tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// Options name what to package.
type Options struct {
	// App is the app's name, which names its archive.
	App string
	// Repo is the operator's checkout of the app's repository. Only
	// its git objects are used; its working tree is left as it is.
	Repo string
	// Tag is the release tag to package, such as v0.5.7.
	Tag string
	// Deployforge and Fleet are the absolute paths of the deployforge binary
	// and the fleet document the release script packages with.
	Deployforge, Fleet string
	// Script is the release script, relative to the repository's root;
	// ReleaseScript when empty.
	Script string
	// Fetch fetches the tag from origin first.
	Fetch bool
	// Local builds Ref instead of Tag, with Upstreams from their local
	// commits. The release has no tag.
	Local bool
	// Ref is the app's branch or commit a local build ships; default main.
	Ref string
	// Upstreams are the Go modules a local build takes from local commits.
	Upstreams []Upstream
	// WebUpstreams are the repositories whose npm packages a local build
	// packs from local commits for the app's front end.
	WebUpstreams []Upstream
	// RequirePushed refuses a local build from a commit that is on no
	// remote branch of its checkout, after fetching origin, except for the
	// repositories AllowUnpushed names.
	RequirePushed bool
	AllowUnpushed map[string]bool
	// Env is extra environment for the release script, as NAME=value, after
	// DEPLOYFORGE and DEPLOYFORGE_FLEET: for scripts at older tags that read
	// other names for the same paths.
	Env []string
	// Log receives the commands' output.
	Log io.Writer
}

// Upstream is a module a local build takes from a local checkout's commit
// instead of the version the app's go.mod requires.
type Upstream struct {
	// Repo is host/org/repo, which the checkout's origin must name.
	Repo string
	// Dir is the checkout. Only its git objects are used.
	Dir string
	// Ref is the branch or commit to build; default main.
	Ref string
}

// Package is a packaged release in a temporary directory.
type Package struct {
	// Archive is the release archive, beside its .sha256 file.
	Archive string
	// Commit is the app's commit.
	Commit string
	// Inputs are the upstream commits a local build used.
	Inputs []provenance.Input
	dir    string
	repo   string
	tree   string
	trees  []worktree
	// commits is each upstream repository's commit, so it is recorded once.
	commits map[string]string
}

type worktree struct{ repo, path string }

// Close removes the worktree and the temporary directory.
func (p *Package) Close() error {
	var errs []error
	for _, w := range p.trees {
		if out, err := git(w.repo, "worktree", "remove", "--force", w.path); err != nil {
			errs = append(errs, fmt.Errorf("ship: removing the worktree %s: %w: %s", w.path, err, out))
		}
	}
	if err := os.RemoveAll(p.dir); err != nil {
		errs = append(errs, fmt.Errorf("ship: %w", err))
	}
	return errors.Join(errs...)
}

// Build packages o.Tag of o.Repo, or for a local build o.Ref with
// o.Upstreams. The caller closes the Package.
func Build(ctx context.Context, o Options) (_ *Package, err error) {
	if o.Local {
		if o.Tag != "" {
			return nil, errors.New("ship: a local build has no tag; tag the version when its batch is pushed")
		}
		if o.Ref == "" {
			o.Ref = "main"
		}
	} else {
		if !tagPattern.MatchString(o.Tag) {
			return nil, fmt.Errorf("ship: %q is not a release tag such as v1.2.3", o.Tag)
		}
		if len(o.Upstreams)+len(o.WebUpstreams) > 0 {
			return nil, errors.New("ship: upstreams from local commits need a local build")
		}
	}
	if !filepath.IsAbs(o.Deployforge) || !filepath.IsAbs(o.Fleet) {
		return nil, errors.New("ship: the deployforge binary and the fleet document must be absolute paths")
	}
	scriptPath := o.Script
	if scriptPath == "" {
		scriptPath = ReleaseScript
	}
	if filepath.IsAbs(scriptPath) || strings.HasPrefix(filepath.Clean(scriptPath), "..") {
		return nil, fmt.Errorf("ship: the release script %q must be a path inside the repository", scriptPath)
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	ref := o.Tag
	if o.Local {
		ref = o.Ref
	} else if o.Fetch {
		if out, err := git(o.Repo, "fetch", "--quiet", "origin", "tag", o.Tag); err != nil {
			return nil, fmt.Errorf("ship: fetching %s: %w: %s", o.Tag, err, out)
		}
	}
	commit, err := git(o.Repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		if o.Local {
			return nil, fmt.Errorf("ship: %s has no commit %s: %w", o.Repo, ref, err)
		}
		return nil, fmt.Errorf("ship: %s has no tag %s; push it, or fetch it: %w", o.Repo, o.Tag, err)
	}
	dir, err := os.MkdirTemp("", "deployforge-ship-")
	if err != nil {
		return nil, fmt.Errorf("ship: %w", err)
	}
	p := &Package{Commit: commit, dir: dir, repo: o.Repo, commits: map[string]string{}}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Close())
		}
	}()
	if o.Local && o.RequirePushed {
		if err := requirePushed(o, commit); err != nil {
			return nil, err
		}
	}
	tree := filepath.Join(dir, o.App)
	if err := p.checkout(o.Repo, tree, commit); err != nil {
		return nil, err
	}
	p.tree = tree
	script := filepath.Join(tree, scriptPath)
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("ship: %s at %s has no %s", o.App, ref, scriptPath)
	}
	if err := checkModes(o.Repo, commit, filepath.ToSlash(filepath.Clean(scriptPath)), o.Log); err != nil {
		return nil, fmt.Errorf("ship: %s at %s: %w", o.App, ref, err)
	}
	out := filepath.Join(dir, "dist")
	args := []string{"--out", out}
	// Without a local build, no go.work in the operator's shell may choose
	// the build's inputs.
	gowork := "off"
	if o.Local {
		if gowork, err = p.workspace(o.Upstreams); err != nil {
			return nil, err
		}
		if err := p.webInputs(ctx, o.WebUpstreams, o.Log); err != nil {
			return nil, err
		}
		args = append(args, "--allow-untagged")
		for _, in := range p.Inputs {
			args = append(args, "--input", in.String())
		}
	}
	cmd := exec.CommandContext(ctx, script, args...) //nolint:gosec // G204: the app's own release script, at the commit shipped
	cmd.Dir = tree
	cmd.Env = append(withoutEnv(os.Environ(), "GOWORK"), "GOWORK="+gowork, "DEPLOYFORGE="+o.Deployforge, "DEPLOYFORGE_FLEET="+o.Fleet)
	cmd.Env = append(cmd.Env, o.Env...)
	cmd.Stdout, cmd.Stderr = o.Log, o.Log
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ship: %s's release script: %w", o.App, err)
	}
	archives, err := filepath.Glob(filepath.Join(out, o.App+"-*.tgz"))
	if err != nil {
		return nil, fmt.Errorf("ship: %w", err)
	}
	if len(archives) != 1 {
		return nil, fmt.Errorf("ship: the release script made %d archives in %s, not one", len(archives), out)
	}
	p.Archive = archives[0]
	return p, nil
}

// ExecutableMode is git's mode for an executable file.
const ExecutableMode = "100755"

// checkModes reads, in commit's tree, the mode git recorded for the release
// script and the shell scripts beside it. The script itself must be
// executable, or running it fails with "permission denied" in every
// checkout of the commit, whatever the operator's own checkout has; a
// script beside it that is not is reported to log, since the release
// script may call it. Modes come from git, not the file system, so the
// check is the same on every operating system.
func checkModes(repo, commit, script string, log io.Writer) error {
	dir := path.Dir(script)
	spec := dir + "/"
	if dir == "." {
		spec = "."
	}
	out, err := git(repo, "ls-tree", commit, "--", spec)
	if err != nil {
		return fmt.Errorf("reading the modes of %s: %w: %s", spec, err, out)
	}
	var notExec []string
	scriptMode := ""
	for _, line := range strings.Split(out, "\n") {
		meta, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		switch {
		case name == script:
			scriptMode = fields[0]
		case strings.HasSuffix(name, ".sh") && fields[0] != ExecutableMode:
			notExec = append(notExec, name)
		}
	}
	for _, n := range notExec {
		if _, err := fmt.Fprintf(log, "warning: %s is not executable in this commit; if the release script runs it, fix it with git update-index --chmod=+x %s, then commit and tag again\n", n, n); err != nil {
			return err
		}
	}
	if scriptMode != ExecutableMode {
		return fmt.Errorf("the release script %s is not executable in this commit (mode %s): fix it with git update-index --chmod=+x %s, then commit and tag again", script, cmp.Or(scriptMode, "not found"), script)
	}
	return nil
}

// checkout adds a detached worktree of repo at commit, removed by Close.
func (p *Package) checkout(repo, path, commit string) error {
	if out, err := git(repo, "worktree", "add", "--quiet", "--detach", path, commit); err != nil {
		return fmt.Errorf("ship: checking out %s of %s: %w: %s", commit, repo, err, out)
	}
	p.trees = append(p.trees, worktree{repo: repo, path: path})
	return nil
}

// workspace checks out each upstream's commit beside the app's worktree
// and writes a go.work that uses them all, returning its path. Each
// upstream must be a module the app's go.mod requires, so a typo cannot
// silently build nothing different.
func (p *Package) workspace(ups []Upstream) (string, error) {
	app, err := readModule(p.tree)
	if err != nil {
		return "", err
	}
	required := map[string]bool{}
	for _, r := range app.Require {
		required[r.Mod.Path] = true
	}
	goVersion := app.Go.Version
	uses := []string{"./" + filepath.Base(p.tree)}
	seen := map[string]bool{}
	for _, u := range ups {
		if seen[u.Repo] {
			return "", fmt.Errorf("ship: %s is named twice", u.Repo)
		}
		seen[u.Repo] = true
		commit, err := p.upstreamCommit(u)
		if err != nil {
			return "", err
		}
		name := strings.ReplaceAll(u.Repo, "/", "_")
		path := filepath.Join(p.dir, "upstream", name)
		if err := p.checkout(u.Dir, path, commit); err != nil {
			return "", err
		}
		mod, err := readModule(path)
		if err != nil {
			return "", err
		}
		if !required[mod.Module.Mod.Path] {
			return "", fmt.Errorf("ship: the app's go.mod does not require %s, so building it from %s would change nothing", mod.Module.Mod.Path, u.Repo)
		}
		if mod.Go != nil && semver.Compare("v"+mod.Go.Version, "v"+goVersion) > 0 {
			goVersion = mod.Go.Version
		}
		uses = append(uses, "./upstream/"+name)
		if err := p.addInput(u.Repo, commit); err != nil {
			return "", err
		}
	}
	work := "go " + goVersion + "\n\nuse (\n"
	for _, u := range uses {
		work += "\t" + u + "\n"
	}
	work += ")\n"
	path := filepath.Join(p.dir, "go.work")
	if err := os.WriteFile(path, []byte(work), 0o600); err != nil {
		return "", fmt.Errorf("ship: %w", err)
	}
	return path, nil
}

// upstreamCommit checks the upstream's checkout is the repository named and
// returns the full commit of its ref (main by default).
func (p *Package) upstreamCommit(u Upstream) (string, error) {
	return resolveUpstream(u)
}

func resolveUpstream(u Upstream) (string, error) {
	if err := requireOrigin(u.Dir, u.Repo); err != nil {
		return "", err
	}
	ref := u.Ref
	if ref == "" {
		ref = "main"
	}
	commit, err := git(u.Dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("ship: %s has no commit %s: %w", u.Dir, ref, err)
	}
	if err := (provenance.Input{Repo: u.Repo, Commit: commit}).Validate(); err != nil {
		return "", fmt.Errorf("ship: %w", err)
	}
	return commit, nil
}

// addInput records an upstream commit once; a repository named for both Go
// and web must resolve to the same commit.
func (p *Package) addInput(repo, commit string) error {
	if prev, ok := p.commits[repo]; ok {
		if prev != commit {
			return fmt.Errorf("ship: %s is named at two commits, %s and %s", repo, prev[:12], commit[:12])
		}
		return nil
	}
	p.commits[repo] = commit
	p.Inputs = append(p.Inputs, provenance.Input{Repo: repo, Commit: commit})
	provenance.Sort(p.Inputs)
	return nil
}

// requirePushed refuses the build when the app's commit or an upstream's is
// on no remote branch of its checkout, after fetching origin: whatever a
// local build ships is recorded on a remote, without waiting for its CI.
func requirePushed(o Options, commit string) error {
	type check struct{ repo, dir, commit string }
	checks := []check{{repo: "the app", dir: o.Repo, commit: commit}}
	for _, u := range append(append([]Upstream(nil), o.Upstreams...), o.WebUpstreams...) {
		if o.AllowUnpushed[u.Repo] {
			continue
		}
		c, err := resolveUpstream(u)
		if err != nil {
			return err
		}
		checks = append(checks, check{repo: u.Repo, dir: u.Dir, commit: c})
	}
	fetched := map[string]bool{}
	var missing []string
	for _, c := range checks {
		if !fetched[c.dir] {
			if out, err := git(c.dir, "fetch", "--quiet", "origin"); err != nil {
				return fmt.Errorf("ship: fetching %s to check what is pushed: %w: %s", c.dir, err, out)
			}
			fetched[c.dir] = true
		}
		branches, err := git(c.dir, "branch", "--remotes", "--contains", c.commit)
		if err != nil {
			return fmt.Errorf("ship: %w: %s", err, branches)
		}
		if branches == "" {
			missing = append(missing, fmt.Sprintf("%s at %s (%s)", c.repo, c.commit[:12], c.dir))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("ship: push these commits to any branch first; the build does not wait for CI:\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

// readModule parses the go.mod at the root of dir.
func readModule(dir string) (*modfile.File, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // G304: a worktree this package made
	if err != nil {
		return nil, fmt.Errorf("ship: %s has no go.mod: %w", dir, err)
	}
	f, err := modfile.Parse(filepath.Join(dir, "go.mod"), data, nil)
	if err != nil {
		return nil, fmt.Errorf("ship: %w", err)
	}
	if f.Module == nil || f.Go == nil {
		return nil, fmt.Errorf("ship: %s's go.mod names no module or Go version", dir)
	}
	return f, nil
}

// requireOrigin checks the checkout's origin is the repository named, so
// the wrong checkout is not recorded under another's name.
func requireOrigin(dir, want string) error {
	// The configured URL, before any insteadOf rewriting.
	url, err := git(dir, "config", "--get", "remote.origin.url")
	if err != nil {
		return fmt.Errorf("ship: %s has no origin remote to check against %s: %w", dir, want, err)
	}
	if got := normalizeRemote(url); !strings.EqualFold(got, want) {
		return fmt.Errorf("ship: %s's origin is %s, not %s", dir, got, want)
	}
	return nil
}

// normalizeRemote turns a git remote URL into host/org/repo.
func normalizeRemote(u string) string {
	u = strings.TrimSuffix(strings.TrimSpace(u), ".git")
	for _, p := range []string{"https://", "http://", "ssh://", "git://"} {
		u = strings.TrimPrefix(u, p)
	}
	if i := strings.Index(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	return strings.Replace(u, ":", "/", 1)
}

// withoutEnv drops every setting of name from env.
func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// git runs git in repo and returns its trimmed output.
func git(repo string, args ...string) (string, error) {
	var b bytes.Buffer
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...) //nolint:gosec // G204: fixed git subcommands
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return strings.TrimSpace(b.String()), err
}
