# Getting started

This guide takes you from nothing to a first deploy: installing deployforge, writing a minimal fleet document, checking it, preparing a host, and shipping a release. For every field of the fleet document, see [Fleet document](fleet-document.md). For what a deploy does, see [Deploying](deploying.md). For host preparation in detail, see [Hosts](hosts.md).

## Install

```bash
go install github.com/plexusone/deployforge/cmd/deployforge@latest
```

## What you need

- A Linux host with systemd and Caddy 2. `host setup` and `caddy install` prepare the rest.
- SSH access to the host as a user with sudo. The default user is `ubuntu`; set `sshUser` on the host to change it.
- A parameter store for secrets. deployforge reads them from AWS SSM Parameter Store, using your AWS profile and region (`AWS_PROFILE`, `AWS_REGION`).
- An app repository with a release script (`ops/host/release.sh` by default) that builds the app for linux/amd64 and calls `release package`.
- A linux/amd64 build of deployforge for the host. It runs there during a deploy. Pass it with `--host-binary` or `DEPLOYFORGE_HOST_BINARY`.

## A minimal fleet document

Save this as `fleet.json` in your operations repository:

```json
{
  "formatVersion": "1",
  "portRange": { "min": 8101, "max": 8199 },
  "hosts": [{ "name": "web-1", "address": "203.0.113.10" }],
  "apps": [
    {
      "name": "example-app",
      "repo": "github.com/example-org/example-app",
      "domain": "example.com",
      "secretPrefix": "/example/{stack}/example-app/",
      "stacks": {
        "prod": {
          "host": "web-1",
          "hosts": ["app.example.com"],
          "port": 8101,
          "status": "live",
          "limits": { "memoryMax": "512M", "cpuQuota": "50%", "connectionLimit": 10 }
        }
      }
    }
  ]
}
```

A live stack needs `limits`. The app gets certificates from Caddy itself, because it names no `edge`. Set `acmeEmail` at the top level to give Caddy an address to register with.

Print the schema to get completion in your editor:

```bash
deployforge schema > fleet.schema.json
```

## Validate

```bash
deployforge validate
```

`validate` reports every problem it finds. On success it prints the counts of hosts, apps and stacks. Commands read `fleet.json` in the current directory. Use `--fleet <path>` for another file.

## Prepare the host

Run these once for each stack, and again when the host layout changes:

```bash
deployforge host setup example-app prod --apply
deployforge caddy install web-1
```

`host setup` creates the stack's Unix user, its `/opt` and `/etc` directories, its systemd unit, log rotation, and `/etc/hosts` lines for loopback host names. It never starts the service. Without `--apply` it prints the script, so you can read it first.

`caddy install` renders the host's Caddyfile, has Caddy validate it on the host, keeps the old file, and reloads Caddy.

Your parameters must exist before the first deploy. A parameter named `<prefix><VAR>` becomes an environment variable, so `/example/prod/example-app/SESSION_KEY` becomes `SESSION_KEY` in `/etc/example-app/env`.

## First deploy

Put the stack's config at `stacks/example-app/prod/config.yaml`. deployforge installs it as is. Tag the app and push the tag, then ship it:

```bash
deployforge deploy ship example-app prod --tag v1.0.0 \
  --host-binary bin/deployforge-linux-amd64
```

`ship` packages the tag in a temporary worktree, deploys it, and checks `https://<host>/readyz` for each host name from outside. Commit the ledger entry it writes under `deployments/`.

Check the result:

```bash
deployforge ledger show example-app prod
deployforge deploy status example-app prod
```

To go back to the previous release, run `deployforge deploy rollback example-app prod`.

## Next steps

- [Fleet document](fleet-document.md): every field.
- [Deploying](deploying.md): local builds, rollback, hooks and the ledger.
- [Edge](edge.md): Cloudflare zones and origin certificates.
- [Databases](databases.md): roles, snapshots and copies.
- [Credentials](credentials.md): the credential registry and the pre-flight check.
