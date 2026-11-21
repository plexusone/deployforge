package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/plexusone/deployforge/deploy/archive"
	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/provenance"
)

var amd64Bin, arm64Bin string // static test binaries, built once

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "release-test")
	if err != nil {
		panic(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o750); err != nil { //nolint:gosec // G301: a scratch directory
		panic(err)
	}
	for name, body := range map[string]string{
		"go.mod":  "module example.com/tiny\n\ngo 1.22\n",
		"main.go": "package main\n\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			panic(err)
		}
	}
	build := func(arch string) string {
		out := filepath.Join(dir, "tiny-"+arch)
		cmd := exec.Command("go", "build", "-o", out, ".") //nolint:gosec // G204: builds a fixed tiny program for the test
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
		if o, err := cmd.CombinedOutput(); err != nil {
			panic("build " + arch + ": " + err.Error() + "\n" + string(o))
		}
		return out
	}
	amd64Bin, arm64Bin = build("amd64"), build("arm64")
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "removing the test binaries:", err)
	}
	os.Exit(code)
}

func testFleet() *fleet.Fleet {
	return &fleet.Fleet{
		FormatVersion: fleet.FormatVersion,
		PortRange:     fleet.PortRange{Min: 8101, Max: 8199},
		Hosts:         []fleet.Host{{Name: "web-1", Address: "203.0.113.10"}},
		Apps: []fleet.App{{
			Name: "one-cloud", Repo: "github.com/example/one-cloud", Domain: "one.example.com",
			SecretPrefix: "/example/{stack}/one-cloud/",
			Stacks: map[string]fleet.Stack{
				"prod": {Host: "web-1", Hosts: []string{"one.example.com"}, Port: 8101, Databases: []fleet.Database{{Name: "one", AppRole: true}}, Status: fleet.Planned},
			},
		}},
	}
}

// gitIn runs git in a scratch repository the test made.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: git with arguments the test chose
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-10-23T18:00:00Z", "GIT_COMMITTER_DATE=2026-10-23T18:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil { //nolint:gosec // G301: a scratch directory
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil { //nolint:gosec // G703: a path under the test's own temporary directory
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from) //nolint:gosec // G304: a test binary the test built
	if err != nil {
		t.Fatal(err)
	}
	write(t, to, string(data), 0o755)
}

// repo makes a clean, committed checkout whose origin is the app's repository.
func repo(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "go.mod"), "module github.com/example/one-cloud\n\ngo 1.22\n", 0o644)
	write(t, filepath.Join(dir, "main.go"), "package main\n", 0o644)
	gitIn(t, dir, "remote", "add", "origin", "git@github.com:example/one-cloud.git")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	if tag != "" {
		gitIn(t, dir, "-c", "tag.gpgsign=false", "tag", tag)
	}
	return dir
}

// built makes a build output directory: the binary and a web file.
func built(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	copyFile(t, amd64Bin, filepath.Join(dir, "one-cloud"))
	write(t, filepath.Join(dir, "web", "vision", "index.html"), "<html></html>", 0o600)
	write(t, filepath.Join(dir, "web", "agents", "app.js"), "x", 0o644)
	return dir
}

func entries(t *testing.T, archive string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(archive) //nolint:gosec // G304: an archive the test just made
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s carries an owner: %d:%d %s:%s", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		if want := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC); !h.ModTime.Equal(want) {
			t.Errorf("%s has mtime %s, want the commit's %s", h.Name, h.ModTime, want)
		}
		if h.Typeflag == tar.TypeDir {
			out[h.Name] = "<dir>"
		} else {
			out[h.Name] = string(body)
		}
		if h.Name == "one-cloud" && h.Mode != 0o755 {
			t.Errorf("the binary has mode %o", h.Mode)
		}
		if h.Name == "web/vision/index.html" && h.Mode != 0o644 {
			t.Errorf("a web file has mode %o, want it normalized to 644", h.Mode)
		}
	}
}

func TestATaggedCleanCheckoutIsPackagedWithItsRevisionAndChecksum(t *testing.T) {
	r, from, out := repo(t, "v1.2.0"), built(t), t.TempDir()
	res, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	rev := gitIn(t, r, "rev-parse", "--short=12", "HEAD")
	if res.Revision != rev || res.Tag != "v1.2.0" || filepath.Base(res.Archive) != "one-cloud-"+rev+".tgz" {
		t.Fatalf("result = %+v", res)
	}
	got := entries(t, res.Archive)
	if got["REVISION"] != "one-cloud "+rev+"\ntag v1.2.0\n" {
		t.Errorf("REVISION = %q", got["REVISION"])
	}
	for _, want := range []string{"one-cloud", "web/", "web/vision/", "web/vision/index.html", "web/agents/app.js"} {
		if _, ok := got[want]; !ok {
			t.Errorf("archive lacks %s; has %v", want, keys(got))
		}
	}
	// The checksum file and the result agree with the bytes, in the form sha256sum checks.
	data, _ := os.ReadFile(res.Archive) //nolint:gosec // G304: an archive the test just made
	h := sha256.Sum256(data)
	if res.SHA256 != hex.EncodeToString(h[:]) {
		t.Errorf("sha256 = %s, want %x", res.SHA256, h)
	}
	sumFile, _ := os.ReadFile(res.ChecksumFile) //nolint:gosec // G304: a checksum the test just made
	if string(sumFile) != res.SHA256+"  "+filepath.Base(res.Archive)+"\n" {
		t.Errorf("checksum file = %q", sumFile)
	}
}

func keys(m map[string]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

func TestThePackageIsReproducible(t *testing.T) {
	r, from := repo(t, "v1.0.0"), built(t)
	a, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != b.SHA256 {
		t.Fatalf("the same inputs gave two archives: %s and %s", a.SHA256, b.SHA256)
	}
}

func TestAnUntaggedRevisionNeedsToBeAllowed(t *testing.T) {
	r, from := repo(t, ""), built(t)
	if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "release tag") {
		t.Fatalf("untagged: %v", err)
	}
	res, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir(), AllowUntagged: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tag != "" || strings.Contains(entries(t, res.Archive)["REVISION"], "tag ") {
		t.Errorf("an untagged release named a tag: %+v", res)
	}
}

func TestACheckoutThatCannotBeRebuiltIsRefused(t *testing.T) {
	from := built(t)
	t.Run("an untracked file", func(t *testing.T) {
		r := repo(t, "v1.0.0")
		write(t, filepath.Join(r, "scratch.txt"), "x", 0o644)
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "uncommitted or untracked") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a modified file", func(t *testing.T) {
		r := repo(t, "v1.0.0")
		write(t, filepath.Join(r, "main.go"), "package main // changed\n", 0o644)
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "uncommitted") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a replace directive", func(t *testing.T) {
		r := repo(t, "")
		write(t, filepath.Join(r, "go.mod"), "module github.com/example/one-cloud\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n\nreplace example.com/dep => ../dep\n", 0o644)
		gitIn(t, r, "add", ".")
		gitIn(t, r, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "replace")
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir(), AllowUntagged: true}); err == nil || !strings.Contains(err.Error(), "replaces example.com/dep") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not a git checkout", func(t *testing.T) {
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: t.TempDir(), From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not a git checkout") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTheWrongRepositoryIsNotPackagedUnderASitesName(t *testing.T) {
	r := repo(t, "v1.0.0")
	gitIn(t, r, "remote", "set-url", "origin", "https://github.com/someone/else.git")
	if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: built(t), Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "github.com/someone/else") {
		t.Fatalf("err = %v", err)
	}
	gitIn(t, r, "remote", "remove", "origin")
	if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: built(t), Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no origin") {
		t.Fatalf("no origin: %v", err)
	}
	// Every spelling of the right repository is accepted.
	for _, u := range []string{"https://github.com/example/one-cloud.git", "git@github.com:example/one-cloud.git", "ssh://git@github.com/example/one-cloud", "https://github.com/Example/One-Cloud"} {
		if got := normalizeRemote(u); !strings.EqualFold(got, "github.com/example/one-cloud") {
			t.Errorf("normalizeRemote(%q) = %q", u, got)
		}
	}
}

func TestBuildOutputThatCannotRunOnTheInstanceIsRefused(t *testing.T) {
	r := repo(t, "v1.0.0")
	t.Run("the wrong platform", func(t *testing.T) {
		from := t.TempDir()
		copyFile(t, arm64Bin, filepath.Join(from, "one-cloud"))
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not linux/amd64") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not a linux executable", func(t *testing.T) {
		from := t.TempDir()
		write(t, filepath.Join(from, "one-cloud"), "#!/bin/sh\n", 0o755)
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not a linux executable") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no binary", func(t *testing.T) {
		from := t.TempDir()
		write(t, filepath.Join(from, "web", "index.html"), "x", 0o644)
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no binary one-cloud") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a symbolic link", func(t *testing.T) {
		from := built(t)
		if err := os.Symlink("/etc/passwd", filepath.Join(from, "web", "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a REVISION the build wrote", func(t *testing.T) {
		from := built(t)
		write(t, filepath.Join(from, "REVISION"), "forged\n", 0o644)
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "REVISION") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a missing or empty directory", func(t *testing.T) {
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: filepath.Join(t.TempDir(), "nope"), Out: t.TempDir()}); err == nil {
			t.Error("a missing directory was accepted")
		}
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: t.TempDir(), Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Errorf("an empty directory: %v", err)
		}
	})
}

func TestAnUnknownSiteIsRefusedAndRepackagingKeepsOnlyTheSameArchive(t *testing.T) {
	r, from := repo(t, "v1.0.0"), built(t)
	if _, err := Package(testFleet(), Options{App: "other", Repo: r, From: from, Out: t.TempDir()}); err == nil || !strings.Contains(err.Error(), `no app "other"`) {
		t.Errorf("unknown app: %v", err)
	}
	out := t.TempDir()
	first, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: out})
	if err != nil || first.Existing {
		t.Fatalf("first: %+v, %v", first, err)
	}
	// Again: reproducible, so the same bytes, kept.
	again, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: out})
	if err != nil || !again.Existing || again.SHA256 != first.SHA256 {
		t.Errorf("again: %+v, %v", again, err)
	}
	// A different archive under the name is never written over.
	if err := os.WriteFile(first.Archive, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: out}); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Errorf("a different archive: %v", err)
	}
	// Half an earlier packaging is refused too.
	if err := os.Remove(first.Archive); err != nil {
		t.Fatal(err)
	}
	if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: out}); err == nil || !strings.Contains(err.Error(), "only part") {
		t.Errorf("half a packaging: %v", err)
	}
}

// infra deploy makes these checks before it sends a release anywhere; an
// archive this package writes must pass them.
func TestTheArchivePassesTheDeploysChecks(t *testing.T) {
	res, err := Package(testFleet(), Options{App: "one-cloud", Repo: repo(t, "v1.0.0"), From: built(t), Out: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := archive.Check(res.Archive, "one-cloud", "")
	if err != nil {
		t.Fatalf("the deploy's checks rejected the archive: %v", err)
	}
	if got.Revision != res.Revision || got.SHA256 != res.SHA256 {
		t.Errorf("the deploy read %+v, the packager wrote %+v", got, res)
	}
}

func TestALocalBuildNamesItsInputs(t *testing.T) {
	sf := provenance.Input{Repo: "github.com/plexusone/systemforge", Commit: "0123456789abcdef0123456789abcdef01234567"}
	cf := provenance.Input{Repo: "github.com/plexusone/connectforge", Commit: "fedcba9876543210fedcba9876543210fedcba98"}
	r, from := repo(t, ""), built(t)
	res, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir(), AllowUntagged: true,
		Inputs: []provenance.Input{sf, cf}})
	if err != nil {
		t.Fatal(err)
	}
	rev := gitIn(t, r, "rev-parse", "--short=12", "HEAD")
	// Sorted by repository, after the app's own line, which the deploy reads.
	want := "one-cloud " + rev + "\ninput " + cf.Repo + " " + cf.Commit + "\ninput " + sf.Repo + " " + sf.Commit + "\n"
	if got := entries(t, res.Archive)["REVISION"]; got != want {
		t.Errorf("REVISION = %q, want %q", got, want)
	}
	// The name tells this build apart from the app commit built without them.
	if filepath.Base(res.Archive) != "one-cloud-"+rev+"-"+provenance.Digest(res.Inputs)+".tgz" {
		t.Errorf("archive = %s", filepath.Base(res.Archive))
	}
	if got, err := archive.Check(res.Archive, "one-cloud", ""); err != nil || got.Revision != rev {
		t.Fatalf("the deploy's checks: %+v, %v", got, err)
	}

	for name, ins := range map[string][]provenance.Input{
		"a short commit":     {{Repo: sf.Repo, Commit: sf.Commit[:12]}},
		"a repository twice": {sf, {Repo: sf.Repo, Commit: cf.Commit}},
		"not host/org/repo":  {{Repo: "systemforge", Commit: sf.Commit}},
	} {
		if _, err := Package(testFleet(), Options{App: "one-cloud", Repo: r, From: from, Out: t.TempDir(), AllowUntagged: true, Inputs: ins}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
