# deployforge

Ship Go apps to your own servers: release archives, not containers.

deployforge deploys Go web apps to hosts you already have. Each app is a
single binary with a YAML config, run by systemd behind Caddy, with its
secrets in a parameter store. A deploy builds a release from a clean,
tagged commit, ships it with the app's config and secrets, runs the app's
`validate` and `migrate`, switches to it, checks readiness, and restores
the previous release on any failure, recording every release in a ledger.

Your repository keeps the configuration; deployforge is the binary.

## What it does

| Area | What you get | Guide |
|---|---|---|
| The fleet document | Hosts, apps and their named stacks as JSON, validated before anything runs | [The Fleet Document](guides/fleet-document.md) |
| Deploys | Releases from a clean, tagged commit; an atomic switch, readiness on every host name, automatic restore, a deploy lock, an audit log and a ledger; hooks | [Deploying](guides/deploying.md) |
| Hosts | systemd units with a sandbox and limits, an idempotent host-setup script, a Caddyfile per host | [Hosts](guides/hosts.md) |
| The edge | Cloudflare zones checked against their name servers, a security baseline, origin certificates made on the host, a TLS minimum check | [The Edge](guides/edge.md) |
| Databases | Owner and app roles with connection limits and SCRAM verifiers, snapshots, copies between servers | [Databases](guides/databases.md) |
| Credentials | The credential registry the fleet names: checked, generated, installed and set up step by step; a check before every deploy; IAM per stack | [Credentials](guides/credentials.md) |
| Taking over | Moving a stack another tool deployed, without a redeploy | [Taking Over a Stack](guides/taking-over.md) |
| Embedding | Every capability as a Go package, and the command tree mountable in your own tool | [Embedding deployforge](guides/embedding.md) |

## Install

```bash
go install github.com/plexusone/deployforge/cmd/deployforge@latest
```

Then follow [Getting Started](guides/getting-started.md).

## Documentation

- [Guides](guides/getting-started.md), from a first deploy to every command
- [API reference](https://pkg.go.dev/github.com/plexusone/deployforge)
- [Design](specs/initiatives/INIT-DEPLOYFORGE-001/PRD.md): requirements, plan and roadmap
- [Releases](releases/changelog.md), the latest [v0.3.0](releases/v0.3.0.md)
