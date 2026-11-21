// Package fleettest builds the fleet the tests share: one host, and one app,
// one-cloud, with a live prod stack and a live staging stack.
package fleettest

import "github.com/plexusone/deployforge/fleet"

// App is the shared fleet's app.
const App = "one-cloud"

// Fleet returns a new copy of the shared fleet, safe to change.
func Fleet() *fleet.Fleet {
	limits := func() *fleet.Limits { return &fleet.Limits{MemoryMax: "512M", CPUQuota: "50%", ConnectionLimit: 10} }
	return &fleet.Fleet{
		FormatVersion: fleet.FormatVersion,
		PortRange:     fleet.PortRange{Min: 8101, Max: 8199},
		Hosts:         []fleet.Host{{Name: "web-1", Address: "203.0.113.10"}},
		Apps: []fleet.App{{
			Name: App, Repo: "github.com/example/one-cloud", Domain: "one.example.com",
			SecretPrefix: "/example/{stack}/one-cloud/",
			Stacks: map[string]fleet.Stack{
				"prod": {Host: "web-1", Hosts: []string{"one.example.com", "auth.one.example.com"}, Port: 8101,
					Databases: []fleet.Database{{Name: "one", AppRole: true}}, Status: fleet.Live, Limits: limits()},
				"staging": {Host: "web-1", Hosts: []string{"stage-app.one.example.com"}, Port: 8102,
					Databases: []fleet.Database{{Name: "one_staging", AppRole: true}}, Status: fleet.Live, Limits: limits()},
			},
		}},
	}
}
