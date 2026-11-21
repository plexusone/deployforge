// Package hostaddr resolves a fleet host's address: the one the document
// gives, or one read from the tool that made the host, such as a Pulumi
// stack output.
package hostaddr

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"

	"github.com/plexusone/deployforge/fleet"
)

// Runner runs a command in a directory and returns its standard output;
// tests replace it.
type Runner func(ctx context.Context, dir string, argv ...string) ([]byte, error)

// Exec runs the command with os/exec.
func Exec(ctx context.Context, dir string, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: pulumi with names the fleet document's validation checked
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// hostname is a DNS name an address may be instead of an IP.
var hostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// Resolve returns h with its address: as given, or read from its source.
// A read address must be an IP address or a host name.
func Resolve(ctx context.Context, h fleet.Host, run Runner) (fleet.Host, error) {
	if h.AddressFrom == nil {
		return h, nil
	}
	p := h.AddressFrom.Pulumi
	if p == nil {
		return fleet.Host{}, fmt.Errorf("hostaddr: %s: addressFrom names no source", h.Name)
	}
	if run == nil {
		run = Exec
	}
	out, err := run(ctx, p.Dir, "pulumi", "stack", "output", p.Output, "--stack", p.Stack, "--non-interactive")
	if err != nil {
		return fleet.Host{}, fmt.Errorf("hostaddr: %s's address: %w", h.Name, err)
	}
	addr := strings.TrimSpace(string(out))
	if net.ParseIP(addr) == nil && !hostname.MatchString(addr) {
		return fleet.Host{}, fmt.Errorf("hostaddr: %s's address from Pulumi output %s is %q, neither an IP address nor a host name", h.Name, p.Output, addr)
	}
	h.Address, h.AddressFrom = addr, nil
	return h, nil
}
