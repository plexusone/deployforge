package fleet

import (
	"slices"
	"strings"
	"testing"
)

func load(t *testing.T) *Fleet {
	t.Helper()
	f, err := Load("testdata/fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoadAndNames(t *testing.T) {
	f := load(t)
	a, ok := f.App("example-app")
	if !ok {
		t.Fatal("no example-app")
	}
	if got := f.StackNames(a); !slices.Equal(got, []string{"prod", "staging"}) {
		t.Errorf("StackNames = %v", got)
	}
	for stack, want := range map[string][2]string{
		"prod":    {"example-app", "exampleapp"},
		"staging": {"example-app-staging", "exampleappstaging"},
	} {
		if u := f.Unit(a, stack); u != want[0] {
			t.Errorf("Unit(%s) = %s, want %s", stack, u, want[0])
		}
		if u := f.UnixUser(a, stack); u != want[1] {
			t.Errorf("UnixUser(%s) = %s, want %s", stack, u, want[1])
		}
	}
	if p := a.SecretPrefixFor("staging"); p != "/example/staging/example-app/" {
		t.Errorf("SecretPrefixFor = %s", p)
	}
	h, ok := f.Host("web-1")
	if !ok || h.User() != DefaultSSHUser {
		t.Errorf("Host = %+v, %v", h, ok)
	}
	if p, err := f.NextPort("web-1"); err != nil || p != 8103 {
		t.Errorf("NextPort = %d, %v", p, err)
	}
}

func TestPrimaryStack(t *testing.T) {
	f := load(t)
	f.PrimaryStack = "staging"
	a, _ := f.App("example-app")
	if f.Unit(a, "staging") != "example-app" || f.Unit(a, "prod") != "example-app-prod" {
		t.Errorf("units with primary staging: %s, %s", f.Unit(a, "staging"), f.Unit(a, "prod"))
	}
	if got := f.StackNames(a); got[0] != "staging" {
		t.Errorf("StackNames = %v", got)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	f := load(t)
	a := f.Apps[0]
	prod := a.Stacks["prod"]
	prod.Limits = nil
	a.Stacks["prod"] = prod
	staging := a.Stacks["staging"]
	staging.Port = 8101
	staging.Host = "web-2"
	staging.Hosts = []string{"app.example.com", "other.test"}
	staging.Databases = []Database{{Name: "example"}, {Name: "Bad-Name"}}
	staging.Status = "running"
	a.Stacks["staging"] = staging
	a.SecretPrefix = "/example/app/"
	a.Repo = "example-app"
	f.Apps = append(f.Apps, a)
	f.PortRange = PortRange{Min: 9000, Max: 8000}
	err := f.Validate()
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"portRange: must be within",
		"example-app/prod: a live stack needs limits",
		`example-app/staging: host "web-2" is not in hosts`,
		"example-app/staging: host name app.example.com is used by example-app/prod",
		"example-app/staging: host name other.test is not in the app's domain example.com",
		"example-app/staging: database example is used by example-app/prod",
		`example-app/staging: database "Bad-Name" must be`,
		`example-app/staging: status "running" must be`,
		"example-app: name is used by another app",
		"example-app: secretPrefix must",
		"example-app: repo must be",
		"unit example-app is used by example-app/prod",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestPortsArePerHost(t *testing.T) {
	f := load(t)
	f.Hosts = append(f.Hosts, Host{Name: "web-2", Address: "203.0.113.11"})
	a := f.Apps[0]
	s := a.Stacks["staging"]
	s.Host, s.Port = "web-2", 8101
	a.Stacks["staging"] = s
	if err := f.Validate(); err != nil {
		t.Errorf("the same port on two hosts: %v", err)
	}
	s.Host = "web-1"
	a.Stacks["staging"] = s
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "port 8101 on web-1 is used by example-app/prod") {
		t.Errorf("the same port on one host: %v", err)
	}
}

func TestParseRefusesUnknownFieldsAndVersions(t *testing.T) {
	if _, err := Parse([]byte(`{"formatVersion":"1","sites":[]}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := Parse([]byte(`{"formatVersion":"9","portRange":{"min":8101,"max":8199},"hosts":[{"name":"h","address":"a"}],"apps":[]}`)); err == nil || !strings.Contains(err.Error(), `formatVersion "9"`) {
		t.Errorf("unknown version: %v", err)
	}
}

func TestNextPortFull(t *testing.T) {
	f := load(t)
	f.PortRange = PortRange{Min: 8101, Max: 8102}
	if _, err := f.NextPort("web-1"); err == nil {
		t.Error("a full range gave a port")
	}
}

func TestStack(t *testing.T) {
	f := load(t)
	if a, s, err := f.Stack("example-app", "staging"); err != nil || a.Name != "example-app" || s.Port != 8102 {
		t.Errorf("Stack = %v, %+v, %v", a.Name, s, err)
	}
	for _, c := range [][2]string{{"other", "prod"}, {"example-app", "qa"}} {
		if _, _, err := f.Stack(c[0], c[1]); err == nil {
			t.Errorf("%s/%s resolved", c[0], c[1])
		}
	}
}

func TestLoopbackHostsAndLogFiles(t *testing.T) {
	f := load(t)
	a := f.Apps[0]
	a.LogFiles = []string{"security.jsonl", "../escape"}
	prod := a.Stacks["prod"]
	prod.LoopbackHosts = []string{"auth.example.com", "elsewhere.example.com"}
	a.Stacks["prod"] = prod
	f.Apps[0] = a
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), `log file "../escape"`) || !strings.Contains(err.Error(), "loopback host elsewhere.example.com") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), "security.jsonl") || strings.Contains(err.Error(), "loopback host auth.example.com") {
		t.Errorf("a good value was refused: %v", err)
	}
}

func TestEdge(t *testing.T) {
	f := load(t)
	f.Apps[0].Edge = Cloudflare
	if err := f.Validate(); err != nil {
		t.Errorf("cloudflare with a domain: %v", err)
	}
	f.Apps[0].Domain = ""
	f.Apps[0].Stacks = map[string]Stack{"prod": {Host: "web-1", Hosts: []string{"app.example.org"}, Port: 8101, Status: Planned}}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "needs its domain") {
		t.Errorf("cloudflare without a domain: %v", err)
	}
	f.Apps[0].Edge = "akamai"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), `edge "akamai"`) {
		t.Errorf("an unknown edge: %v", err)
	}
}

func TestAddressFrom(t *testing.T) {
	f := load(t)
	f.Hosts[0] = Host{Name: "web-1", AddressFrom: &AddressSource{Pulumi: &PulumiOutput{Dir: "stack", Stack: "prod", Output: "staticIp"}}}
	if err := f.Validate(); err != nil {
		t.Errorf("pulumi: %v", err)
	}
	f.Hosts[0].Address = "203.0.113.10"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("both: %v", err)
	}
	f.Hosts[0] = Host{Name: "web-1", AddressFrom: &AddressSource{Pulumi: &PulumiOutput{Dir: "stack", Stack: "prod; rm", Output: "staticIp"}}}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "addressFrom.pulumi") {
		t.Errorf("a bad stack name: %v", err)
	}
	f.Hosts[0] = Host{Name: "web-1"}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "needs an address") {
		t.Errorf("none: %v", err)
	}
}

func goodServer() *DatabaseServer {
	return &DatabaseServer{
		Endpoint: "db.example.internal", AdminUser: "admin", AdminDatabase: "fleet",
		AdminPassword: SecretSource{PulumiConfig: &PulumiConfig{Dir: "stack", Stack: "prod", Key: "fleet:dbMasterPassword"}},
		Via:           "web-1", Lockdown: []string{"fleet"}, Reachable: []string{"template1"},
	}
}

func TestDatabaseServer(t *testing.T) {
	f := load(t)
	f.DatabaseServer = goodServer()
	if err := f.Validate(); err != nil {
		t.Fatalf("good: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*DatabaseServer)
		want   string
	}{
		"no endpoint": {func(s *DatabaseServer) { s.Endpoint = "" }, "needs an endpoint"},
		"two endpoints": {func(s *DatabaseServer) {
			s.EndpointFrom = &AddressSource{Pulumi: &PulumiOutput{Dir: "stack", Stack: "prod", Output: "dbEndpoint"}}
		}, "not both"},
		"two passwords":     {func(s *DatabaseServer) { s.AdminPassword.Parameter = "/fleet/admin" }, "exactly one"},
		"no password":       {func(s *DatabaseServer) { s.AdminPassword = SecretSource{} }, "exactly one"},
		"relative param":    {func(s *DatabaseServer) { s.AdminPassword = SecretSource{Parameter: "fleet/admin"} }, "full name"},
		"unknown via":       {func(s *DatabaseServer) { s.Via = "web-9" }, `via "web-9"`},
		"bad admin":         {func(s *DatabaseServer) { s.AdminUser = "Admin; drop" }, "adminUser"},
		"bad lockdown":      {func(s *DatabaseServer) { s.Lockdown = []string{"x y"} }, `database "x y"`},
		"bad pulumi key":    {func(s *DatabaseServer) { s.AdminPassword.PulumiConfig.Key = "-x" }, "pulumiConfig"},
		"port out of range": {func(s *DatabaseServer) { s.Port = 70000 }, "port 70000"},
	} {
		srv := goodServer()
		tc.mutate(srv)
		f.DatabaseServer = srv
		if err := f.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestDatabaseParameters(t *testing.T) {
	d := Database{Name: "one", AppRole: true}
	if d.Owner() != "one_owner" || d.App() != "one_app" || (Database{Name: "auth"}).App() != "" {
		t.Errorf("roles: %s %s", d.Owner(), d.App())
	}
	f := load(t)
	prod := f.Apps[0].Stacks["prod"]
	prod.Databases = []Database{
		{Name: "example_auth", OwnerDSN: "AUTH_DSN", AppDSN: "NOPE"},
		{Name: "example", AppRole: true, OwnerDSN: "../escape", AppDSN: "APP_DSN"},
	}
	f.Apps[0].Stacks["prod"] = prod
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "example_auth has no app role") || !strings.Contains(err.Error(), `ownerDsn "../escape"`) {
		t.Errorf("err = %v", err)
	}
}

func TestCredentials(t *testing.T) {
	f := load(t)
	f.Credentials = &Credentials{Registry: "credentials/credentials.json", Targets: "credentials/targets.json"}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	f.Credentials = &Credentials{Events: "/abs/events.json"}
	err := f.Validate()
	for _, want := range []string{"needs a registry", "events must be relative"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v, want %q", err, want)
		}
	}
}

func TestLabels(t *testing.T) {
	f := load(t)
	f.Apps[0].Labels = map[string]string{"org": "example", "personalData": "adults,children"}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	f.Apps[0].Labels = map[string]string{"has space": "x"}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), `label "has space"`) {
		t.Errorf("a bad label: %v", err)
	}
}

// TestCloudflareHostsAreOneLabelUnderTheZone checks that, behind
// cloudflare, a host name two labels under the zone is refused, since the
// zone's certificates cover only the zone and its wildcard, and that
// without an edge it is allowed.
func TestCloudflareHostsAreOneLabelUnderTheZone(t *testing.T) {
	f := load(t)
	a := f.Apps[0]
	a.Edge = Cloudflare
	prod := a.Stacks["prod"]
	prod.Hosts = []string{"app.example.com", "example.com", "api.app.example.com"}
	a.Stacks["prod"] = prod
	f.Apps[0] = a
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "host name api.app.example.com is more than one label under example.com") {
		t.Errorf("a deep host behind cloudflare: %v", err)
	}
	if err != nil && (strings.Contains(err.Error(), "host name app.example.com is") || strings.Contains(err.Error(), "host name example.com is")) {
		t.Errorf("the zone or a one-label host was refused: %v", err)
	}
	a.Edge = NoEdge
	f.Apps[0] = a
	if err := f.Validate(); err != nil {
		t.Errorf("a deep host without an edge: %v", err)
	}
	for host, want := range map[string]bool{"example.com": true, "app.example.com": true, "a.b.example.com": false, "badexample.com": false} {
		if got := oneLabelUnder(host, "example.com"); got != want {
			t.Errorf("oneLabelUnder(%q) = %v", host, got)
		}
	}
}

func TestHostLightsailName(t *testing.T) {
	f := load(t)
	f.Hosts[0].LightsailName = "Web-1"
	if err := f.Validate(); err != nil {
		t.Errorf("a valid Lightsail name: %v", err)
	}
	f.Hosts[0].LightsailName = "web 1"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "lightsailName") {
		t.Errorf("an invalid Lightsail name: %v", err)
	}
}
