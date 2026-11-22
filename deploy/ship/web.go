package ship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// A web upstream is a repository holding npm packages the app's front end
// uses. A local build packs each such package from the upstream's local
// commit and makes the app's front end install the packed file in place of
// its registry or git version:
//
//  1. the upstream's pnpm workspace is installed, which runs each
//     package's prepare build, and a package with no prepare script is
//     built with its dependencies (pnpm --filter <name>... run build);
//  2. pnpm pack writes it as a tarball, turning workspace: ranges into
//     versions;
//  3. the app's pnpm-workspace.yaml gains an override of each package to
//     its tarball, and pnpm-lock.yaml is resolved again;
//  4. both files are marked skip-worktree in the app's temporary
//     worktree, so the release script's clean-checkout checks pass while
//     the archive's inputs name every upstream commit.
//
// Nothing outside the temporary directory changes.

// runCommand runs argv in dir, sending its output to log. Tests replace it.
var runCommand = func(ctx context.Context, dir string, log io.Writer, argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: pnpm with fixed subcommands
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ship: %s in %s: %w", strings.Join(argv, " "), dir, err)
	}
	return nil
}

// npmPackage is one package.json in a checkout.
type npmPackage struct {
	Name    string            `json:"name"`
	Scripts map[string]string `json:"scripts"`
	Deps    map[string]string `json:"dependencies"`
	DevDeps map[string]string `json:"devDependencies"`
	dir     string            // relative to the checkout, slash-separated
}

// packages lists the package.json files git tracks in a checkout.
func packages(tree string) ([]npmPackage, error) {
	out, err := git(tree, "ls-files", "-z", "--", "package.json", "*/package.json")
	if err != nil {
		return nil, fmt.Errorf("ship: listing %s's packages: %w: %s", tree, err, out)
	}
	var pkgs []npmPackage
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(tree, filepath.FromSlash(rel))) //nolint:gosec // G304: a file git tracks in a worktree this package made
		if err != nil {
			return nil, fmt.Errorf("ship: %w", err)
		}
		var p npmPackage
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("ship: %s in %s: %w", rel, tree, err)
		}
		p.dir = path.Dir(rel)
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// tracked reports whether git tracks rel in the worktree.
func tracked(tree, rel string) bool {
	_, err := git(tree, "ls-files", "--error-unmatch", "--", rel)
	return err == nil
}

// appFronts are the app's front ends: each directory with a tracked
// pnpm-lock.yaml beside its package.json, and the packages it depends on.
func appFronts(tree string) (map[string]map[string]bool, error) {
	pkgs, err := packages(tree)
	if err != nil {
		return nil, err
	}
	fronts := map[string]map[string]bool{}
	for _, p := range pkgs {
		if !tracked(tree, path.Join(p.dir, "pnpm-lock.yaml")) {
			continue
		}
		uses := map[string]bool{}
		for name := range p.Deps {
			uses[name] = true
		}
		for name := range p.DevDeps {
			uses[name] = true
		}
		fronts[p.dir] = uses
	}
	return fronts, nil
}

// workspaceRoot is the nearest directory at or above dir, within the
// checkout, that holds a tracked pnpm-lock.yaml.
func workspaceRoot(tree, dir string) (string, error) {
	for d := dir; ; d = path.Dir(d) {
		if tracked(tree, path.Join(d, "pnpm-lock.yaml")) {
			return d, nil
		}
		if d == "." {
			return "", fmt.Errorf("ship: %s in %s has no pnpm-lock.yaml at or above it", dir, tree)
		}
	}
}

// packWeb builds and packs every package of the upstream at tree that the
// app's front ends use, into packs, and returns name -> tarball.
func packWeb(ctx context.Context, tree, repo, packs string, fronts map[string]map[string]bool, log io.Writer) (map[string]string, error) {
	pkgs, err := packages(tree)
	if err != nil {
		return nil, err
	}
	var used []npmPackage
	for _, p := range pkgs {
		for _, uses := range fronts {
			if p.Name != "" && uses[p.Name] {
				used = append(used, p)
				break
			}
		}
	}
	if len(used) == 0 {
		return nil, fmt.Errorf("ship: %s provides no package the app's front end uses", repo)
	}
	sort.Slice(used, func(a, b int) bool { return used[a].Name < used[b].Name })
	installed := map[string]bool{}
	tarballs := map[string]string{}
	for _, p := range used {
		root, err := workspaceRoot(tree, p.dir)
		if err != nil {
			return nil, err
		}
		rootDir := filepath.Join(tree, filepath.FromSlash(root))
		if !installed[root] {
			// Installing runs each package's prepare build.
			if err := runCommand(ctx, rootDir, log, "pnpm", "install", "--frozen-lockfile"); err != nil {
				return nil, err
			}
			installed[root] = true
		}
		if _, prepared := p.Scripts["prepare"]; !prepared {
			if _, ok := p.Scripts["build"]; ok {
				if err := runCommand(ctx, rootDir, log, "pnpm", "--filter", p.Name+"...", "run", "--if-present", "build"); err != nil {
					return nil, err
				}
			}
		}
		before, err := tgzs(packs)
		if err != nil {
			return nil, err
		}
		if err := runCommand(ctx, filepath.Join(tree, filepath.FromSlash(p.dir)), log, "pnpm", "pack", "--pack-destination", packs); err != nil {
			return nil, err
		}
		after, err := tgzs(packs)
		if err != nil {
			return nil, err
		}
		var made []string
		for f := range after {
			if !before[f] {
				made = append(made, f)
			}
		}
		if len(made) != 1 {
			return nil, fmt.Errorf("ship: packing %s made %d tarballs, not one", p.Name, len(made))
		}
		tarballs[p.Name] = filepath.Join(packs, made[0])
	}
	return tarballs, nil
}

func tgzs(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ship: %w", err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tgz") {
			out[e.Name()] = true
		}
	}
	return out, nil
}

// overrideFront points the front end at dir in the app's worktree to the
// packed tarballs it uses, resolves its lockfile again, and hides both
// changes from git status in this worktree only.
func overrideFront(ctx context.Context, tree, dir string, uses map[string]bool, tarballs map[string]string, log io.Writer) error {
	var names []string
	for name := range tarballs {
		if uses[name] {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	ws := path.Join(dir, "pnpm-workspace.yaml")
	if !tracked(tree, ws) {
		return fmt.Errorf("ship: %s has no pnpm-workspace.yaml in git, where the local packages' overrides go", dir)
	}
	wsPath := filepath.Join(tree, filepath.FromSlash(ws))
	data, err := os.ReadFile(wsPath) //nolint:gosec // G304: a file git tracks in a worktree this package made
	if err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "overrides:") {
			return fmt.Errorf("ship: %s already has overrides, which a local build does not merge", ws)
		}
	}
	var b bytes.Buffer
	b.Write(data)
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		b.WriteString("\n")
	}
	b.WriteString("\n# A local build's packages; in a temporary worktree only.\noverrides:\n")
	for _, name := range names {
		fmt.Fprintf(&b, "  %s: %s\n", yamlQuote(name), yamlQuote("file:"+tarballs[name]))
	}
	if err := os.WriteFile(wsPath, b.Bytes(), 0o600); err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	if err := runCommand(ctx, filepath.Join(tree, filepath.FromSlash(dir)), log, "pnpm", "install", "--lockfile-only"); err != nil {
		return err
	}
	if out, err := git(tree, "update-index", "--skip-worktree", "--", ws, path.Join(dir, "pnpm-lock.yaml")); err != nil {
		return fmt.Errorf("ship: hiding the overrides from git status: %w: %s", err, out)
	}
	return nil
}

func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// webInputs packs each web upstream and overrides the app's front ends.
func (p *Package) webInputs(ctx context.Context, ups []Upstream, log io.Writer) error {
	if len(ups) == 0 {
		return nil
	}
	fronts, err := appFronts(p.tree)
	if err != nil {
		return err
	}
	if len(fronts) == 0 {
		return errors.New("ship: the app has no front end with a pnpm-lock.yaml for web upstreams to replace packages in")
	}
	packs := filepath.Join(p.dir, "packs")
	if err := os.MkdirAll(packs, 0o750); err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	all := map[string]string{}
	from := map[string]string{}
	for _, u := range ups {
		commit, err := p.upstreamCommit(u)
		if err != nil {
			return err
		}
		tree := filepath.Join(p.dir, "web", strings.ReplaceAll(u.Repo, "/", "_"))
		if err := p.checkout(u.Dir, tree, commit); err != nil {
			return err
		}
		tarballs, err := packWeb(ctx, tree, u.Repo, packs, fronts, log)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(tarballs))
		for name := range tarballs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			tgz := tarballs[name]
			if other, ok := from[name]; ok {
				return fmt.Errorf("ship: both %s and %s provide %s", other, u.Repo, name)
			}
			from[name] = u.Repo
			all[name] = tgz
			if _, err := fmt.Fprintf(log, "ship: %s from %s at %s\n", name, u.Repo, commit[:12]); err != nil {
				return fmt.Errorf("ship: %w", err)
			}
		}
		if err := p.addInput(u.Repo, commit); err != nil {
			return err
		}
	}
	dirs := make([]string, 0, len(fronts))
	for d := range fronts {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if err := overrideFront(ctx, p.tree, d, fronts[d], all, log); err != nil {
			return err
		}
	}
	if st, err := git(p.tree, "status", "--porcelain", "--untracked-files=all"); err != nil || st != "" {
		return fmt.Errorf("ship: the app's worktree is not clean after the overrides: %v %s", err, st)
	}
	return nil
}
