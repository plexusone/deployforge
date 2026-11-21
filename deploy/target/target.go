// Package target resolves what a deploy needs to know about one stack of
// one app from the fleet document: the host it runs on, its unit, Unix
// user, loopback address, the host names its readiness probe sends, and its
// secret prefix.
package target

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/plexusone/deployforge/fleet"
)

// Target is one app stack, as a deploy sees it.
type Target struct {
	// App is the app's name, which is also its binary's.
	App string `json:"app"`
	// Stack is the stack's name.
	Stack string `json:"stack"`
	// Host is the fleet's name for the host the stack runs on.
	Host string `json:"host"`
	// Unit is the systemd unit, and the name of /opt/<unit> and /etc/<unit>.
	Unit string `json:"unit"`
	// User is the Unix user and group the stack runs as.
	User string `json:"user"`
	// Listen is the stack's loopback address, such as 127.0.0.1:8102.
	Listen string `json:"listen"`
	// HealthHosts are the Host headers the readiness probe sends, one probe
	// each: the stack's host names.
	HealthHosts []string `json:"healthHosts"`
	// SecretPrefix is the stack's secret prefix, ending in a slash.
	SecretPrefix string `json:"secretPrefix"`
}

var (
	safeName   = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	safeHost   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	safePrefix = regexp.MustCompile(`^/[A-Za-z0-9._/-]+/$`)
	safeUser   = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	loopback   = regexp.MustCompile(`^127\.0\.0\.1:([0-9]{4,5})$`)
)

// Resolve finds an app's stack in the fleet.
func Resolve(f *fleet.Fleet, app, stack string) (Target, error) {
	a, s, err := f.Stack(app, stack)
	if err != nil {
		return Target{}, fmt.Errorf("target: %w", err)
	}
	if len(s.Hosts) == 0 {
		return Target{}, fmt.Errorf("target: %s/%s has no host names", app, stack)
	}
	t := Target{
		App:          a.Name,
		Stack:        stack,
		Host:         s.Host,
		Unit:         f.Unit(a, stack),
		User:         f.UnixUser(a, stack),
		Listen:       fmt.Sprintf("127.0.0.1:%d", s.Port),
		HealthHosts:  append([]string(nil), s.Hosts...),
		SecretPrefix: a.SecretPrefixFor(stack),
	}
	return t, t.Validate()
}

// Validate checks that every value is a plain name, so none can reach a
// path, a command or a header as anything else. The fleet document's own
// validation already guarantees it; a target also arrives on the host in a
// request, so it is checked again there.
func (t Target) Validate() error {
	for _, v := range []struct{ name, value string }{{"app", t.App}, {"stack", t.Stack}, {"host", t.Host}, {"unit", t.Unit}} {
		if !safeName.MatchString(v.value) {
			return fmt.Errorf("target: %s %q is not a plain name", v.name, v.value)
		}
	}
	if !safeUser.MatchString(t.User) {
		return fmt.Errorf("target: user %q is not a plain name", t.User)
	}
	m := loopback.FindStringSubmatch(t.Listen)
	if m == nil {
		return fmt.Errorf("target: listen %q is not a loopback address", t.Listen)
	}
	if port, err := strconv.Atoi(m[1]); err != nil || port < 1024 || port > 65535 {
		return fmt.Errorf("target: listen %q is not a loopback port from 1024 to 65535", t.Listen)
	}
	if len(t.HealthHosts) == 0 {
		return fmt.Errorf("target: no health hosts")
	}
	for _, h := range t.HealthHosts {
		if !safeHost.MatchString(h) {
			return fmt.Errorf("target: health host %q is not a host name", h)
		}
	}
	if !safePrefix.MatchString(t.SecretPrefix) {
		return fmt.Errorf("target: secret prefix %q must be /<path>/", t.SecretPrefix)
	}
	return nil
}
