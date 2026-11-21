package onhost

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"time"
)

// Systemd is the System of a real host: systemctl, systemd-run, journalctl
// and an HTTP probe on loopback.
type Systemd struct {
	// Client probes /readyz; a client with a five-second timeout when nil.
	Client *http.Client
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: only systemctl, with a unit name from a validated request
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}

// DaemonReload implements System.
func (Systemd) DaemonReload(ctx context.Context) error { return run(ctx, "systemctl", "daemon-reload") }

// Restart implements System.
func (Systemd) Restart(ctx context.Context, unit string) error {
	return run(ctx, "systemctl", "restart", unit)
}

// Stop implements System.
func (Systemd) Stop(ctx context.Context, unit string) error {
	return run(ctx, "systemctl", "stop", unit)
}

// RunAs implements System with systemd-run, so the environment files are parsed
// exactly as the unit parses them.
func (Systemd) RunAs(ctx context.Context, usr string, envFiles, argv []string, stdout, stderr io.Writer) error {
	args := []string{"--quiet", "--wait", "--pipe", "--collect", "--uid=" + usr, "--gid=" + usr}
	for _, f := range envFiles {
		args = append(args, "-p", "EnvironmentFile="+f)
	}
	args = append(args, "--")
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "systemd-run", args...) //nolint:gosec // G204: the binary and paths come from a validated request
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// Journal implements System.
func (Systemd) Journal(ctx context.Context, unit string, lines int) string {
	out, err := exec.CommandContext(ctx, "journalctl", "-u", unit, "-n", strconv.Itoa(lines), "--no-pager").CombinedOutput() //nolint:gosec // G204: the unit comes from a validated request
	if err != nil {
		return fmt.Sprintf("(journalctl: %v)\n%s", err, out)
	}
	return string(out)
}

// Ready implements System.
func (s Systemd) Ready(ctx context.Context, listen, host string) bool {
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listen+"/readyz", nil)
	if err != nil {
		return false
	}
	req.Host = host
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection is reused; the status is what matters
	_ = resp.Body.Close()                 // read-only
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Own implements System: root and the stack's group.
func (Systemd) Own(path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("group %s: %w", group, err)
	}
	return os.Chown(path, 0, gid)
}
