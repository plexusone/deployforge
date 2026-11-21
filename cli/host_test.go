package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/plexusone/deployforge/deploy/deployer"
	"github.com/plexusone/deployforge/fleet"
)

// fakeTransport records what a command does on a host.
type fakeTransport struct {
	calls  []string
	script string
}

func (f *fakeTransport) TempDir(context.Context) (string, error) { return "/tmp/tmp.abc", nil }
func (f *fakeTransport) Upload(_ context.Context, local, remote string) error {
	data, err := os.ReadFile(local) //nolint:gosec // G304: the file the command under test wrote
	if err != nil {
		return err
	}
	f.script = string(data)
	f.calls = append(f.calls, "upload "+remote)
	return nil
}
func (f *fakeTransport) Sudo(_ context.Context, argv []string, _ io.Reader, stdout, _ io.Writer) error {
	f.calls = append(f.calls, "sudo "+strings.Join(argv, " "))
	if argv[0] == "bash" {
		_, err := io.WriteString(stdout, "example-app is ready for its first deploy\n")
		return err
	}
	return nil
}

func TestHostSetupPrintsOrApplies(t *testing.T) {
	out, err := run(t, "host", "setup", "example-app", "prod")
	if err != nil || !strings.HasPrefix(out, "#!/bin/bash") || !strings.Contains(out, "unit=example-app") {
		t.Fatalf("print: %v\n%s", err, out)
	}

	tr := &fakeTransport{}
	var dialled fleet.Host
	cmd := New(Options{Fleet: testFleet, Dial: func(string) deployer.Dialer {
		return func(h fleet.Host) (deployer.Transport, error) { dialled = h; return tr, nil }
	}})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"host", "setup", "example-app", "prod", "--apply"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("apply: %v\n%s", err, buf.String())
	}
	if dialled.Name != "web-1" || tr.script != out {
		t.Errorf("dialled %+v; uploaded the printed script: %v", dialled, tr.script == out)
	}
	want := "upload /tmp/tmp.abc/host-setup.sh|sudo bash /tmp/tmp.abc/host-setup.sh|sudo rm -rf /tmp/tmp.abc"
	if got := strings.Join(tr.calls, "|"); got != want {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(buf.String(), "ready for its first deploy") {
		t.Errorf("output = %q", buf.String())
	}
}
