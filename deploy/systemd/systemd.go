// Package systemd renders an app stack's systemd unit from the fleet: the
// sandbox every app runs in, with the stack's own user, paths and resource
// limits.
package systemd

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"

	"github.com/plexusone/deployforge/fleet"
)

//go:embed unit.service.tmpl
var source string

var tmpl = template.Must(template.New("unit").Parse(source))

type data struct {
	App, Stack, Unit, User string
	Limits                 fleet.Limits
	StateDirectory         bool
	Hardening              []string
}

// hardening is the sandbox every app's service runs in, one systemd setting
// each. The unit and the self-check both take it from here, so the check
// runs in exactly the service's sandbox.
var hardening = []string{
	"UMask=0077",
	"NoNewPrivileges=true",
	"ProtectSystem=strict",
	"ProtectHome=true",
	"PrivateTmp=true",
	"PrivateDevices=true",
	"ProtectClock=true",
	"ProtectHostname=true",
	"ProtectKernelTunables=true",
	"ProtectKernelModules=true",
	"ProtectKernelLogs=true",
	"ProtectControlGroups=true",
	"ProtectProc=invisible",
	"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
	"RestrictNamespaces=true",
	"RestrictRealtime=true",
	"RestrictSUIDSGID=true",
	"RemoveIPC=true",
	"LockPersonality=true",
	"MemoryDenyWriteExecute=true",
	"SystemCallArchitectures=native",
	"SystemCallFilter=@system-service",
	"SystemCallFilter=~@privileged @resources",
	"CapabilityBoundingSet=",
	"AmbientCapabilities=",
}

// Render writes the unit for one stack of one app. The stack needs limits,
// because a unit without them would let it starve the other apps.
func Render(f *fleet.Fleet, app, stack string) ([]byte, error) {
	d, err := stackData(f, app, stack)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, d); err != nil {
		return nil, fmt.Errorf("systemd: %w", err)
	}
	return b.Bytes(), nil
}

// Sandbox is the service's sandbox as systemd-run properties: its limits,
// its state and logs directories, the hardening and its read-only paths.
// The user and environment files are set by the caller, as for validate.
func Sandbox(f *fleet.Fleet, app, stack string) ([]string, error) {
	d, err := stackData(f, app, stack)
	if err != nil {
		return nil, err
	}
	props := []string{
		"MemoryMax=" + d.Limits.MemoryMax,
		"CPUQuota=" + d.Limits.CPUQuota,
		"LogsDirectory=" + d.Unit,
		"LogsDirectoryMode=0700",
	}
	if d.StateDirectory {
		props = append(props, "StateDirectory="+d.Unit, "StateDirectoryMode=0700")
	}
	props = append(props, d.Hardening...)
	return append(props, "ReadOnlyPaths=/opt/"+d.Unit+" /etc/"+d.Unit), nil
}

func stackData(f *fleet.Fleet, app, stack string) (data, error) {
	a, st, err := f.Stack(app, stack)
	if err != nil {
		return data{}, fmt.Errorf("systemd: %w", err)
	}
	if st.Limits == nil {
		return data{}, fmt.Errorf("systemd: %s/%s has no limits in the fleet document; set them before generating its unit", app, stack)
	}
	return data{
		App: a.Name, Stack: stack, Unit: f.Unit(a, stack), User: f.UnixUser(a, stack),
		Limits: *st.Limits, StateDirectory: a.StateDirectory, Hardening: hardening,
	}, nil
}
