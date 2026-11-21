// Package release turns a host repository's build output into the release
// archive the deploy script ships: a reproducible gzipped tar whose
// root holds the app's binary, its web files and a REVISION file, with a
// checksum beside it.
//
// The host repository builds a directory; this package checks that what it was
// built from is reproducible and that what it built is deployable, then packs
// it. Nothing here imports a host repository.
package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/provenance"
)

// Options say what to package.
type Options struct {
	// App is the app's name in the fleet document.
	App string
	// Repo is the host repository's checkout the release is built from.
	Repo string
	// From is the directory the host's build wrote: the binary named for the
	// app at its root, and the web files beside it.
	From string
	// Out is the directory the archive and its checksum are written to.
	Out string
	// AllowUntagged lets a revision with no release tag be packaged, for a
	// staging trial or a local build.
	AllowUntagged bool
	// Inputs are the upstream repositories a local build took from local
	// commits; REVISION names each, and the archive's name carries their
	// digest, so two builds of one app commit are told apart.
	Inputs []provenance.Input
}

// Result describes the archive written.
type Result struct {
	// Archive and ChecksumFile are the files written.
	Archive, ChecksumFile string
	// SHA256 is the archive's checksum.
	SHA256 string
	// Revision is the 12-character commit the release was built from, and Tag
	// its release tag, empty when untagged.
	Revision, Tag string
	// Inputs are the upstream commits the release was built with.
	Inputs []provenance.Input
	// Existing is true when the same archive and checksum were already
	// there, from packaging the same revision before, and were kept.
	Existing bool
}

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Package checks the checkout and the build output and writes the archive.
func Package(f *fleet.Fleet, o Options) (Result, error) {
	app, ok := f.App(o.App)
	if !ok {
		return Result{}, fmt.Errorf("release: no app %q in the fleet document", o.App)
	}
	g := git{dir: o.Repo}
	if err := g.requireCleanCheckout(); err != nil {
		return Result{}, err
	}
	if err := requireRepoMatches(g, app.Repo); err != nil {
		return Result{}, err
	}
	if err := requireNoReplace(o.Repo); err != nil {
		return Result{}, err
	}
	rev, err := g.out("rev-parse", "--short=12", "HEAD")
	if err != nil || !revisionPattern.MatchString(rev) {
		return Result{}, fmt.Errorf("release: cannot read the revision of %s: %v %s", o.Repo, err, rev)
	}
	tag := ""
	if t, err := g.out("describe", "--exact-match", "--tags", "HEAD"); err == nil {
		tag = t
	} else if !o.AllowUntagged {
		return Result{}, fmt.Errorf("release: %s is not at a release tag; tag it, or allow an untagged revision for a staging trial", rev)
	}
	committed, err := g.out("show", "-s", "--format=%ct", "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("release: %w", err)
	}
	secs, err := strconv.ParseInt(committed, 10, 64)
	if err != nil {
		return Result{}, fmt.Errorf("release: commit time %q: %w", committed, err)
	}

	files, err := collect(o.From)
	if err != nil {
		return Result{}, err
	}
	if err := checkBinary(filepath.Join(o.From, app.Name)); err != nil {
		return Result{}, err
	}

	inputs := append([]provenance.Input(nil), o.Inputs...)
	provenance.Sort(inputs)
	for i, in := range inputs {
		if err := in.Validate(); err != nil {
			return Result{}, fmt.Errorf("release: %w", err)
		}
		if i > 0 && inputs[i-1].Repo == in.Repo {
			return Result{}, fmt.Errorf("release: %s is named twice as an input", in.Repo)
		}
	}
	revision := app.Name + " " + rev + "\n"
	if tag != "" {
		revision += "tag " + tag + "\n"
	}
	for _, in := range inputs {
		revision += "input " + in.Repo + " " + in.Commit + "\n"
	}
	archive, err := pack(files, o.From, []byte(revision), time.Unix(secs, 0).UTC())
	if err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(archive)
	hexsum := hex.EncodeToString(sum[:])

	name := fmt.Sprintf("%s-%s.tgz", app.Name, rev)
	if d := provenance.Digest(inputs); d != "" {
		name = fmt.Sprintf("%s-%s-%s.tgz", app.Name, rev, d)
	}
	res := Result{
		Archive: filepath.Join(o.Out, name), ChecksumFile: filepath.Join(o.Out, name+".sha256"),
		SHA256: hexsum, Revision: rev, Tag: tag, Inputs: inputs,
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil { //nolint:gosec // G301: release artifacts are meant to be readable
		return Result{}, fmt.Errorf("release: %w", err)
	}
	sumLine := []byte(hexsum + "  " + name + "\n")
	// Packaging is reproducible, so packaging the same revision again makes
	// the same bytes: files already there that match are kept, and the run
	// succeeds. Files that differ are never replaced.
	same, err := matches(map[string][]byte{res.Archive: archive, res.ChecksumFile: sumLine})
	if err != nil {
		return Result{}, err
	}
	if same {
		res.Existing = true
		return res, nil
	}
	if err := os.WriteFile(res.Archive, archive, 0o644); err != nil { //nolint:gosec // G306: a release artifact, meant to be readable
		return Result{}, fmt.Errorf("release: %w", err)
	}
	if err := os.WriteFile(res.ChecksumFile, sumLine, 0o644); err != nil { //nolint:gosec // G306: a checksum, meant to be readable
		return Result{}, fmt.Errorf("release: %w", err)
	}
	return res, nil
}

// matches reports whether every file exists with exactly the given bytes.
// None existing is false; some existing, or any differing, is an error, so
// a different archive is never written over one of the same name.
func matches(files map[string][]byte) (bool, error) {
	present := 0
	for path, want := range files {
		got, err := os.ReadFile(path) //nolint:gosec // G304: the archive and checksum this package names
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return false, fmt.Errorf("release: %w", err)
		case !bytes.Equal(got, want):
			return false, fmt.Errorf("release: %s already exists and differs from this packaging; remove it to package again", path)
		}
		present++
	}
	switch present {
	case 0:
		return false, nil
	case len(files):
		return true, nil
	}
	return false, errors.New("release: only part of an earlier packaging is there; remove the archive and its checksum to package again")
}

// git runs git in a checkout.
type git struct{ dir string }

func (g git) out(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", g.dir}, args...)...) //nolint:gosec // G204: git with fixed subcommands in a directory the operator names
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// requireCleanCheckout refuses a checkout with changes or untracked files, so
// the release can be rebuilt from the commit it names.
func (g git) requireCleanCheckout() error {
	if _, err := g.out("rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("release: %s is not a git checkout", g.dir)
	}
	status, err := g.out("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if status != "" {
		return fmt.Errorf("release: %s has uncommitted or untracked changes; refusing to package an unreproducible build:\n%s", g.dir, status)
	}
	return nil
}

// requireRepoMatches checks the checkout's origin is the repository the fleet document
// names for the app, so the wrong repository is not packaged under its name.
func requireRepoMatches(g git, want string) error {
	url, err := g.out("remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("release: %s has no origin remote to check against the fleet document's %s", g.dir, want)
	}
	if got := normalizeRemote(url); !strings.EqualFold(got, want) {
		return fmt.Errorf("release: the checkout's origin is %s, but the fleet document names %s", got, want)
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

// requireNoReplace refuses a build whose go.mod redirects a dependency, because
// a release uses released dependencies only.
func requireNoReplace(repo string) error {
	data, err := os.ReadFile(filepath.Join(repo, "go.mod")) //nolint:gosec // G304: the operator names the checkout
	if errors.Is(err, fs.ErrNotExist) {
		return nil // not a Go module; there is nothing to redirect
	}
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	// Parse, not ParseLax: the lax parser ignores replace directives, which are the
	// point of this check.
	mf, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if len(mf.Replace) > 0 {
		r := mf.Replace[0]
		return fmt.Errorf("release: go.mod replaces %s with %s; a release uses released dependencies only", r.Old.Path, r.New.Path)
	}
	return nil
}

// collect lists the regular files under dir, relative and sorted, and refuses
// anything that could escape or surprise: a symlink, a device, a REVISION that
// the build wrote itself.
func collect(dir string) ([]string, error) {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("release: %s is not a directory", dir)
	}
	var files []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("release: %s is a symbolic link; the archive holds files only", rel)
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("release: %s is not a regular file", rel)
		case rel == "REVISION":
			return errors.New("release: the build wrote a REVISION file; packaging writes it from the checkout")
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("release: %s is empty", dir)
	}
	return files, nil
}

// checkBinary requires the app's binary to be a static linux/amd64 executable,
// which is what runs on the host: a binary built on the operator's laptop
// for the wrong platform would pass every other check and fail on the host.
func checkBinary(path string) error {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("release: no binary %s at the root of the build output", filepath.Base(path))
	}
	if err := CheckLinuxBinary(path); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// CheckLinuxBinary requires a file to be a static linux/amd64 executable, the
// only kind the hosts run.
func CheckLinuxBinary(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s is not a linux executable (build it with GOOS=linux GOARCH=amd64 CGO_ENABLED=0): %w", filepath.Base(path), err)
	}
	defer f.Close() //nolint:errcheck // read-only
	if f.Machine != elf.EM_X86_64 || f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("%s is built for %s, not linux/amd64", filepath.Base(path), f.Machine)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("%s is not an executable", filepath.Base(path))
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked; build it with CGO_ENABLED=0", filepath.Base(path))
		}
	}
	return nil
}

// pack writes a reproducible gzipped tar: sorted entries, no owners, one
// modification time (the commit's), and normalized modes. The same files from the
// same commit give the same bytes with the same Go toolchain.
func pack(files []string, dir string, revision []byte, mtime time.Time) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)
	hdr := func(name string, mode int64, size int64, typ byte) *tar.Header {
		return &tar.Header{Name: name, Mode: mode, Size: size, Typeflag: typ, ModTime: mtime}
	}

	// Directories first, so extraction does not depend on file order.
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		for d := filepath.ToSlash(filepath.Dir(f)); d != "." && !seen[d]; d = filepath.ToSlash(filepath.Dir(d)) {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if err := tw.WriteHeader(hdr(d+"/", 0o755, 0, tar.TypeDir)); err != nil {
			return nil, err
		}
	}
	add := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(hdr(name, mode, int64(len(data)), tar.TypeReg)); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	all := append([]string{"REVISION"}, files...)
	sort.Strings(all)
	for _, name := range all {
		if name == "REVISION" {
			if err := add(name, 0o644, revision); err != nil {
				return nil, err
			}
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		fh, err := os.Open(p) //nolint:gosec // G304: a file the walk of the build output just found
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(fh)
		_ = fh.Close() // read-only; the read error is the one to report
		if err != nil {
			return nil, err
		}
		mode := int64(0o644)
		if st.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := add(name, mode, data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
