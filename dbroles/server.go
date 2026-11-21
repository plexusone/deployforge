package dbroles

import (
	"context"
	"errors"
	"fmt"

	"github.com/plexusone/deployforge/deploy/hostaddr"
	"github.com/plexusone/deployforge/fleet"
)

// Resolved is the fleet's database server with what provisioning reads
// from elsewhere: its endpoint and its administrator's password.
type Resolved struct {
	Server Server
	// Port is the server's port, 5432 when the document gives none.
	Port int
	// AdminUser, AdminDatabase and AdminPassword are how provisioning
	// connects. The password is a secret: never print it.
	AdminUser, AdminDatabase, AdminPassword string
	// Via is the fleet host psql runs on.
	Via fleet.Host
}

// ResolveServer reads the fleet's database server: its endpoint from the
// document or its source, and its administrator's password from the
// parameter store or Pulumi.
func ResolveServer(ctx context.Context, f *fleet.Fleet, store Store, run hostaddr.Runner) (Resolved, error) {
	srv := f.DatabaseServer
	if srv == nil {
		return Resolved{}, errors.New("dbroles: the fleet document has no databaseServer")
	}
	endpoint := srv.Endpoint
	if srv.EndpointFrom != nil {
		var err error
		if endpoint, err = hostaddr.Address(ctx, *srv.EndpointFrom, run); err != nil {
			return Resolved{}, fmt.Errorf("dbroles: the server's endpoint: %w", err)
		}
	}
	var password string
	switch pw := srv.AdminPassword; {
	case pw.Parameter != "":
		if store == nil {
			return Resolved{}, errors.New("dbroles: the administrator's password is a parameter, but there is no parameter store")
		}
		v, found, err := store.Get(ctx, pw.Parameter)
		if err != nil {
			return Resolved{}, fmt.Errorf("dbroles: reading %s: %w", pw.Parameter, err)
		}
		if !found || v == "" {
			return Resolved{}, fmt.Errorf("dbroles: no administrator's password at %s", pw.Parameter)
		}
		password = v
	case pw.PulumiConfig != nil:
		v, err := hostaddr.Config(ctx, *pw.PulumiConfig, run)
		if err != nil {
			return Resolved{}, fmt.Errorf("dbroles: the administrator's password: %w", err)
		}
		password = v
	default:
		return Resolved{}, errors.New("dbroles: the fleet document names no administrator's password")
	}
	via, ok := f.Host(srv.Via)
	if !ok {
		return Resolved{}, fmt.Errorf("dbroles: no host %q", srv.Via)
	}
	via, err := hostaddr.Resolve(ctx, via, run)
	if err != nil {
		return Resolved{}, err
	}
	port := srv.Port
	if port == 0 {
		port = 5432
	}
	return Resolved{
		Server:    Server{Host: endpoint, Port: srv.Port, Lockdown: srv.Lockdown, Reachable: srv.Reachable},
		Port:      port,
		AdminUser: srv.AdminUser, AdminDatabase: srv.AdminDatabase, AdminPassword: password,
		Via: via,
	}, nil
}

// PSQL is a runner over ssh to the server's via host, with key when given.
func (r Resolved) PSQL(key string) PSQL {
	ssh := []string{"ssh", "-o", "BatchMode=yes"}
	if key != "" {
		ssh = append(ssh, "-i", key, "-o", "IdentityAgent=SSH_AUTH_SOCK")
	}
	ssh = append(ssh, r.Via.User()+"@"+r.Via.Address)
	return PSQL{SSH: ssh, Host: r.Server.Host, Port: r.Port, User: r.AdminUser, Database: r.AdminDatabase, Password: r.AdminPassword}
}
