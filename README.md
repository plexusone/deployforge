# deployforge

Ship Go apps to your own servers: release archives, not containers.

deployforge deploys Go web apps to hosts you already have. Each app is a
single binary with a YAML config, run by systemd behind Caddy, with its
secrets in a parameter store. A deploy builds a release from a clean,
tagged commit, ships it with the app's config and secrets, runs the app's
`validate` and `migrate`, switches to it, checks readiness, and restores
the previous release on any failure, recording every release in a ledger.

Your repository keeps the configuration; deployforge is the binary.

> **Status:** in development. The design is in
> [docs/specs](docs/specs/initiatives/INIT-DEPLOYFORGE-001/PRD.md).

## Features (planned)

- **Fleet document:** hosts, apps and their stacks as JSON with a generated
  schema, validated before anything runs.
- **Safe deploys:** release directories, an atomic switch, readiness per
  host name, automatic restore, a deploy lock and an audit log.
- **Secrets from a parameter store:** never in config files, arguments or
  logs.
- **The host around the apps:** systemd units, host setup and the
  Caddyfile rendered from the fleet document.
- **The edge, optionally:** Cloudflare zones, hardening, origin
  certificates and Workers sites.
- **Databases:** owner and app roles with connection limits, and snapshots
  before a deploy.

## License

MIT
