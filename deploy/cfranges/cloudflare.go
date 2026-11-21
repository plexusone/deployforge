// Package cfranges holds Cloudflare's published proxy address ranges,
// which Caddy trusts to set a request's client address. The
// ranges are committed in ips-v4.txt and ips-v6.txt, one CIDR a line, and
// change only by review: Fetch reads Cloudflare's published lists so they
// can be refreshed or checked.
package cfranges

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// Where Cloudflare publishes its ranges.
const (
	URLv4 = "https://www.cloudflare.com/ips-v4"
	URLv6 = "https://www.cloudflare.com/ips-v6"
)

// The committed lists.
var (
	//go:embed ips-v4.txt
	v4 string
	//go:embed ips-v6.txt
	v6 string
)

// Ranges returns the committed ranges, IPv4 then IPv6, in file order.
func Ranges() ([]netip.Prefix, error) {
	a, err := Parse(v4, false)
	if err != nil {
		return nil, fmt.Errorf("cfranges: ips-v4.txt: %w", err)
	}
	b, err := Parse(v6, true)
	if err != nil {
		return nil, fmt.Errorf("cfranges: ips-v6.txt: %w", err)
	}
	return append(a, b...), nil
}

// Parse reads one CIDR a line, skipping blank lines and lines starting with
// #. Every range must be in its canonical form, of the family asked for.
func Parse(text string, ipv6 bool) ([]netip.Prefix, error) {
	var out []netip.Prefix
	var errs []error
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := netip.ParsePrefix(line)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("line %d: %w", i+1, err))
		case p.Addr().Is6() != ipv6 || p.Addr().Is4In6():
			errs = append(errs, fmt.Errorf("line %d: %s is not an %s range", i+1, line, family(ipv6)))
		case p.Masked() != p:
			errs = append(errs, fmt.Errorf("line %d: %s is not canonical; want %s", i+1, line, p.Masked()))
		default:
			out = append(out, p)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no %s ranges", family(ipv6))
	}
	return out, nil
}

func family(ipv6 bool) string {
	if ipv6 {
		return "IPv6"
	}
	return "IPv4"
}

// Format writes ranges as a list file, with a header naming its source.
func Format(ranges []netip.Prefix, source string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Cloudflare's proxy address ranges, from %s.\n", source)
	b.WriteString("# Refresh with `make cloudflare-ips`; do not edit by hand.\n")
	for _, p := range ranges {
		b.WriteString(p.String())
		b.WriteString("\n")
	}
	return b.String()
}

// Fetch reads Cloudflare's published IPv4 and IPv6 lists.
func Fetch(ctx context.Context, client *http.Client) (v4, v6 []netip.Prefix, err error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if v4, err = fetch(ctx, client, URLv4, false); err != nil {
		return nil, nil, err
	}
	if v6, err = fetch(ctx, client, URLv6, true); err != nil {
		return nil, nil, err
	}
	return v4, v6, nil
}

func fetch(ctx context.Context, client *http.Client, url string, ipv6 bool) ([]netip.Prefix, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("cfranges: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cfranges: fetch %s: %w", url, err)
	}
	defer func() {
		// The body was read in full or the request failed; a close error
		// changes neither result.
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cfranges: fetch %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("cfranges: read %s: %w", url, err)
	}
	ranges, err := Parse(string(body), ipv6)
	if err != nil {
		return nil, fmt.Errorf("cfranges: %s: %w", url, err)
	}
	return ranges, nil
}

// Diff reports ranges in want but not have, and in have but not want.
func Diff(have, want []netip.Prefix) (added, removed []netip.Prefix) {
	for _, p := range want {
		if !slices.Contains(have, p) {
			added = append(added, p)
		}
	}
	for _, p := range have {
		if !slices.Contains(want, p) {
			removed = append(removed, p)
		}
	}
	return added, removed
}
