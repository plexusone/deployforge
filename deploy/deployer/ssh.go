package deployer

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/plexusone/deployforge/fleet"
)

// SSH reaches a host with the system's ssh and scp, so the operator's SSH
// configuration, agent and known hosts apply. BatchMode means a missing key or
// an unknown host fails instead of prompting.
type SSH struct {
	// Host is the ssh target, such as ubuntu@203.0.113.7.
	Host string
	// Key, when set, is the private key ssh and scp use, through the agent
	// SSH_AUTH_SOCK names: an SSH configuration that sends every host to
	// another agent, such as a password manager's, cannot hide it.
	Key string
}

// options are the ssh and scp options every call uses.
func (s *SSH) options() []string {
	opts := []string{"-o", "BatchMode=yes"}
	if s.Key != "" {
		opts = append(opts, "-i", s.Key, "-o", "IdentityAgent=SSH_AUTH_SOCK")
	}
	return opts
}

// NewSSH checks the target is a plain user@host or host; key, when not
// empty, is the private key to use.
func NewSSH(host, key string) (*SSH, error) {
	if host == "" {
		return nil, fmt.Errorf("deployer: no ssh target; give the host an address")
	}
	if !safeWord.MatchString(host) || strings.HasPrefix(host, "-") {
		return nil, fmt.Errorf("deployer: invalid ssh target %q", host)
	}
	if strings.HasPrefix(key, "-") {
		return nil, fmt.Errorf("deployer: invalid key path %q", key)
	}
	return &SSH{Host: host, Key: key}, nil
}

// TempDir implements Transport.
func (s *SSH) TempDir(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "ssh", append(s.options(), s.Host, "mktemp", "-d")...).Output() //nolint:gosec // G204: the host and key are checked in NewSSH
	if err != nil {
		return "", fmt.Errorf("ssh %s mktemp -d: %w", s.Host, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Upload implements Transport.
func (s *SSH) Upload(ctx context.Context, local, remote string) error {
	if !safeWord.MatchString(remote) {
		return fmt.Errorf("unsafe remote path %q", remote)
	}
	cmd := exec.CommandContext(ctx, "scp", append(append([]string{"-q"}, s.options()...), "--", local, s.Host+":"+remote)...) //nolint:gosec // G204: the host, key and remote path are checked
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scp %s: %w: %s", local, err, out)
	}
	return nil
}

// Sudo implements Transport. Every word of argv must be a plain word: the
// remote shell sees them joined.
func (s *SSH) Sudo(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	for _, a := range argv {
		if !safeWord.MatchString(a) {
			return fmt.Errorf("unsafe remote argument %q", a)
		}
	}
	args := append(append(s.options(), s.Host, "sudo"), argv...)
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // G204: every word is checked
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// SSHDialer reaches each host as <sshUser>@<address> with the system's ssh;
// key, when not empty, is the private key to use.
func SSHDialer(key string) Dialer {
	return func(h fleet.Host) (Transport, error) {
		return NewSSH(h.User()+"@"+h.Address, key)
	}
}
