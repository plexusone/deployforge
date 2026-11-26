# DeployForge

Ship Go apps to your own servers: release archives, not containers.

DeployForge deploys Go web apps to hosts you already have. Each app is a
single binary with a YAML config, run by systemd behind Caddy, with its
secrets in a parameter store. A deploy builds a release from a clean,
tagged commit, ships it with the app's config and secrets, runs the app's
`validate` and `migrate`, switches to it, checks readiness, and restores
the previous release on any failure, recording every release in a ledger.

Your repository keeps the configuration; deployforge is the binary.

> **Status:** v0.1.0. Deploying, the host, the edge, databases and
> credentials work; static sites on Cloudflare Workers are next. The
> [guides](docs/guides/getting-started.md) cover every command, and the
> design is in [docs/specs](docs/specs/initiatives/INIT-DEPLOYFORGE-001/PRD.md).

## Features

- **Fleet document:** hosts, apps and their named stacks as JSON with a
  generated schema, validated before anything runs. A host's address can
  come from a Pulumi output.
- **Releases from a clean, tagged, pushed commit:** a reproducible archive
  with its revision and checksum, packaged in a temporary worktree.
- **Safe deploys:** release directories, an atomic switch, readiness on
  every host name, automatic restore, a deploy lock and an audit log.
- **Secrets from a parameter store** (AWS SSM first): never in config
  files, arguments or logs.
- **A ledger** of every deploy and rollback, kept in your repository.
- **Hooks** around packaging, deploy and rollback.
- **The host around the apps:** systemd units with a hardening sandbox
  and limits, an idempotent host-setup script, and a Caddyfile per host,
  validated on the host before it is reloaded.
- **The edge, optionally:** Cloudflare DNS zones checked against their
  name servers, a security baseline applied through Cloudflare's API, and
  origin certificates made on the host and signed by Cloudflare. An app
  with no edge gets certificates from Caddy itself.
- **Databases:** owner and app roles per database with connection limits
  and SCRAM verifiers, their passwords kept in the parameter store, and a
  snapshot of the server before deploying a primary stack.
- **Credentials:** the fleet document names a credential registry
  ([systemspec-credential](https://github.com/plexusone/systemspec-credential)
  format), checked, reported on and installed through
  [credforge](https://github.com/plexusone/credforge), which also makes
  random values and keys where they are missing. `ship` checks that
  every secret the registry stores under a stack's prefix is in the
  parameter store before it packages anything.
- **Access per stack:** an IAM policy and an operators' group for each
  stack's parameter-store prefix.
- **A firewall check:** `host ports` reads a Lightsail instance's port
  states and fails unless HTTPS is open exactly to Cloudflare's ranges,
  SSH only to the admin addresses, and nothing else is open.
- **Runbooks:** a recurring deploy runbook for each live stack, run step
  by step or a phase at a time, and `runbookkit`, the pieces a tool's own
  runbooks keep needing.

## Commands

| Command | Does |
|---|---|
| `validate`, `schema`, `next-port`, `unit` | Check the fleet document, print its schema, find a free port, print a unit |
| `host setup <app> <stack> [--apply]` | Prepare a stack's user, directories and unit on its host |
| `caddyfile <host>`, `caddy install <host>` | Render a host's Caddyfile, and install it after Caddy validates it there |
| `release package <app>` | Package a clean, tagged checkout |
| `deploy ship\|release\|rollback\|unlock <app> <stack>` | Deploy, roll back, or clear a stale lock |
| `deploy status <app> <stack>` | The live release, releases kept and lock on the host, against the ledger |
| `deploy unpushed [--fetch]` | Commits live releases were built from that are on no remote branch |
| `ledger record\|show\|current` | Record and read the deploy ledger |
| `ledger import <app> <stack> --from <file>` | Bring in another tool's ledger, checking every entry |
| `db roles plan\|apply\|verify <app> <stack>` | Provision a stack's databases and roles |
| `db snapshot plan\|apply\|verify\|list` | Snapshot the database server |
| `db copy run\|verify\|promote <app> <stack>` | Move a stack's databases to another server: copy, compare row counts, switch its parameters |
| `db import <app> <stack> <database> --file <dump>` | Load a data-only dump as the database's owner, in one transaction; rolled back without `--confirm` |
| `zones validate\|verify\|get\|page\|schema` | Check DNS zones against their name servers |
| `zones harden plan\|apply\|verify` | Apply the security baseline to Cloudflare zones |
| `zones tls [<host>...]` | Check from outside that each host refuses TLS 1.1 and accepts TLS 1.2 |
| `origin-cert install\|issue\|list\|revoke` | Cloudflare origin certificates, made on the host |
| `credentials check\|generate\|install\|review\|status\|record\|report\|validate\|…` | The fleet's credential registry: observe the stores, generate or install a value, review, record and report |
| `credentials setup --system <app> --env <stack> list\|show\|run\|verify\|doc` | Set a stack's credentials up step by step, from a runbook built from the registry |
| `credentials page <app> <stack>` | An HTML page of what the stack's credentials are, how each is made, whether it is recorded, and how to set them up |
| `credentials preflight <app> <stack>` | The stack's secret prefix against the registry: present, missing and unlisted; `--record` records the present ones |
| `iam plan\|apply\|verify\|policy [<app> <stack>]…` | Each stack's parameter-store IAM policy and operators' group; `apply` needs `--confirm`, `policy` prints one document offline |
| `cloudflare-ips [--check]` | Refresh the committed Cloudflare ranges, or check this build's against the published ones |
| `host ports <host> --admin-cidr <cidr>…` | Check a host's Lightsail firewall: 443 to Cloudflare, 22 to the admin addresses, nothing else |
| `deploy runbook --app <app> --stack <stack> list\|prepare\|run\|state\|doc\|export\|…` | A live stack's recurring deploy runbook |
| `fleet inventory\|format` | The fleet's inventory page, and the fleet document's format reference |

## Quick Start

```bash
go install github.com/plexusone/deployforge/cmd/deployforge@latest

deployforge schema > fleet.schema.json   # for your editor
deployforge validate                     # checks fleet.json
deployforge unit example-app prod        # the stack's systemd unit

# Once per stack: its user, directories and unit, then Caddy.
deployforge host setup example-app prod --apply
deployforge caddy install web-1

# A routine deploy: package the tag in a temporary worktree, deploy it with
# stacks/example-app/prod/config.yaml, and check every host name.
make build   # in a deployforge checkout: bin/deployforge-linux-amd64 for the host
deployforge deploy ship example-app prod --tag v1.2.0 \
  --host-binary bin/deployforge-linux-amd64

# A development deploy: a pushed commit on main, untagged; the archive and
# the ledger still name its commit.
deployforge deploy ship example-app prod --local

# The same outcome as one JSON report, for a script or an agent.
deployforge deploy ship example-app prod --tag v1.2.0 --format json

deployforge ledger show example-app prod
deployforge deploy rollback example-app prod
```

Every migration must work with the release before it, because a rollback
switches the code back but never the schema; the
[Deploying guide](docs/guides/deploying.md#migrations) says how.

The app's repository has a release script, `ops/host/release.sh` by
default, that builds the app for linux/amd64 and runs
`$DEPLOYFORGE release package <app> --fleet $DEPLOYFORGE_FLEET --repo . --from <build dir>`
with the arguments it was given. The script must be committed executable
(git mode 100755): `deploy ship` checks the mode in the commit it packages
and stops with the fix when it is not.

## Taking over a running stack

A stack another tool deployed with the same layout (`/opt/<unit>/releases`,
`current` and `/etc/<unit>/`) can move to deployforge without a redeploy:

```bash
# Bring in its ledger; --app-field names the field that holds the app.
deployforge ledger import example-app prod --from old/example-app.jsonl --app-field site
# The host and the ledger must agree on the live release.
deployforge deploy status example-app prod
# The next deploy takes the lock and starts the audit log.
deployforge deploy ship example-app prod --tag v1.2.3
```

The other tool takes no deploy lock, so once a stack has moved, deploy it
only with deployforge.

## Documentation

- [Guides](docs/guides/getting-started.md): getting started, the fleet
  document, deploying, hosts, the edge, databases, credentials, taking
  over a stack, and embedding deployforge
- [Release notes](docs/releases/v0.1.0.md) and the [changelog](CHANGELOG.md)
- [Design](docs/specs/initiatives/INIT-DEPLOYFORGE-001/PRD.md)
- [API reference](https://pkg.go.dev/github.com/plexusone/deployforge)

The site is built with MkDocs: `mkdocs serve` from the repository root.

## Development

```sh
make test     # go test ./...
make lint     # golangci-lint with the organization's shared configuration, as CI runs it
```

`make lint` reads the shared configuration from a checkout of the
organization's `.github` repository beside this one; set
`LINT_CONFIG_REPO` to point elsewhere. It enables the gofmt and goimports
formatters, which a plain `golangci-lint run` without it does not.

## License

MIT
