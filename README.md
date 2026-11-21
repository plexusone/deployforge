# deployforge

Ship Go apps to your own servers: release archives, not containers.

deployforge deploys Go web apps to hosts you already have. Each app is a
single binary with a YAML config, run by systemd behind Caddy, with its
secrets in a parameter store. A deploy builds a release from a clean,
tagged commit, ships it with the app's config and secrets, runs the app's
`validate` and `migrate`, switches to it, checks readiness, and restores
the previous release on any failure, recording every release in a ledger.

Your repository keeps the configuration; deployforge is the binary.

> **Status:** in development, not yet released. Deploying a release works;
> the host, edge and database commands are next. The design is in
> [docs/specs](docs/specs/initiatives/INIT-DEPLOYFORGE-001/PRD.md).

## Features

- **Fleet document:** hosts, apps and their named stacks as JSON with a
  generated schema, validated before anything runs.
- **Releases from a clean, tagged, pushed commit:** a reproducible archive
  with its revision and checksum, packaged in a temporary worktree.
- **Safe deploys:** release directories, an atomic switch, readiness on
  every host name, automatic restore, a deploy lock and an audit log.
- **Secrets from a parameter store** (AWS SSM first): never in config
  files, arguments or logs.
- **A ledger** of every deploy and rollback, kept in your repository.
- **Hooks** around packaging, deploy and rollback.
- **systemd units** with a hardening sandbox and resource limits.

Planned: host setup and the Caddyfile, Cloudflare zones, hardening, origin
certificates and Workers sites, and database roles and snapshots.

## Quick Start

```bash
go install github.com/plexusone/deployforge/cmd/deployforge@latest

deployforge schema > fleet.schema.json   # for your editor
deployforge validate                     # checks fleet.json
deployforge unit example-app prod        # the stack's systemd unit

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

## License

MIT
