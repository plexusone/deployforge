// Package hostports checks a Lightsail instance's firewall against what a
// fleet host behind a CDN needs: HTTPS open exactly to the CDN's ranges,
// SSH open only to the operators' addresses, and nothing else open.
//
// Lightsail replaces an instance's firewall as a whole, and a tool that
// replaces its rules new-first then deletes the old ones can close the
// ports it just opened, leaving every site unreachable while the instance
// itself is healthy. Run the check after any change to the firewall.
package hostports

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lightsail"
	"github.com/aws/aws-sdk-go-v2/service/lightsail/types"
)

// API is the part of Lightsail the check reads.
type API interface {
	GetInstancePortStates(ctx context.Context, in *lightsail.GetInstancePortStatesInput, opts ...func(*lightsail.Options)) (*lightsail.GetInstancePortStatesOutput, error)
}

// Ports the check expects.
const (
	HTTPS = 443
	SSH   = 22
)

// Want is what the firewall should admit.
type Want struct {
	// Edge are the ranges HTTPS is open to: the CDN's, IPv4 and IPv6.
	Edge []netip.Prefix
	// Admin are the addresses SSH is open to.
	Admin []netip.Prefix
}

// Result is what the check found.
type Result struct {
	// OK are lines saying what matched.
	OK []string
	// Problems are lines saying what did not; none means the firewall is
	// as wanted.
	Problems []string
}

// Check reads the instance's port states and compares them with want.
func Check(ctx context.Context, api API, instance string, want Want) (Result, error) {
	out, err := api.GetInstancePortStates(ctx, &lightsail.GetInstancePortStatesInput{InstanceName: aws.String(instance)})
	if err != nil {
		return Result{}, fmt.Errorf("hostports: reading %s's ports: %w", instance, err)
	}
	return Compare(out.PortStates, want), nil
}

// Compare compares port states with want.
func Compare(states []types.InstancePortState, want Want) Result {
	var r Result
	seen := map[int32]bool{}
	for _, ps := range states {
		if ps.State != types.PortStateOpen {
			continue
		}
		name := portName(ps)
		switch {
		case ps.Protocol == types.NetworkProtocolTcp && ps.FromPort == HTTPS && ps.ToPort == HTTPS:
			seen[HTTPS] = true
			r.compare(name, ps, want.Edge)
		case ps.Protocol == types.NetworkProtocolTcp && ps.FromPort == SSH && ps.ToPort == SSH:
			seen[SSH] = true
			r.compare(name, ps, want.Admin)
		default:
			r.Problems = append(r.Problems, fmt.Sprintf("%s is open to %s; only %d and %d should be", name, sources(ps), HTTPS, SSH))
		}
	}
	for _, p := range []int32{HTTPS, SSH} {
		if !seen[p] {
			r.Problems = append(r.Problems, fmt.Sprintf("tcp %d is closed", p))
		}
	}
	return r
}

// compare checks one port's sources against the wanted ranges.
func (r *Result) compare(name string, ps types.InstancePortState, want []netip.Prefix) {
	got := map[string]bool{}
	for _, c := range append(slices.Clone(ps.Cidrs), ps.Ipv6Cidrs...) {
		got[canonical(c)] = true
	}
	wanted := map[string]bool{}
	for _, p := range want {
		wanted[p.Masked().String()] = true
	}
	var missing, extra []string
	for c := range wanted {
		if !got[c] {
			missing = append(missing, c)
		}
	}
	for c := range got {
		if !wanted[c] {
			extra = append(extra, c)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	if len(ps.CidrListAliases) > 0 {
		extra = append(extra, ps.CidrListAliases...)
	}
	if len(missing) == 0 && len(extra) == 0 {
		r.OK = append(r.OK, fmt.Sprintf("%s is open to exactly %d range(s)", name, len(wanted)))
		return
	}
	if len(missing) > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("%s is not open to %s", name, strings.Join(missing, ", ")))
	}
	if len(extra) > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("%s is also open to %s", name, strings.Join(extra, ", ")))
	}
}

func portName(ps types.InstancePortState) string {
	if ps.FromPort == ps.ToPort {
		return fmt.Sprintf("%s %d", ps.Protocol, ps.FromPort)
	}
	return fmt.Sprintf("%s %d-%d", ps.Protocol, ps.FromPort, ps.ToPort)
}

func sources(ps types.InstancePortState) string {
	all := append(append(slices.Clone(ps.Cidrs), ps.Ipv6Cidrs...), ps.CidrListAliases...)
	if len(all) == 0 {
		return "nothing"
	}
	return strings.Join(all, ", ")
}

// canonical is a CIDR in its masked form, or the text as given when it
// does not parse, so it is reported rather than dropped.
func canonical(c string) string {
	p, err := netip.ParsePrefix(c)
	if err != nil {
		return c
	}
	return p.Masked().String()
}
