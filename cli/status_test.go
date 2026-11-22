package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/deploy/onhost"
	"github.com/plexusone/deployforge/ledger"
)

func TestWriteStatus(t *testing.T) {
	const a, b = "20261023T180000Z-aaaaaaa", "20261024T090000Z-bbbbbbb"
	host := &onhost.Status{Live: b, Releases: []string{a, b}}
	for name, tc := range map[string]struct {
		st      *onhost.Status
		entries []ledger.Entry
		want    string
		fails   bool
	}{
		"agrees":         {host, []ledger.Entry{{Release: a}, {Release: b}}, "ledger    agrees", false},
		"disagrees":      {host, []ledger.Entry{{Release: a}}, "says " + a, true},
		"no ledger":      {host, nil, "ledger    empty", true},
		"never deployed": {&onhost.Status{}, nil, "nothing is live", false},
		"one release":    {&onhost.Status{Live: a, Releases: []string{a}}, []ledger.Entry{{Release: a}}, "nothing to roll back to", false},
		"not audited":    {host, []ledger.Entry{{Release: b}}, "deployforge has not deployed", false},
	} {
		var out bytes.Buffer
		err := writeStatus(&out, "example-app", "prod", tc.st, tc.entries)
		if (err != nil) != tc.fails || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: %v\n%s", name, err, out.String())
		}
	}
}
