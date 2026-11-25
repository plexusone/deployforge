# Embedding

This guide is for Go developers who want deployforge inside their own tools. deployforge is library first: each capability is a package with no command-line code, and the `deployforge` command is a thin layer over them. You can import the packages, or mount the whole command tree under your own command and add commands beside it. See [Getting started](getting-started.md) for the command line, and [Deploying](deploying.md) for what the commands do.

## Design

- Each capability lives in its own package. The `cli` package holds the Cobra command tree.
- Every CDN or cloud API sits behind an interface in the package that uses it. Cloudflare and AWS implementations live in subpackages, so tests use fakes and another provider can be added without touching the callers.
- The host side of a deploy is the same binary, run as root over SSH with a JSON request on standard input. There is no agent on the host.

## Packages

| Package | Responsibility |
|---|---|
| `fleet` | The fleet document's types, loading, validation and schema; units, users and paths derived from names |
| `schema` | The fleet and zones documents' generated JSON Schemas, embedded |
| `release`, `provenance` | Package a clean, tagged checkout into an archive with its revision and checksum; name upstream commits |
| `deploy/target` | Resolve an app stack to its host, unit, user, paths and ports |
| `deploy/secrets`, `deploy/secrets/awsssm` | Read a stack's prefix into environment, migrate-only variables and key files, and mask them; AWS SSM |
| `deploy/archive` | Verify an archive and extract it safely |
| `deploy/deployer` | The operator side: build the request, dial the stack's host, transfer, run on the host, record |
| `deploy/onhost` | The host side: lock, unpack, install, validate, migrate, switch, health, restore, prune, audit |
| `deploy/ship` | Package a tag or local commit in a temporary worktree with the app's release script |
| `deploy/unpushed` | The commits each live release was built from that are on no remote branch of their checkout |
| `deploy/readiness` | Check every host name's `/readyz` from outside |
| `deploy/hooks` | Run the operator's hooks around packaging, deploy and rollback |
| `deploy/hostaddr` | Resolve a host's or server's address, and Pulumi configuration values |
| `deploy/systemd`, `deploy/host` | Render units and their sandbox, and the idempotent host-setup script |
| `deploy/caddy`, `deploy/cfranges` | Render a host's Caddyfile; Cloudflare's published proxy ranges |
| `ledger` | Append and read the per-stack JSON Lines ledger |
| `tlsmin` | A host's minimum TLS version, checked from outside |
| `zones`, `zoneharden` | The zones document and its verification; the security baseline through Cloudflare's API |
| `origincert` | Issue, list and revoke origin certificates, made on the host |
| `dbroles`, `dbsnapshot` | Database and role plans with SCRAM verifiers and connection limits; snapshots of a Lightsail server |
| `dbcopy` | A stack's databases copied to another server, row counts compared, and its parameters promoted |
| `credpage` | One app stack's credentials as an HTML page |
| `deploy/preflight` | Before a deploy, the registry's secrets under the stack's prefix checked by name in the parameter store |
| `stackiam` | Each stack's parameter-store IAM policy and operators' group: plan, apply, verify |
| `cli` | The Cobra command tree, mountable by a host tool |

Load and check a fleet document with the `fleet` package:

```go
f, err := fleet.Load("fleet.json")
if err != nil {
	return err
}
app, stack, err := f.Stack("example-app", "prod")
```

## Mount the command tree

`cli.New` returns the root `*cobra.Command`. The `deployforge` command is just this:

```go
err := cli.New(cli.Options{}).ExecuteContext(ctx)
```

To mount it under your own tool, rename it with `Use` and add it as a subcommand:

```go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/plexusone/deployforge/cli"
)

func main() {
	root := &cobra.Command{Use: "mytool"}
	root.AddCommand(cli.New(cli.Options{
		Use:   "deploy-fleet",
		Fleet: "ops/fleet.json",
	}))
	// Add your own commands beside it.
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

The tree sets `SilenceUsage` and `SilenceErrors`, so print the returned error yourself, as the `deployforge` command does.

## Options

Every field is optional. A nil function uses the default, which is the real service.

| Field | Meaning |
|---|---|
| `Use` | The root command's name. `deployforge` when empty. |
| `Fleet` | The fleet document's default path. `fleet.json` when empty. The `--fleet` flag still overrides it. |
| `Store` | Opens the secret store a release reads. AWS SSM Parameter Store with the default AWS configuration when nil. |
| `Dial` | Reaches hosts with an SSH key. The system's `ssh` and `scp` when nil. |
| `Params` | Opens the parameter store that database provisioning reads and writes. AWS SSM Parameter Store when nil. |
| `Lightsail` | Opens the client database snapshots use. Lightsail with the default AWS configuration when nil. |
| `OriginCA` | Opens Cloudflare's Origin CA. The official client, with the token in `CLOUDFLARE_API_TOKEN`, when nil. |
| `IAM` | Opens the clients the `iam` command uses. IAM and STS with the default AWS configuration when nil. |
| `CredentialConnector` | Reaches the credential stores for the check before a deploy. credforge's, through the targets, when nil. |
| `ReleaseEnv` | Extra environment for an app's release script, as `NAME=value`, from the running binary's path and the fleet document's. For scripts at older tags that read other names for them. None when nil. |

Their types are:

```go
Store               func(c *cobra.Command) (secrets.Store, error)
Dial                func(key string) deployer.Dialer
Params              func(c *cobra.Command) (dbroles.Store, error)
Lightsail           func(c *cobra.Command) (dbsnapshot.API, error)
OriginCA            func() (origincert.API, error)
IAM                 func(c *cobra.Command) (stackiam.Clients, error)
CredentialConnector func(*targets.File) credforge.Connector
ReleaseEnv          func(self, fleet string) []string
```

Replace a field to use another secret store or transport, or to test your tool with fakes.

## Environment variables

The `cli` package exports the names of the variables the commands read: `EnvSSHKey` (`DEPLOYFORGE_SSH_KEY`), `EnvOperator` (`DEPLOYFORGE_OPERATOR`) and `EnvHostBinary` (`DEPLOYFORGE_HOST_BINARY`).
