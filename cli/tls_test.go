package cli

import (
	"strings"
	"testing"

	"github.com/plexusone/deployforge/fleet"
)

func TestLiveHostNames(t *testing.T) {
	f, err := fleet.Load(testFleet)
	if err != nil {
		t.Fatal(err)
	}
	got := liveHostNames(f)
	if len(got) == 0 || !strings.HasSuffix(got[0], "example.com") {
		t.Errorf("live host names %v", got)
	}
}
