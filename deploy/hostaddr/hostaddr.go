// Package hostaddr resolves a fleet host's address: the one the document
// gives, or one read from the tool that made the host, such as a Pulumi
// stack output.
package hostaddr

import (
	"bytes"
	"context"
	"errors"
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
	addr, err := Address(ctx, *h.AddressFrom, run)
	if err != nil {
		return fleet.Host{}, fmt.Errorf("hostaddr: %s's address: %w", h.Name, err)
	}
	h.Address, h.AddressFrom = addr, nil
	return h, nil
}

// Address reads an address from its source. It must be an IP address or
// a host name.
func Address(ctx context.Context, src fleet.AddressSource, run Runner) (string, error) {
	if src.Pulumi == nil {
		return "", fmt.Errorf("hostaddr: the source names no Pulumi output")
	}
	addr, err := Output(ctx, *src.Pulumi, run)
	if err != nil {
		return "", err
	}
	if net.ParseIP(addr) == nil && !hostname.MatchString(addr) {
		return "", fmt.Errorf("hostaddr: Pulumi output %s is %q, neither an IP address nor a host name", src.Pulumi.Output, addr)
	}
	return addr, nil
}

// Output reads a Pulumi stack output.
func Output(ctx context.Context, p fleet.PulumiOutput, run Runner) (string, error) {
	if run == nil {
		run = Exec
	}
	out, err := run(ctx, p.Dir, "pulumi", "stack", "output", p.Output, "--stack", p.Stack, "--non-interactive")
	if err != nil {
		return "", fmt.Errorf("hostaddr: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Config reads a Pulumi stack's configuration value, decrypted when it is a
// secret. The value is a secret: the caller must not print it.
func Config(ctx context.Context, p fleet.PulumiConfig, run Runner) (string, error) {
	if run == nil {
		run = Exec
	}
	out, err := run(ctx, p.Dir, "pulumi", "config", "get", p.Key, "--stack", p.Stack, "--non-interactive")
	if err != nil {
		// The command's output could hold the value; only its exit is reported.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("hostaddr: pulumi config get %s: %s", p.Key, ee.String())
		}
		return "", fmt.Errorf("hostaddr: pulumi config get %s: %w", p.Key, err)
	}
	v := strings.TrimRight(string(out), "\r\n")
	if v == "" {
		return "", fmt.Errorf("hostaddr: pulumi config %s is empty", p.Key)
	}
	return v, nil
}
