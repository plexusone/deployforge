# deployforge

Ship Go apps to your own servers: release archives, not containers.

deployforge deploys Go web apps to hosts you already have. Each app is a
single binary with a YAML config, run by systemd behind Caddy, with its
secrets in a parameter store. A deploy builds a release from a clean,
tagged commit, ships it with the app's config and secrets, runs the app's
`validate` and `migrate`, switches to it, checks readiness, and restores
the previous release on any failure, recording every release in a ledger.

Your repository keeps the configuration; deployforge is the binary.

> **Status:** in development, not yet released. Deploying, the host, the
> edge and databases work; static sites on Cloudflare Workers are next.
> The design is in [docs/specs](docs/specs/initiatives/INIT-DEPLOYFORGE-001/PRD.md).

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
| `zones validate\|verify\|get\|page\|schema` | Check DNS zones against their name servers |
| `zones harden plan\|apply\|verify` | Apply the security baseline to Cloudflare zones |
| `zones tls [<host>...]` | Check from outside that each host refuses TLS 1.1 and accepts TLS 1.2 |
| `origin-cert install\|issue\|list\|revoke` | Cloudflare origin certificates, made on the host |
| `credentials check\|generate\|install\|review\|status\|record\|report\|validate\|…` | The fleet's credential registry: observe the stores, generate or install a value, review, record and report |
| `credentials setup --system <app> --env <stack> list\|show\|run\|verify\|doc` | Set a stack's credentials up step by step, from a runbook built from the registry |
| `credentials page <app> <stack>` | An HTML page of what the stack's credentials are, how each is made, whether it is recorded, and how to set them up |
| `iam plan\|apply\|verify [<app> <stack>]…` | Each stack's parameter-store IAM policy and operators' group |

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

deployforge ledger show example-app prod
deployforge deploy rollback example-app prod
```

The app's repository has a release script, `ops/host/release.sh` by
default, that builds the app for linux/amd64 and runs
`$DEPLOYFORGE release package <app> --fleet $DEPLOYFORGE_FLEET --repo . --from <build dir>`
with the arguments it was given.

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

## License

MIT
