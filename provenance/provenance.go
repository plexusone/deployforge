// Package provenance names what a release was built from besides the app's
// own commit: the upstream repositories a local build took from their local
// commits instead of from the versions the app's go.mod requires.
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Input is one upstream repository and the commit a release was built with.
type Input struct {
	// Repo is host/org/repo, such as github.com/plexusone/systemforge.
	Repo string `json:"repo"`
	// Commit is the full commit.
	Commit string `json:"commit"`
}

var (
	repoPattern   = regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]{2,}/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Validate reports whether the input is a repository and a full commit.
func (i Input) Validate() error {
	if !repoPattern.MatchString(i.Repo) {
		return fmt.Errorf("provenance: %q is not a repository such as github.com/org/repo", i.Repo)
	}
	if !commitPattern.MatchString(i.Commit) {
		return fmt.Errorf("provenance: %s's commit %q is not 40 lowercase hex digits", i.Repo, i.Commit)
	}
	return nil
}

// String is the input as Parse reads it: repo@commit.
func (i Input) String() string { return i.Repo + "@" + i.Commit }

// Parse reads repo@commit.
func Parse(s string) (Input, error) {
	repo, commit, ok := strings.Cut(strings.TrimSpace(s), "@")
	if !ok {
		return Input{}, fmt.Errorf("provenance: %q is not repo@commit", s)
	}
	in := Input{Repo: repo, Commit: commit}
	return in, in.Validate()
}

// ParseAll reads each repo@commit, and refuses a repository named twice.
func ParseAll(ss []string) ([]Input, error) {
	ins := make([]Input, 0, len(ss))
	seen := map[string]bool{}
	for _, s := range ss {
		in, err := Parse(s)
		if err != nil {
			return nil, err
		}
		if seen[in.Repo] {
			return nil, fmt.Errorf("provenance: %s is named twice", in.Repo)
		}
		seen[in.Repo] = true
		ins = append(ins, in)
	}
	Sort(ins)
	return ins, nil
}

// Sort orders inputs by repository, so the same inputs read the same.
func Sort(ins []Input) {
	sort.Slice(ins, func(a, b int) bool { return ins[a].Repo < ins[b].Repo })
}

// Digest is a short hash of sorted inputs, which tells two builds of the same
// app commit apart; empty for none.
func Digest(ins []Input) string {
	if len(ins) == 0 {
		return ""
	}
	sorted := append([]Input(nil), ins...)
	Sort(sorted)
	var b strings.Builder
	for _, in := range sorted {
		b.WriteString(in.String() + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:8]
}
