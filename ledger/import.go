package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plexusone/deployforge/fleet"
)

// Import reads another ledger's JSON Lines for one stack, renaming the
// field appField to app when it is not app, and writes them as the stack's
// ledger in dir. Each entry is checked as Record checks it, in order, so an
// imported ledger is as trustworthy as one deployforge wrote. The stack's
// ledger must be empty, or be the file imported, which is then replaced
// only once every entry passes. It returns the number of entries.
func Import(f *fleet.Fleet, dir, app, stack, src, appField string) (n int, err error) {
	a, _, err := f.Stack(app, stack)
	if err != nil {
		return 0, fmt.Errorf("ledger: %w", err)
	}
	if appField == "" {
		appField = "app"
	}
	dest := Path(dir, f, a, stack)
	same, err := sameFile(src, dest)
	if err != nil {
		return 0, err
	}
	if !same {
		existing, err := Read(dest)
		if err != nil {
			return 0, err
		}
		if len(existing) > 0 {
			return 0, fmt.Errorf("ledger: %s already has %d entries; import only into an empty ledger", dest, len(existing))
		}
	}
	data, err := os.ReadFile(src) //nolint:gosec // G304: the operator names the ledger to import
	if err != nil {
		return 0, fmt.Errorf("ledger: %w", err)
	}
	entries, err := convert(data, appField)
	if err != nil {
		return 0, fmt.Errorf("ledger: %s %w", src, err)
	}
	tmp, err := os.MkdirTemp(dir, ".import-")
	if err != nil {
		return 0, fmt.Errorf("ledger: %w", err)
	}
	defer func() {
		// Only the temporary directory is removed; the ledger is renamed out
		// of it first when the import succeeds.
		if rerr := os.RemoveAll(tmp); rerr != nil && err == nil {
			err = fmt.Errorf("ledger: removing %s: %w", tmp, rerr)
		}
	}()
	for i, e := range entries {
		if e.App != app || e.Stack != stack {
			return 0, fmt.Errorf("ledger: %s line %d is %s/%s, not %s/%s", src, i+1, e.App, e.Stack, app, stack)
		}
		if err := Record(f, tmp, e); err != nil {
			return 0, fmt.Errorf("ledger: %s line %d: %w", src, i+1, err)
		}
	}
	if len(entries) == 0 {
		return 0, fmt.Errorf("ledger: %s has no entries", src)
	}
	if err := os.Rename(Path(tmp, f, a, stack), dest); err != nil {
		return 0, fmt.Errorf("ledger: %w", err)
	}
	return len(entries), nil
}

// convert decodes each line, renaming appField to app, and refuses any
// other field an entry does not have.
func convert(data []byte, appField string) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if appField != "app" {
			v, ok := m[appField]
			if !ok {
				return nil, fmt.Errorf("line %d has no %q field", n, appField)
			}
			if _, dup := m["app"]; dup {
				return nil, fmt.Errorf("line %d has both %q and app", n, appField)
			}
			m["app"] = v
			delete(m, appField)
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var e Entry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// sameFile reports whether a and b are one file; a b that does not exist
// is not a.
func sameFile(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, fmt.Errorf("ledger: %w", err)
	}
	sb, err := os.Stat(b)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ledger: %w", err)
	}
	return os.SameFile(sa, sb) || filepath.Clean(a) == filepath.Clean(b), nil
}
