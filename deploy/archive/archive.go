// Package archive checks a release archive before a deploy sends it anywhere:
// its checksum, the revision it records, and that it holds the app's binary.
// It also unpacks one safely, on the host.
package archive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/plexusone/deployforge/provenance"
)

// Checked is what a deploy learns from a valid archive.
type Checked struct {
	// SHA256 is the archive's checksum, which the ledger records.
	SHA256 string
	// Revision is the commit REVISION names.
	Revision string
	// Inputs are the upstream commits a local build used, from REVISION's
	// input lines.
	Inputs []provenance.Input
}

var (
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// Check verifies an archive for an app. The checksum is want when given,
// otherwise the first field of <archive>.sha256; an archive nobody vouches for
// is refused. The archive's REVISION must begin with "<app> <commit>", and its
// root must hold a binary named for the app.
func Check(archivePath, app, want string) (Checked, error) {
	st, err := os.Stat(archivePath)
	if err != nil || !st.Mode().IsRegular() {
		return Checked{}, fmt.Errorf("archive: %s is not a release archive file", archivePath)
	}
	if want == "" {
		want, err = sumFile(archivePath + ".sha256")
		if err != nil {
			return Checked{}, err
		}
	}
	want = strings.ToLower(strings.TrimSpace(want))
	if !sha256Pattern.MatchString(want) {
		return Checked{}, fmt.Errorf("archive: the checksum must be 64 hex digits, got %q", want)
	}
	actual, err := Sum(archivePath)
	if err != nil {
		return Checked{}, err
	}
	if actual != want {
		return Checked{}, fmt.Errorf("archive: checksum mismatch for %s: expected %s, got %s", archivePath, want, actual)
	}

	revision, hasBinary, err := inspect(archivePath, app)
	if err != nil {
		return Checked{}, err
	}
	first, _, _ := strings.Cut(revision, "\n")
	name, rev, ok := strings.Cut(strings.TrimSpace(first), " ")
	if !ok || name != app {
		return Checked{}, fmt.Errorf("archive: REVISION must begin with %q, got %q", app+" <commit>", first)
	}
	rev = strings.TrimSpace(rev)
	if !revisionPattern.MatchString(rev) {
		return Checked{}, fmt.Errorf("archive: REVISION commit must be 7 to 40 lowercase hex digits, got %q", rev)
	}
	if !hasBinary {
		return Checked{}, fmt.Errorf("archive: no binary named %s at its root", app)
	}
	inputs, err := revisionInputs(revision)
	if err != nil {
		return Checked{}, err
	}
	return Checked{SHA256: actual, Revision: rev, Inputs: inputs}, nil
}

// sumFile reads the checksum from a .sha256 file: its first field.
func sumFile(p string) (string, error) {
	data, err := os.ReadFile(p) //nolint:gosec // G304: the operator names the archive; its checksum file sits beside it
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("archive: no checksum: give --sha256 or put it in %s", p)
	}
	if err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", fmt.Errorf("archive: %s is empty", p)
	}
	return fields[0], nil
}

// Sum is a file's SHA-256 as lowercase hex.
func Sum(p string) (string, error) {
	fh, err := os.Open(p) //nolint:gosec // G304: the operator names the archive
	if err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	defer fh.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// clean is an entry's name relative to the archive's root, without a leading
// "./", or "" for the root itself.
func clean(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimSuffix(name, "/")
	if name == "." {
		return ""
	}
	return name
}

// revisionInputs reads REVISION's "input <repo> <commit>" lines. Other lines,
// such as the tag, are not inputs.
func revisionInputs(revision string) ([]provenance.Input, error) {
	var ss []string
	for _, line := range strings.Split(revision, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "input" {
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("archive: REVISION line %q is not \"input <repo> <commit>\"", line)
		}
		ss = append(ss, f[1]+"@"+f[2])
	}
	if len(ss) == 0 {
		return nil, nil
	}
	ins, err := provenance.ParseAll(ss)
	if err != nil {
		return nil, fmt.Errorf("archive: REVISION: %w", err)
	}
	return ins, nil
}

func inspect(archivePath, app string) (revision string, hasBinary bool, err error) {
	err = walk(archivePath, func(h *tar.Header, r io.Reader) error {
		switch name := clean(h.Name); {
		case name == "REVISION" && h.Typeflag == tar.TypeReg:
			b, err := io.ReadAll(io.LimitReader(r, 4096))
			if err != nil {
				return err
			}
			revision = string(b)
		case name == app && h.Typeflag == tar.TypeReg:
			hasBinary = true
		}
		return nil
	})
	return revision, hasBinary, err
}

func walk(archivePath string, fn func(*tar.Header, io.Reader) error) error {
	fh, err := os.Open(archivePath) //nolint:gosec // G304: the operator names the archive
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	defer fh.Close() //nolint:errcheck // read-only
	gz, err := gzip.NewReader(bufio.NewReader(fh))
	if err != nil {
		return fmt.Errorf("archive: %s is not a gzipped tar: %w", archivePath, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: %s: %w", archivePath, err)
		}
		if err := fn(h, tr); err != nil {
			return fmt.Errorf("archive: %s: %w", archivePath, err)
		}
	}
}

// Extract unpacks an archive into dir, which must not exist yet. It writes only
// directories and regular files, refuses any entry that would land outside
// dir, and normalizes modes: directories 0755, files 0644, or 0755 when any
// execute bit is set. Ownership is the caller's, as the process writes it.
func Extract(archivePath, dir string) error {
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // G301: a release is readable by the service
		return fmt.Errorf("archive: %w", err)
	}
	return walk(archivePath, func(h *tar.Header, r io.Reader) error {
		name := clean(h.Name)
		if name == "" {
			return nil
		}
		if path.IsAbs(name) || strings.HasPrefix(path.Clean(name), "../") || path.Clean(name) == ".." || strings.Contains(name, "\\") {
			return fmt.Errorf("entry %q is outside the release", h.Name)
		}
		dest := filepath.Join(dir, filepath.FromSlash(path.Clean(name)))
		switch h.Typeflag {
		case tar.TypeDir:
			return os.MkdirAll(dest, 0o755) //nolint:gosec // G301: a release is readable by the service
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { //nolint:gosec // G301: a release is readable by the service
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // G304: dest is checked to be inside dir
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(r, h.Size)); err != nil { //nolint:gosec // G110: the archive's checksum was checked by the operator
				_ = out.Close() // the copy error is the one to report
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			return os.Chmod(dest, mode) // the process umask must not narrow it
		default:
			return fmt.Errorf("entry %q is not a file or a directory", h.Name)
		}
	})
}
