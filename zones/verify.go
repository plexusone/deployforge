package zones

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Outcome is what verifying a record found.
type Outcome string

// Outcomes.
const (
	OK   Outcome = "ok"
	Fail Outcome = "fail"
	Skip Outcome = "skip"
)

// Finding is one record's verification.
type Finding struct {
	Zone, Name, Type string
	Outcome          Outcome
	Detail           string
}

func (f Finding) String() string {
	return fmt.Sprintf("%-4s %s %s: %s", f.Outcome, f.Name, f.Type, f.Detail)
}

// errNoRecord is a record the zone does not answer.
var errNoRecord = errors.New("no record")

// Querier answers one DNS question from a zone's own name servers.
type Querier func(ctx context.Context, zone, name string, typ dnsmessage.Type) ([]dnsmessage.Resource, error)

// Verifier checks the zones' records as their name servers answer them.
type Verifier struct {
	// Query asks a zone's name servers; nil uses UDP to the first of the
	// zone's NS records, so no resolver's cache hides a change.
	Query Querier
	// Cloudflare are Cloudflare's address ranges, which a proxied record's
	// answers fall in.
	Cloudflare []netip.Prefix
	// HTTP makes the records' checks; nil uses a client that does not
	// follow redirects, with a 20-second timeout.
	HTTP *http.Client
	// RecordsOnly skips the records' HTTPS checks, for a check before a
	// planned change while a host is meant to be down.
	RecordsOnly bool
	// FailPlanned fails a planned record instead of skipping it, for the
	// check after a step that adds records: they must be marked present.
	FailPlanned bool
}

// Verify checks every record of the named zones, or of all zones when
// none is named. Planned records are skipped.
func (v *Verifier) Verify(ctx context.Context, d *Document, only ...string) ([]Finding, error) {
	var out []Finding
	matched := 0
	for _, z := range d.Zones {
		if len(only) > 0 && !slices.Contains(only, z.Name) {
			continue
		}
		matched++
		for _, r := range z.Records {
			out = append(out, v.record(ctx, z, r)...)
		}
	}
	if matched < len(only) {
		return out, fmt.Errorf("zones: not every zone of %s is in zones.json", strings.Join(only, ", "))
	}
	return out, nil
}

func (v *Verifier) record(ctx context.Context, z Zone, r Record) []Finding {
	fqdn := r.FQDN(z.Name)
	f := Finding{Zone: z.Name, Name: fqdn, Type: r.Type}
	if r.State() == Planned {
		// A planned record is not required yet, but one the zone already
		// answers differently would be overwritten or clash when the plan
		// is carried out, so it fails.
		_, err := v.dns(ctx, z, r, fqdn, &f)
		switch {
		case errors.Is(err, errNoRecord) && v.FailPlanned:
			f.Outcome, f.Detail = Fail, "planned, and the zone has no record: add it, then mark it present"
		case v.FailPlanned:
			f.Outcome, f.Detail = Fail, "planned: mark it present in zones.json"
		case errors.Is(err, errNoRecord):
			f.Outcome, f.Detail = Skip, "planned"
		case err == nil && r.Proxied:
			// A proxied name answers alike whatever is behind it.
			f.Outcome, f.Detail = Skip, "planned; the name already answers through Cloudflare"
		case err == nil:
			f.Outcome, f.Detail = Skip, "planned, and already present as listed: mark it present"
		default:
			f.Outcome, f.Detail = Fail, "planned, but the zone already differs: "+err.Error()
		}
		return []Finding{f}
	}
	f.Outcome, f.Detail = OK, ""
	addrs, err := v.dns(ctx, z, r, fqdn, &f)
	if err != nil {
		f.Outcome, f.Detail = Fail, err.Error()
	}
	out := []Finding{f}
	if r.Check != nil && f.Outcome == OK && !v.RecordsOnly {
		c := Finding{Zone: z.Name, Name: fqdn, Type: "HTTPS", Outcome: OK}
		detail, err := v.http(ctx, fqdn, addrs, r.Check)
		c.Detail = detail
		if err != nil {
			c.Outcome, c.Detail = Fail, err.Error()
		}
		out = append(out, c)
	}
	return out
}

// It returns the addresses the zone answered, for an A or AAAA record or a
// proxied one, so the HTTPS check reaches the same addresses.
func (v *Verifier) dns(ctx context.Context, z Zone, r Record, fqdn string, f *Finding) ([]string, error) {
	query := v.Query
	if query == nil {
		query = udpQuery
	}
	typ := map[string]dnsmessage.Type{"A": dnsmessage.TypeA, "AAAA": dnsmessage.TypeAAAA, "CNAME": dnsmessage.TypeCNAME, "MX": dnsmessage.TypeMX, "TXT": dnsmessage.TypeTXT}[r.Type]
	if r.Proxied {
		// Cloudflare answers a proxied record, of any type, with its own
		// addresses, and never shows the origin.
		typ = dnsmessage.TypeA
	}
	answers, err := query(ctx, z.Name, fqdn, typ)
	if err != nil {
		return nil, err
	}
	got := values(answers, typ)
	if len(got) == 0 {
		return nil, errNoRecord
	}
	if r.Proxied {
		for _, a := range got {
			ip, err := netip.ParseAddr(a)
			if err != nil || !slices.ContainsFunc(v.Cloudflare, func(p netip.Prefix) bool { return p.Contains(ip) }) {
				return nil, fmt.Errorf("answers %s, outside Cloudflare's ranges: it is not proxied", a)
			}
		}
		f.Detail = "proxied to " + strings.Join(r.Values, ", ")
		return got, nil
	}
	want := normalize(r.Values, r.Type)
	if !slices.Equal(got, want) {
		return nil, fmt.Errorf("answers %s; want %s", strings.Join(got, ", "), strings.Join(want, ", "))
	}
	f.Detail = short(strings.Join(got, ", "))
	if typ == dnsmessage.TypeA || typ == dnsmessage.TypeAAAA {
		return got, nil
	}
	return nil, nil
}

// short keeps a finding to a line: long values, such as DKIM keys, are cut.
func short(s string) string {
	if len(s) > 100 {
		return s[:97] + "..."
	}
	return s
}

// values gives the answers of the asked type in a comparable form, sorted.
func values(rs []dnsmessage.Resource, typ dnsmessage.Type) []string {
	var out []string
	for _, r := range rs {
		if r.Header.Type != typ {
			continue
		}
		switch b := r.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(b.A).String())
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(b.AAAA).String())
		case *dnsmessage.CNAMEResource:
			out = append(out, strings.ToLower(b.CNAME.String()))
		case *dnsmessage.MXResource:
			out = append(out, fmt.Sprintf("%d %s", b.Pref, strings.ToLower(b.MX.String())))
		case *dnsmessage.TXTResource:
			out = append(out, strings.Join(b.TXT, ""))
		}
	}
	slices.Sort(out)
	return out
}

// normalize puts the document's values in the form values gives.
func normalize(vs []string, typ string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		switch typ {
		case "A", "AAAA":
			if a, err := netip.ParseAddr(v); err == nil {
				v = a.String()
			}
		case "CNAME":
			v = fqdnDot(v)
		case "MX":
			pref, host, _ := strings.Cut(v, " ")
			v = pref + " " + fqdnDot(host)
		}
		out[i] = v
	}
	slices.Sort(out)
	return out
}

func fqdnDot(h string) string {
	h = strings.ToLower(h)
	if !strings.HasSuffix(h, ".") {
		h += "."
	}
	return h
}

func (v *Verifier) http(ctx context.Context, host string, addrs []string, c *HTTPCheck) (string, error) {
	client := v.HTTP
	if client == nil {
		client = &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     dialing(host, addrs),
		}
	}
	url := "https://" + host + c.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	if err := resp.Body.Close(); err != nil {
		return "", fmt.Errorf("GET %s: closing the body: %w", url, err)
	}
	if resp.StatusCode != c.Status {
		return "", fmt.Errorf("GET %s: %d, want %d", url, resp.StatusCode, c.Status)
	}
	if c.Location != "" && !strings.HasPrefix(resp.Header.Get("Location"), c.Location) {
		return "", fmt.Errorf("GET %s: redirects to %q, want %q", url, resp.Header.Get("Location"), c.Location)
	}
	return fmt.Sprintf("GET %s: %d", url, resp.StatusCode), nil
}

// udpQuery asks the first name server of the zone that answers.
func udpQuery(ctx context.Context, zone, name string, typ dnsmessage.Type) ([]dnsmessage.Resource, error) {
	nss, err := net.DefaultResolver.LookupNS(ctx, zone)
	if err != nil {
		return nil, fmt.Errorf("the zone's name servers: %w", err)
	}
	var errs []error
	for _, ns := range nss {
		rs, err := ask(ctx, strings.TrimSuffix(ns.Host, "."), name, typ)
		if err == nil {
			return rs, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

func ask(ctx context.Context, server, name string, typ dnsmessage.Type) ([]dnsmessage.Resource, error) {
	qname, err := dnsmessage.NewName(fqdnDot(name))
	if err != nil {
		return nil, err
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: uint16(time.Now().UnixNano())}, //nolint:gosec // G115: a query ID, any value will do
		Questions: []dnsmessage.Question{{Name: qname, Type: typ, Class: dnsmessage.ClassINET}},
	}
	packed, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(server, "53"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", server, err)
	}
	defer conn.Close() //nolint:errcheck // a UDP socket's close reports nothing actionable once the answer is read
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return nil, err
		}
	}
	if _, err := conn.Write(packed); err != nil {
		return nil, fmt.Errorf("%s: %w", server, err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", server, err)
	}
	var resp dnsmessage.Message
	if err := resp.Unpack(buf[:n]); err != nil {
		return nil, fmt.Errorf("%s: %w", server, err)
	}
	if resp.ID != msg.ID {
		return nil, fmt.Errorf("%s: answered another query", server)
	}
	switch resp.RCode {
	case dnsmessage.RCodeSuccess:
		return resp.Answers, nil
	case dnsmessage.RCodeNameError:
		return nil, nil
	default:
		return nil, fmt.Errorf("%s: %s", server, resp.RCode)
	}
}

// dialing is a transport that connects to host at the addresses the zone's
// name servers answered, in turn, not where a resolver's cache says, so a
// new or changed record is checked as soon as the zone has it. With no
// addresses, as for a CNAME, it resolves as usual.
func dialing(host string, addrs []string) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if len(addrs) == 0 {
		return t
	}
	d := net.Dialer{Timeout: 5 * time.Second} // each address, so one that hangs leaves time for the next
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		h, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if h != host {
			return d.DialContext(ctx, network, address)
		}
		var errs []error
		for _, a := range addrs {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(a, port))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}
	return t
}

// Get makes check's HTTPS request of host, a name in zone, at the
// addresses the zone's name servers answer for it, as a record's check
// does, so no resolver's cache hides a record added moments ago.
func (v *Verifier) Get(ctx context.Context, zone, host string, check *HTTPCheck) (string, error) {
	query := v.Query
	if query == nil {
		query = udpQuery
	}
	answers, err := query(ctx, zone, host, dnsmessage.TypeA)
	if err != nil {
		return "", fmt.Errorf("%s: %w", host, err)
	}
	addrs := values(answers, dnsmessage.TypeA)
	if len(addrs) == 0 {
		return "", fmt.Errorf("%s: %w", host, errNoRecord)
	}
	return v.http(ctx, host, addrs, check)
}

// ZoneOf returns the zone in d that host belongs to, the longest whose name
// is host or a suffix of it.
func (d *Document) ZoneOf(host string) (string, bool) {
	best := ""
	for _, z := range d.Zones {
		if (host == z.Name || strings.HasSuffix(host, "."+z.Name)) && len(z.Name) > len(best) {
			best = z.Name
		}
	}
	return best, best != ""
}
