package cfranges

import (
	"net/netip"
	"strings"
	"testing"
)

func TestTheCommittedRangesParse(t *testing.T) {
	r, err := Ranges()
	if err != nil {
		t.Fatal(err)
	}
	var four, six int
	for _, p := range r {
		if p.Addr().Is4() {
			four++
		} else {
			six++
		}
	}
	if four == 0 || six == 0 {
		t.Fatalf("%d IPv4 and %d IPv6 ranges", four, six)
	}
}

func TestParseChecksEveryLine(t *testing.T) {
	for text, want := range map[string]string{
		"173.245.48.0/20\nnot-a-range\n": "line 2",
		"2400:cb00::/32\n":               "not an IPv4 range",
		"173.245.48.1/20\n":              "not canonical; want 173.245.48.0/20",
		"# only a comment\n\n":           "no IPv4 ranges",
	} {
		if _, err := Parse(text, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", text, err, want)
		}
	}
	got, err := Parse("# header\n173.245.48.0/20\n\n103.21.244.0/22\n", false)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestFormatRoundTrips(t *testing.T) {
	in := []netip.Prefix{netip.MustParsePrefix("2400:cb00::/32"), netip.MustParsePrefix("2606:4700::/32")}
	out, err := Parse(Format(in, URLv6), true)
	if err != nil || len(out) != 2 || out[0] != in[0] {
		t.Fatalf("got %v, %v", out, err)
	}
}

func TestDiff(t *testing.T) {
	a, b, c := netip.MustParsePrefix("1.0.0.0/8"), netip.MustParsePrefix("2.0.0.0/8"), netip.MustParsePrefix("3.0.0.0/8")
	added, removed := Diff([]netip.Prefix{a, b}, []netip.Prefix{b, c})
	if len(added) != 1 || added[0] != c || len(removed) != 1 || removed[0] != a {
		t.Fatalf("added %v, removed %v", added, removed)
	}
}
