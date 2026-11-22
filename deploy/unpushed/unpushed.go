// Package unpushed finds the commits a live release was built from that are
// on no remote branch: what a stack runs that exists only on the operator's
// machine until the next push, as after a local build.
package unpushed

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/plexusone/deployforge/fleet"
	"github.com/plexusone/deployforge/ledger"
)

// Commit is one commit a live release was built from.
type Commit struct {
	// App and Stack name the stack running it, and Release its release.
	App, Stack, Release string
	// Repo is host/org/repo, and Commit the commit as the ledger records it.
	Repo, Commit string
	// Input is true for an upstream, false for the app's own commit.
	Input bool
	// Pushed is true when a remote branch of the checkout contains it.
	Pushed bool
	// Err says why it could not be checked, such as no checkout.
	Err error
}

// Options say where to look.
type Options struct {
	// LedgerDir holds the ledger files.
	LedgerDir string
	// Src is the directory checkouts are under, as <Src>/<repo>.
	Src string
	// Fetch fetches each checkout's origin first, so remote branches are current.
	Fetch bool
}

// Live returns every commit each stack's live release was built from, and
// whether each is on a remote branch.
func Live(f *fleet.Fleet, o Options) ([]Commit, error) {
	fetched := map[string]error{}
	var out []Commit
	for _, a := range f.Apps {
		for _, stack := range f.StackNames(a) {
			entries, err := ledger.Read(ledger.Path(o.LedgerDir, f, a, stack))
			if err != nil {
				return nil, err
			}
			cur, ok := ledger.Current(entries)
			if !ok {
				continue
			}
			// A rollback reuses a release: its inputs are its deploy's.
			deploy, ok := deployOf(entries, cur.Release)
			if !ok {
				return nil, fmt.Errorf("unpushed: %s/%s runs %s, which the ledger never deployed", a.Name, stack, cur.Release)
			}
			check := func(repo, commit string, input bool) {
				c := Commit{App: a.Name, Stack: stack, Release: cur.Release, Repo: repo, Commit: commit, Input: input}
				dir := filepath.Join(o.Src, filepath.FromSlash(repo))
				if o.Fetch {
					if _, done := fetched[dir]; !done {
						_, fetched[dir] = git(dir, "fetch", "--quiet", "origin")
					}
					if err := fetched[dir]; err != nil {
						c.Err = fmt.Errorf("fetching %s: %w", dir, err)
						out = append(out, c)
						return
					}
				}
				c.Pushed, c.Err = onRemote(dir, commit)
				out = append(out, c)
			}
			check(a.Repo, deploy.Revision, false)
			for _, in := range deploy.Inputs {
				check(in.Repo, in.Commit, true)
			}
		}
	}
	return out, nil
}

func deployOf(entries []ledger.Entry, release string) (ledger.Entry, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Action == ledger.Deploy && entries[i].Release == release {
			return entries[i], true
		}
	}
	return ledger.Entry{}, false
}

// onRemote reports whether a remote-tracking branch of the checkout in dir
// contains commit.
func onRemote(dir, commit string) (bool, error) {
	full, err := git(dir, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
	if err != nil {
		return false, fmt.Errorf("%s has no commit %s; is it the right checkout?", dir, commit)
	}
	branches, err := git(dir, "branch", "--remotes", "--contains", full)
	if err != nil {
		return false, err
	}
	return branches != "", nil
}

func git(dir string, args ...string) (string, error) {
	var b bytes.Buffer
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: fixed git subcommands
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	out := strings.TrimSpace(b.String())
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return out, nil
}
