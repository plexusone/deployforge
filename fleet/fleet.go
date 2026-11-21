// Package fleet defines the fleet document: the hosts, the apps, and each
// app's stacks, with their host names, ports, databases and limits. Loading
// checks the document against the rules that keep apps apart on shared
// hosts. The Go types are the source of truth for the document's JSON
// Schema.
package fleet

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

// FormatVersion is the version of the fleet document format this package
// reads. Adding an optional field keeps the version; any other change
// raises it, and loading refuses versions it does not know.
const FormatVersion = "1"

// StackPlaceholder stands for the stack's name in an app's secret prefix.
const StackPlaceholder = "{stack}"

// DefaultPrimaryStack is the stack whose unit, user and paths are named
// for the app alone, when the document names none.
const DefaultPrimaryStack = "prod"

// Fleet is the fleet document.
type Fleet struct {
	// Schema is the JSON Schema the file follows, for editors.
	Schema string `json:"$schema,omitempty" jsonschema:"description=The JSON Schema this file follows\\, for editors"`
	// FormatVersion is the version of the format the file follows.
	FormatVersion string `json:"formatVersion" jsonschema:"description=Version of the fleet document format the file follows,enum=1"`
	// PrimaryStack is the stack named for the app alone: its unit is the
	// app's name, and every other stack's is <app>-<stack>.
	PrimaryStack string `json:"primaryStack,omitempty" jsonschema:"description=The stack whose unit\\, user and paths are named for the app alone; prod when empty"`
	// PortRange is where apps' loopback ports come from.
	PortRange PortRange `json:"portRange" jsonschema:"description=The range apps' loopback ports come from"`
	// Hosts are the machines apps run on.
	Hosts []Host `json:"hosts" jsonschema:"description=The machines apps run on,minItems=1"`
	// Apps are the deployed apps.
	Apps []App `json:"apps" jsonschema:"description=The deployed apps"`
}

// PortRange is an inclusive range of loopback ports.
type PortRange struct {
	Min int `json:"min" jsonschema:"description=The lowest port,minimum=1024,maximum=65535"`
	Max int `json:"max" jsonschema:"description=The highest port,minimum=1024,maximum=65535"`
}

// Host is a machine apps run on.
type Host struct {
	// Name is how stacks refer to the host.
	Name string `json:"name" jsonschema:"description=How stacks refer to the host,pattern=^[a-z][a-z0-9-]*[a-z0-9]$"`
	// Address is the host's address for SSH, or a reference resolved at
	// deploy time, such as an output of the tool that made the host.
	Address string `json:"address" jsonschema:"description=The host's address for SSH\\, or a reference resolved at deploy time"`
	// SSHUser is the user deployforge connects as; it needs sudo.
	SSHUser string `json:"sshUser,omitempty" jsonschema:"description=The user to connect as over SSH\\, with sudo; ubuntu when empty"`
}

// DefaultSSHUser is the user deployforge connects as when a host names none.
const DefaultSSHUser = "ubuntu"

// User is the SSH user for the host.
func (h Host) User() string { return cmp.Or(h.SSHUser, DefaultSSHUser) }

// App is one deployed app: one binary built by its repository, run as one
// or more stacks of the same release.
type App struct {
	// Name is the app's name: its binary, and with the stack, its unit,
	// user and paths.
	Name string `json:"name" jsonschema:"description=The app's name: its binary\\, and with the stack\\, its unit\\, user and paths,pattern=^[a-z][a-z0-9-]*[a-z0-9]$"`
	// Repo is the repository that builds the binary.
	Repo string `json:"repo" jsonschema:"description=The repository that builds the binary\\, such as github.com/org/repo"`
	// Domain, when set, is the DNS zone every host name of the app is in.
	Domain string `json:"domain,omitempty" jsonschema:"description=The DNS zone every host name of the app is in"`
	// SecretPrefix is where the app's secrets are in the parameter store,
	// with {stack} for the stack's name, such as /example/{stack}/app/.
	SecretPrefix string `json:"secretPrefix" jsonschema:"description=The app's secrets in the parameter store\\, with {stack} for the stack\\, such as /example/{stack}/app/"`
	// StateDirectory is whether the app keeps state on local disk, so its
	// unit gets a private writable state directory.
	StateDirectory bool `json:"stateDirectory,omitempty" jsonschema:"description=Whether the app keeps state on local disk and gets a private state directory"`
	// Selfcheck is whether a deploy runs the app's selfcheck subcommand
	// before and after the switch.
	Selfcheck bool `json:"selfcheck,omitempty" jsonschema:"description=Whether a deploy runs the app's selfcheck subcommand before and after the switch"`
	// LogFiles are the files the app writes under /var/log/<unit>/, which
	// the host rotates daily by rename, keeping a week.
	LogFiles []string `json:"logFiles,omitempty" jsonschema:"description=Files the app writes under /var/log/<unit>/\\, rotated daily by rename and kept a week"`
	// Stacks are the app's deployments, by name.
	Stacks map[string]Stack `json:"stacks" jsonschema:"description=The app's deployments by stack name\\, such as prod and staging,minProperties=1"`
	// Notes explain choices about the app; JSON has no comments.
	Notes string `json:"notes,omitempty" jsonschema:"description=Why the app is set up this way; JSON has no comments"`
}

// Stack is one deployment of an app.
type Stack struct {
	// Host names the host the stack runs on.
	Host string `json:"host" jsonschema:"description=The host the stack runs on\\, by name"`
	// Hosts are the host names the reverse proxy forwards to the stack.
	Hosts []string `json:"hosts" jsonschema:"description=Host names the reverse proxy forwards to the stack,minItems=1"`
	// LoopbackHosts are host names among Hosts that the host resolves to
	// itself, so the app's calls to them, such as to its own auth server,
	// stay on the host instead of going out through a CDN.
	LoopbackHosts []string `json:"loopbackHosts,omitempty" jsonschema:"description=Host names among hosts that the host resolves to itself\\, so the app's calls to them stay on the host"`
	// Port is the stack's loopback port.
	Port int `json:"port" jsonschema:"description=The loopback port the binary listens on"`
	// Databases are the stack's databases on the database server.
	Databases []Database `json:"databases,omitempty" jsonschema:"description=The stack's databases on the database server; each name unique across the fleet"`
	// Status is where the stack runs.
	Status Status `json:"status" jsonschema:"description=Where the stack runs,enum=planned,enum=live,enum=elsewhere"`
	// Limits are the stack's resource limits, required once it is live.
	Limits *Limits `json:"limits,omitempty" jsonschema:"description=Resource limits; required once the stack is live"`
}

// Database is one of a stack's databases. Its owner role migrates it; when
// AppRole is set, the app runs as a separate role with rows only, and
// otherwise it runs as the owner.
type Database struct {
	Name    string `json:"name" jsonschema:"description=The database's name,pattern=^[a-z][a-z0-9_]*$"`
	AppRole bool   `json:"appRole,omitempty" jsonschema:"description=Whether the app runs as a separate role with rows only\\, rather than as the owner"`
}

// Status is where a stack runs.
type Status string

// Statuses.
const (
	// Planned stacks are not deployed yet.
	Planned Status = "planned"
	// Live stacks run on their host and are deployed by deployforge.
	Live Status = "live"
	// Elsewhere stacks run outside the fleet, until they move in.
	Elsewhere Status = "elsewhere"
)

// Limits keep one stack from starving the others on a host.
type Limits struct {
	// MemoryMax is systemd's MemoryMax, such as 512M or 1G.
	MemoryMax string `json:"memoryMax" jsonschema:"description=systemd MemoryMax\\, such as 512M or 1G,pattern=^[1-9][0-9]*[KMG]$"`
	// CPUQuota is systemd's CPUQuota, such as 50%.
	CPUQuota string `json:"cpuQuota" jsonschema:"description=systemd CPUQuota\\, such as 50%,pattern=^[1-9][0-9]*%$"`
	// ConnectionLimit is each database role's CONNECTION LIMIT, and the
	// size the app's pools should match.
	ConnectionLimit int `json:"connectionLimit" jsonschema:"description=Each database role's CONNECTION LIMIT\\, which the app's pools should match,minimum=1"`
}

// Primary is the stack named for the app alone.
func (f *Fleet) Primary() string { return cmp.Or(f.PrimaryStack, DefaultPrimaryStack) }

// Host returns the host with the name.
func (f *Fleet) Host(name string) (Host, bool) {
	i := slices.IndexFunc(f.Hosts, func(h Host) bool { return h.Name == name })
	if i < 0 {
		return Host{}, false
	}
	return f.Hosts[i], true
}

// App returns the app with the name.
func (f *Fleet) App(name string) (App, bool) {
	i := slices.IndexFunc(f.Apps, func(a App) bool { return a.Name == name })
	if i < 0 {
		return App{}, false
	}
	return f.Apps[i], true
}

// Stack returns an app and one of its stacks, or an error naming what the
// document lacks.
func (f *Fleet) Stack(app, stack string) (App, Stack, error) {
	a, ok := f.App(app)
	if !ok {
		return App{}, Stack{}, fmt.Errorf("fleet: no app %q", app)
	}
	s, ok := a.Stacks[stack]
	if !ok {
		return App{}, Stack{}, fmt.Errorf("fleet: app %s has no stack %q", app, stack)
	}
	return a, s, nil
}

// StackNames are the app's stacks, the primary first, then by name.
func (f *Fleet) StackNames(a App) []string {
	names := make([]string, 0, len(a.Stacks))
	for n := range a.Stacks {
		names = append(names, n)
	}
	primary := f.Primary()
	slices.SortFunc(names, func(x, y string) int {
		switch {
		case x == primary:
			return -1
		case y == primary:
			return 1
		}
		return strings.Compare(x, y)
	})
	return names
}

// Unit is the systemd unit and directory name of an app's stack: the
// app's name for the primary stack, and <app>-<stack> for any other.
func (f *Fleet) Unit(a App, stack string) string {
	if stack == f.Primary() {
		return a.Name
	}
	return a.Name + "-" + stack
}

// UnixUser is the user and group an app's stack runs as: its unit name
// without hyphens.
func (f *Fleet) UnixUser(a App, stack string) string {
	return strings.ReplaceAll(f.Unit(a, stack), "-", "")
}

// SecretPrefixFor is the app's secret prefix for the stack.
func (a App) SecretPrefixFor(stack string) string {
	return strings.ReplaceAll(a.SecretPrefix, StackPlaceholder, stack)
}

// Load reads and checks a fleet document. Unknown fields are an error.
func Load(path string) (*Fleet, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the fleet document to read
	if err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	return Parse(data)
}

// Parse decodes and checks a fleet document. Unknown fields are an error.
func Parse(data []byte) (*Fleet, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f Fleet
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

var (
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	repoPattern   = regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]+/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	hostPattern   = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$`)
	dbPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	logPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	memoryPattern = regexp.MustCompile(`^[1-9][0-9]*[KMG]$`)
	cpuPattern    = regexp.MustCompile(`^[1-9][0-9]*%$`)
)

// maxUnixUser is the longest user name common Linux tools accept.
const maxUnixUser = 32

// Validate reports every way the document breaks the format or the rules
// that keep apps apart: an unknown format version; a bad port range;
// names, units, host names and databases used twice; a stack on an
// unknown host, or with a port outside the range or used on its host; a
// host name outside the app's domain; a secret prefix without {stack};
// and a live stack without limits.
func (f *Fleet) Validate() error {
	if f.FormatVersion != FormatVersion {
		return fmt.Errorf("fleet: formatVersion %q is not supported; this version reads %q", f.FormatVersion, FormatVersion)
	}
	var errs []error
	bad := func(at, format string, a ...any) {
		errs = append(errs, fmt.Errorf("fleet: %s: %s", at, fmt.Sprintf(format, a...)))
	}
	if f.PrimaryStack != "" && !namePattern.MatchString(f.PrimaryStack) {
		bad("primaryStack", "must be lowercase letters, digits and hyphens")
	}
	r := f.PortRange
	if r.Min < 1024 || r.Max > 65535 || r.Min > r.Max {
		bad("portRange", "must be within 1024 to 65535, min no more than max, got %d to %d", r.Min, r.Max)
	}
	hostNames := map[string]bool{}
	for i, h := range f.Hosts {
		at := cmp.Or(h.Name, fmt.Sprintf("hosts[%d]", i))
		switch {
		case !namePattern.MatchString(h.Name):
			bad(at, "name must be lowercase letters, digits and hyphens")
		case hostNames[h.Name]:
			bad(at, "name is used by another host")
		}
		hostNames[h.Name] = true
		if strings.TrimSpace(h.Address) == "" {
			bad(at, "needs an address")
		}
	}
	if len(f.Hosts) == 0 {
		bad("hosts", "needs at least one host")
	}

	apps, units := map[string]bool{}, map[string]string{}
	ports, names, dbs := map[string]string{}, map[string]string{}, map[string]string{}
	for i, a := range f.Apps {
		at := cmp.Or(a.Name, fmt.Sprintf("apps[%d]", i))
		switch {
		case !namePattern.MatchString(a.Name):
			bad(at, "name must be lowercase letters, digits and hyphens")
		case apps[a.Name]:
			bad(at, "name is used by another app")
		}
		apps[a.Name] = true
		if !repoPattern.MatchString(a.Repo) {
			bad(at, "repo must be <host>/<owner>/<repo>, such as github.com/example/app")
		}
		if a.Domain != "" && !hostPattern.MatchString(a.Domain) {
			bad(at, "domain %q must be a lowercase DNS name", a.Domain)
		}
		if !strings.HasPrefix(a.SecretPrefix, "/") || !strings.HasSuffix(a.SecretPrefix, "/") || !strings.Contains(a.SecretPrefix, StackPlaceholder) {
			bad(at, "secretPrefix must start and end with / and contain %s, such as /example/%s/%s/", StackPlaceholder, StackPlaceholder, a.Name)
		}
		if len(a.Stacks) == 0 {
			bad(at, "needs at least one stack")
		}
		for _, l := range a.LogFiles {
			if !logPattern.MatchString(l) {
				bad(at, "log file %q must be a plain file name", l)
			}
		}
		for _, stack := range f.StackNames(a) {
			s := a.Stacks[stack]
			at := at + "/" + stack
			if !namePattern.MatchString(stack) {
				bad(at, "stack name must be lowercase letters, digits and hyphens")
			}
			unit := f.Unit(a, stack)
			if other, ok := units[unit]; ok {
				bad(at, "unit %s is used by %s", unit, other)
			}
			units[unit] = at
			if u := f.UnixUser(a, stack); len(u) > maxUnixUser {
				bad(at, "the Unix user %q is longer than %d characters", u, maxUnixUser)
			}
			if !hostNames[s.Host] {
				bad(at, "host %q is not in hosts", s.Host)
			}
			key := fmt.Sprintf("%s:%d", s.Host, s.Port)
			switch {
			case s.Port < r.Min || s.Port > r.Max:
				bad(at, "port %d is outside %d to %d", s.Port, r.Min, r.Max)
			case ports[key] != "":
				bad(at, "port %d on %s is used by %s", s.Port, s.Host, ports[key])
			}
			ports[key] = at
			if len(s.Hosts) == 0 {
				bad(at, "needs at least one host name")
			}
			for _, h := range s.Hosts {
				switch {
				case !hostPattern.MatchString(h):
					bad(at, "host name %q is not a lowercase DNS name", h)
				case a.Domain != "" && h != a.Domain && !strings.HasSuffix(h, "."+a.Domain):
					bad(at, "host name %s is not in the app's domain %s", h, a.Domain)
				case names[h] != "":
					bad(at, "host name %s is used by %s", h, names[h])
				}
				names[h] = at
			}
			for _, h := range s.LoopbackHosts {
				if !slices.Contains(s.Hosts, h) {
					bad(at, "loopback host %s is not one of the stack's host names", h)
				}
			}
			for _, db := range s.Databases {
				switch {
				case !dbPattern.MatchString(db.Name):
					bad(at, "database %q must be lowercase letters, digits and underscores", db.Name)
				case dbs[db.Name] != "":
					bad(at, "database %s is used by %s", db.Name, dbs[db.Name])
				}
				dbs[db.Name] = at
			}
			if !slices.Contains([]Status{Planned, Live, Elsewhere}, s.Status) {
				bad(at, "status %q must be planned, live or elsewhere", s.Status)
			}
			if s.Status == Live && s.Limits == nil {
				bad(at, "a live stack needs limits")
			}
			if l := s.Limits; l != nil {
				if !memoryPattern.MatchString(l.MemoryMax) {
					bad(at, "limits.memoryMax %q must be like 512M or 1G", l.MemoryMax)
				}
				if !cpuPattern.MatchString(l.CPUQuota) {
					bad(at, "limits.cpuQuota %q must be like 50%%", l.CPUQuota)
				}
				if l.ConnectionLimit < 1 {
					bad(at, "limits.connectionLimit must be at least 1")
				}
			}
		}
	}
	return errors.Join(errs...)
}

// NextPort is the lowest port in the range that no stack on the host
// uses, or an error when the range is full.
func (f *Fleet) NextPort(host string) (int, error) {
	used := map[int]bool{}
	for _, a := range f.Apps {
		for _, s := range a.Stacks {
			if s.Host == host {
				used[s.Port] = true
			}
		}
	}
	for p := f.PortRange.Min; p <= f.PortRange.Max; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("fleet: every port from %d to %d on %s is used", f.PortRange.Min, f.PortRange.Max, host)
}
