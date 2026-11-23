package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	oscfs "github.com/grokify/oscompat/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	mode       int64
	typ        byte
	link       string
}

// build writes a gzipped tar of entries and returns its path and checksum.
func build(t *testing.T, entries ...entry) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Mode: mode, Size: int64(len(e.body)), Typeflag: typ, Linkname: e.link}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "one-cloud.tgz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := Sum(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, sum
}

func good() []entry {
	return []entry{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "./REVISION", body: "one-cloud abc1234def56\nv1.0.0\n"},
		{name: "./one-cloud", body: "#!/bin/sh\n", mode: 0o755},
		{name: "./web/", typ: tar.TypeDir, mode: 0o755},
		{name: "./web/index.html", body: "<html></html>"},
	}
}

func TestCheckReturnsTheChecksumAndTheRevision(t *testing.T) {
	p, sum := build(t, good()...)
	got, err := Check(p, "one-cloud", sum)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != sum || got.Revision != "abc1234def56" {
		t.Errorf("got %+v", got)
	}
	// Entries without a leading "./", as infra release package writes them.
	p2, sum2 := build(t, entry{name: "REVISION", body: "one-cloud abc1234\n"}, entry{name: "one-cloud", body: "x", mode: 0o755})
	if _, err := Check(p2, "one-cloud", strings.ToUpper(sum2)); err != nil {
		t.Errorf("plain entry names and an upper-case checksum: %v", err)
	}
}

func TestCheckReadsTheChecksumFileBesideTheArchive(t *testing.T) {
	p, sum := build(t, good()...)
	if _, err := Check(p, "one-cloud", ""); err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Errorf("no checksum anywhere: %v", err)
	}
	if err := os.WriteFile(p+".sha256", []byte(sum+"  one-cloud.tgz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(p, "one-cloud", ""); err != nil {
		t.Errorf("checksum file: %v", err)
	}
}

func TestCheckRefusesAnArchiveThatIsNotThisSitesRelease(t *testing.T) {
	_, otherSum := build(t, good()...)
	for name, c := range map[string]struct {
		entries []entry
		sum     string
	}{
		"a wrong checksum":       {good(), strings.Repeat("0", 64)},
		"a malformed checksum":   {good(), "abc"},
		"another app's revision": {[]entry{{name: "REVISION", body: "other-cloud abc1234\n"}, {name: "one-cloud", body: "x", mode: 0o755}}, ""},
		"no commit":              {[]entry{{name: "REVISION", body: "one-cloud\n"}, {name: "one-cloud", body: "x", mode: 0o755}}, ""},
		"a bad commit":           {[]entry{{name: "REVISION", body: "one-cloud HEAD\n"}, {name: "one-cloud", body: "x", mode: 0o755}}, ""},
		"no REVISION":            {[]entry{{name: "one-cloud", body: "x", mode: 0o755}}, ""},
		"no binary":              {[]entry{{name: "REVISION", body: "one-cloud abc1234\n"}}, ""},
		"the binary in a folder": {[]entry{{name: "REVISION", body: "one-cloud abc1234\n"}, {name: "bin/one-cloud", body: "x", mode: 0o755}}, ""},
	} {
		p, sum := build(t, c.entries...)
		if c.sum != "" {
			sum = c.sum
		}
		if _, err := Check(p, "one-cloud", sum); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Check(filepath.Join(t.TempDir(), "nope.tgz"), "one-cloud", otherSum); err == nil {
		t.Error("a missing archive: accepted")
	}
}

func TestExtractWritesFilesWithNormalizedModes(t *testing.T) {
	p, _ := build(t, append(good(), entry{name: "./web/app.js", body: "x", mode: 0o600})...)
	dir := filepath.Join(t.TempDir(), "rel")
	if err := Extract(p, dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"one-cloud": 0o755, "web/index.html": 0o644, "web/app.js": 0o644, "REVISION": 0o644} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if oscfs.PermissionsEnforced() && st.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", name, st.Mode().Perm(), want)
		}
	}
	if err := Extract(p, dir); err == nil {
		t.Error("extracting over an existing release succeeded")
	}
}

func TestExtractRefusesEntriesOutsideTheReleaseAndLinks(t *testing.T) {
	for name, e := range map[string]entry{
		"a parent path":   {name: "../evil", body: "x"},
		"a nested escape": {name: "web/../../evil", body: "x"},
		"an absolute":     {name: "/etc/evil", body: "x"},
		"a symlink":       {name: "link", typ: tar.TypeSymlink, link: "/etc/passwd"},
		"a hard link":     {name: "hard", typ: tar.TypeLink, link: "REVISION"},
	} {
		p, _ := build(t, entry{name: "REVISION", body: "one-cloud abc1234\n"}, e)
		dir := filepath.Join(t.TempDir(), "rel")
		if err := Extract(p, dir); err == nil {
			t.Errorf("%s: extracted", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
			t.Errorf("%s: wrote outside the release", name)
		}
	}
}

func TestCheckReadsALocalBuildsInputs(t *testing.T) {
	rev := "one-cloud abc1234def56\ninput github.com/plexusone/systemforge 0123456789abcdef0123456789abcdef01234567\ninput github.com/plexusone/connectforge fedcba9876543210fedcba9876543210fedcba98\n"
	p, sum := build(t, entry{name: "REVISION", body: rev}, entry{name: "one-cloud", body: "x", mode: 0o755})
	got, err := Check(p, "one-cloud", sum)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Inputs) != 2 || got.Inputs[0].Repo != "github.com/plexusone/connectforge" || got.Inputs[1].Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("inputs = %+v", got.Inputs)
	}
	for name, body := range map[string]string{
		"a short commit":     "one-cloud abc1234\ninput github.com/plexusone/systemforge abc1234\n",
		"a missing commit":   "one-cloud abc1234\ninput github.com/plexusone/systemforge\n",
		"a repository twice": "one-cloud abc1234\ninput github.com/a/b 0123456789abcdef0123456789abcdef01234567\ninput github.com/a/b fedcba9876543210fedcba9876543210fedcba98\n",
	} {
		p, sum := build(t, entry{name: "REVISION", body: body}, entry{name: "one-cloud", body: "x", mode: 0o755})
		if _, err := Check(p, "one-cloud", sum); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
