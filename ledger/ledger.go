// Package ledger records every deploy and rollback of each app stack: what
// release is running, since when, who put it there, and what it replaced. It is
// an append-only file per stack, committed with the repository, so "what is
// running, and since when" is history in git.
package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/provenance"
)

// Action is what an entry records.
type Action string

// Actions.
const (
	// Deploy puts a new release into service.
	Deploy Action = "deploy"
	// Rollback puts an earlier release back into service.
	Rollback Action = "rollback"
)

// Entry is one deploy or rollback of one stack.
type Entry struct {
	// Time is when the release went live, in UTC.
	Time time.Time `json:"time"`
	// App is the app's name.
	App string `json:"app"`
	// Stack is the stack's name.
	Stack  string `json:"stack"`
	Action Action `json:"action"`
	// Release is the release directory now live: <UTC timestamp>-<revision>.
	Release string `json:"release"`
	// Revision is the host repository's commit the release was built from.
	Revision string `json:"revision"`
	// Tag is the host repository's release tag, when it has one.
	Tag string `json:"tag,omitempty"`
	// SHA256 is the release archive's checksum. A deploy needs it; a rollback
	// reuses a release already on the host.
	SHA256 string `json:"sha256,omitempty"`
	// Previous is the release that was live before, empty for a stack's first deploy.
	Previous string `json:"previous,omitempty"`
	// Operator is who ran the deploy.
	Operator string `json:"operator"`
	// Inputs are the upstream commits a deploy built from local commits
	// used; empty for a release built from the app's go.mod alone.
	Inputs []provenance.Input `json:"inputs,omitempty"`
	// Selfcheck is the deploy's self-check outcome, when it ran.
	Selfcheck *Selfcheck `json:"selfcheck,omitempty"`
}

// Selfcheck is each stage's outcome and the names of the checks that
// failed, never their details, which could quote configuration.
type Selfcheck struct {
	// Mode is enforce or report.
	Mode string `json:"mode"`
	// Pre and Post are pass or fail; Post is empty when the stage did not run.
	Pre    string   `json:"pre"`
	Post   string   `json:"post,omitempty"`
	Failed []string `json:"failed,omitempty"`
}

var (
	releasePattern  = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{7,40}$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tagPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	checkPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Path is the file for one stack's ledger: <dir>/<unit>.jsonl.
func Path(dir string, f *fleet.Fleet, a fleet.App, stack string) string {
	return filepath.Join(dir, f.Unit(a, stack)+".jsonl")
}

// Validate reports every way an entry is malformed on its own, without
// reference to the history.
func (e Entry) Validate(f *fleet.Fleet) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf("ledger: "+format, a...)) }
	if e.Time.IsZero() || e.Time.Location() != time.UTC {
		bad("time must be set, in UTC")
	}
	if _, _, err := f.Stack(e.App, e.Stack); err != nil {
		bad("%v", err)
	}
	switch e.Action {
	case Deploy:
		if !sha256Pattern.MatchString(e.SHA256) {
			bad("a deploy needs the archive's sha256 as 64 lowercase hex digits")
		}
	case Rollback:
		if e.Previous == "" {
			bad("a rollback needs the release it replaced")
		}
		if e.SHA256 != "" && !sha256Pattern.MatchString(e.SHA256) {
			bad("sha256 must be 64 lowercase hex digits")
		}
	default:
		bad("action %q must be %s or %s", e.Action, Deploy, Rollback)
	}
	if !releasePattern.MatchString(e.Release) {
		bad("release %q must be <UTC timestamp>-<revision>, such as 20260101T000000Z-abc1234", e.Release)
	}
	if e.Previous != "" && !releasePattern.MatchString(e.Previous) {
		bad("previous %q must be a release name", e.Previous)
	}
	if !revisionPattern.MatchString(e.Revision) {
		bad("revision %q must be a commit hash of 7 to 40 lowercase hex digits", e.Revision)
	}
	if e.Tag != "" && !tagPattern.MatchString(e.Tag) {
		bad("tag %q is not a plain tag name", e.Tag)
	}
	if strings.TrimSpace(e.Operator) == "" || strings.ContainsAny(e.Operator, "\r\n\t") {
		bad("operator must name who ran the deploy, on one line")
	}
	if len(e.Inputs) > 0 && e.Action != Deploy {
		bad("only a deploy records inputs; a rollback reuses a release")
	}
	if sc := e.Selfcheck; sc != nil {
		if e.Action != Deploy {
			bad("only a deploy records a self-check")
		}
		if sc.Mode != "enforce" && sc.Mode != "report" {
			bad("self-check mode %q must be enforce or report", sc.Mode)
		}
		if sc.Pre != "pass" && sc.Pre != "fail" {
			bad("self-check pre %q must be pass or fail", sc.Pre)
		}
		if sc.Post != "" && sc.Post != "pass" && sc.Post != "fail" {
			bad("self-check post %q must be pass or fail", sc.Post)
		}
		for _, name := range sc.Failed {
			if !checkPattern.MatchString(name) {
				bad("self-check failure %q is not app/check", name)
			}
		}
	}
	seen := map[string]bool{}
	for _, in := range e.Inputs {
		if err := in.Validate(); err != nil {
			bad("input: %v", err)
		}
		if seen[in.Repo] {
			bad("input %s is named twice", in.Repo)
		}
		seen[in.Repo] = true
	}
	return errors.Join(errs...)
}

// Read returns a ledger's entries, oldest first. A file that does not exist is
// an empty ledger. A line that is not an entry is an error that names its line.
func Read(path string) ([]Entry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller names the ledger
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.DisallowUnknownFields()
		var e Entry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("ledger: %s line %d: %w", path, n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Current is the release now live: the last entry's. ok is false for a stack
// that was never deployed.
func Current(entries []Entry) (Entry, bool) {
	if len(entries) == 0 {
		return Entry{}, false
	}
	return entries[len(entries)-1], true
}

// Record checks an entry against the stack's history and appends it. The
// ledger stays trustworthy by refusing what cannot be true:
//
//   - a deploy must follow the release now live (Previous names it);
//   - a rollback must name the release now live as the one it replaced, and
//     return to a release that was deployed earlier;
//   - an entry cannot be dated before the one it follows.
//
// The ledger is only ever appended to.
func Record(f *fleet.Fleet, dir string, e Entry) error {
	e.Time = e.Time.UTC().Truncate(time.Second)
	if err := e.Validate(f); err != nil {
		return err
	}
	a, _ := f.App(e.App) // Validate found it
	path := Path(dir, f, a, e.Stack)
	history, err := Read(path)
	if err != nil {
		return err
	}
	if cur, ok := Current(history); ok {
		if e.Time.Before(cur.Time) {
			return fmt.Errorf("ledger: %s/%s: entry at %s is before the last one at %s", e.App, e.Stack, e.Time.Format(time.RFC3339), cur.Time.Format(time.RFC3339))
		}
		if e.Previous != cur.Release {
			return fmt.Errorf("ledger: %s/%s: previous is %q but the release now live is %q", e.App, e.Stack, e.Previous, cur.Release)
		}
		if e.Release == cur.Release {
			return fmt.Errorf("ledger: %s/%s: %s is already live", e.App, e.Stack, e.Release)
		}
	} else if e.Action == Rollback {
		return fmt.Errorf("ledger: %s/%s: nothing has been deployed, so there is nothing to roll back", e.App, e.Stack)
	} else if e.Previous != "" {
		return fmt.Errorf("ledger: %s/%s: previous is %q but nothing has been deployed", e.App, e.Stack, e.Previous)
	}
	if e.Action == Rollback {
		deployed := false
		for _, h := range history {
			if h.Action == Deploy && h.Release == e.Release {
				deployed = true
			}
		}
		if !deployed {
			return fmt.Errorf("ledger: %s/%s: %s was never deployed, so it cannot be rolled back to", e.App, e.Stack, e.Release)
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: the ledger is a committed file, meant to be readable
		return fmt.Errorf("ledger: %w", err)
	}
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G302,G304: a committed file, meant to be readable
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if _, err := fh.Write(append(line, '\n')); err != nil {
		_ = fh.Close() // the write error is the one to report
		return fmt.Errorf("ledger: %w", err)
	}
	return fh.Close()
}
