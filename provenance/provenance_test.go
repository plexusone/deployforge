package provenance

import (
	"strings"
	"testing"
)

const commitA = "0123456789abcdef0123456789abcdef01234567"
const commitB = "fedcba9876543210fedcba9876543210fedcba98"

func TestParseReadsRepoAtCommit(t *testing.T) {
	in, err := Parse("github.com/plexusone/systemforge@" + commitA)
	if err != nil {
		t.Fatal(err)
	}
	if in.Repo != "github.com/plexusone/systemforge" || in.Commit != commitA || in.String() != "github.com/plexusone/systemforge@"+commitA {
		t.Fatalf("parsed %+v", in)
	}
}

func TestParseRefusesWhatCannotBeRebuilt(t *testing.T) {
	for _, s := range []string{
		"github.com/plexusone/systemforge",                 // no commit
		"github.com/plexusone/systemforge@main",            // a branch moves
		"github.com/plexusone/systemforge@" + commitA[:12], // short commits can collide
		"github.com/plexusone/systemforge@" + strings.ToUpper(commitA),
		"systemforge@" + commitA,    // not host/org/repo
		"../systemforge@" + commitA, // a path
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted it", s)
		}
	}
}

func TestParseAllSortsAndRefusesARepoTwice(t *testing.T) {
	ins, err := ParseAll([]string{"github.com/b/two@" + commitB, "github.com/a/one@" + commitA})
	if err != nil {
		t.Fatal(err)
	}
	if ins[0].Repo != "github.com/a/one" || ins[1].Repo != "github.com/b/two" {
		t.Fatalf("not sorted: %v", ins)
	}
	if _, err := ParseAll([]string{"github.com/a/one@" + commitA, "github.com/a/one@" + commitB}); err == nil {
		t.Fatal("a repository named twice was accepted")
	}
}

func TestDigestDependsOnTheInputsNotTheirOrder(t *testing.T) {
	a := []Input{{"github.com/a/one", commitA}, {"github.com/b/two", commitB}}
	b := []Input{a[1], a[0]}
	if Digest(a) != Digest(b) || len(Digest(a)) != 8 {
		t.Fatalf("digests %q and %q", Digest(a), Digest(b))
	}
	if Digest(a) == Digest([]Input{{"github.com/a/one", commitB}, a[1]}) {
		t.Fatal("different commits gave the same digest")
	}
	if Digest(nil) != "" {
		t.Fatal("no inputs should have no digest")
	}
	if a[0].Repo != "github.com/a/one" {
		t.Fatal("Digest reordered its argument")
	}
}
