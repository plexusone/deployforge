# PRD: deployforge — ship Go apps to your own servers

**Initiative:** INIT-DEPLOYFORGE-001
**Status:** proposed

## Problem

A small team running a few Go web apps often wants them on one or two
virtual machines it controls, not on a platform: each app a single static
binary with a YAML config, run by systemd, behind a reverse proxy that
terminates TLS, with a CDN and firewall in front and a managed PostgreSQL
behind. It is cheap, fast and easy to reason about.

The deploy itself is where it goes wrong. It starts as a shell script and
grows until it is the riskiest code the team owns:

- A release is built from a dirty checkout, or from a commit nobody pushed,
  and nobody can say later which code is running.
- Secrets are copied by hand into an env file on the server, or into the
  script, and drift from where they are meant to live.
- The new binary is started before its database migration ran, or the
  migration ran and the binary failed to start, and there is no clean way
  back.
- A restart that fails its health check leaves the site down until someone
  notices.
- Adding a second app to the same host means editing the proxy's config by
  hand, picking a free port, and hoping two apps never collide.
- Each team writes this again, privately, because the tools that exist
  assume containers (and a registry), or build on the server from git, or
  have no health gate.

## Goal

deployforge deploys Go apps as release archives to hosts the team already
has, safely and repeatably, from config files the team keeps in its own
repository. One public binary does the work; the private repository holds
only configuration, secrets' locations and the team's own runbooks.

A deploy is: build a release from a clean tagged commit, ship it with the
app's config and its secrets, validate and migrate, switch, check health,
and roll back on failure, recording what was deployed. deployforge also
renders what the host needs around the apps (systemd units, the reverse
proxy's config, host setup) and, optionally, the CDN in front of them.

## Users

- **An operator** of a few apps on one or more hosts, who wants one command
  per deploy and a record of every release.
- **A coding agent** working for that operator: it prepares config, runs
  plans and checks, and reads the ledger, while the operator approves what
  cannot be undone.
- **A host tool** that embeds deployforge's packages and commands, adding
  its own commands beside them.

## Requirements

### Phase 1: Deploy a release

1. **Config files, not code.** A fleet document lists the hosts, the apps,
   each app's stacks (such as prod and staging), their host names, ports,
   databases, resource limits and secret prefix. A JSON Schema is
   generated from the Go types, and `deployforge validate` checks the
   document: ports unique per host, host names unique, every required field
   set.
2. **Releases from a clean, tagged commit.** `release package` refuses a
   dirty checkout, a `replace` directive, a revision that is not the tag's,
   or an unpushed commit, and writes an archive with its revision and
   checksum. A release can also be packaged by the app's own script, which
   calls `release package`.
3. **Secrets from a parameter store, never from the command line.** A
   stack's secrets are read at deploy time from its prefix: variables under
   the prefix become the unit's environment file, variables under
   `migrate/` reach only the migrate step, and parameters under `files/`
   become files in the unit's config directory. No value is printed or
   logged. AWS SSM Parameter Store first; other stores through a provider
   interface.
4. **A safe switch.** On the host, the release is unpacked into
   `releases/<id>`; the config and secrets are installed with the previous
   ones kept; the app's `validate` and `migrate` run as the app's user; the
   `current` symlink switches atomically and the unit restarts; the deploy
   waits for the app's readiness endpoint on each of its host names. Any
   failure restores the previous release and config. Old releases are
   pruned to a count.
5. **One command per deploy.** `deploy ship <app> <stack> --tag vX.Y.Z`
   packages the release, optionally snapshots the database, deploys, and
   checks readiness through the public host names. `deploy rollback`
   switches back to the previous release.
6. **A ledger.** Every deploy appends who, when, which revision and which
   release to a JSON Lines file per app, kept in the operator's repository.
7. **Locks and an audit trail.** A deploy takes a lock on the host, so two
   cannot overlap; the host keeps an audit log of deploys and rollbacks.

### Phase 2: The host around the apps

1. **systemd units** rendered per app stack: user, directories, sandboxing,
   resource limits, an environment file, `validate` before start.
2. **Host setup** as an idempotent script per app stack: the user, the
   directories, the unit, log rotation.
3. **Reverse proxy:** Caddy's config rendered from the fleet document, one
   site block per app host, with origin certificates and the CDN's
   address ranges as trusted proxies; validated on the host before it
   replaces the running config, then reloaded, never restarted.
4. **Ports** allocated from a configurable range, with `next-port`.

### Phase 3: The edge (optional)

1. **DNS zones** as a document: records checked against the CDN's API,
   with planned records that a step makes real.
2. **Hardening:** a baseline of TLS settings, firewall rules and rate limits
   applied per zone from config, with plan, apply and verify.
3. **Origin certificates:** a key made on the host, a CSR signed by the
   CDN, the certificate installed beside the key, with no key leaving the
   host.
4. **Static sites on the CDN's edge workers**, deployed from a tagged
   commit and recorded like app releases.

Cloudflare first; each is optional, and a fleet without a CDN still
deploys.

### Phase 4: Databases

1. **Roles per app:** an owner role that migrates and an app role that
   runs, each with a connection limit, their passwords generated into the
   parameter store and set on the server as SCRAM verifiers.
2. **Snapshots** before a prod deploy, and on demand.

## Non-Goals

- Containers, image registries or a container runtime.
- Building on the server.
- Provisioning the hosts or the database server; a separate tool (such as
  Pulumi) makes them, and deployforge reads their addresses.
- Orchestrating many hosts per app, autoscaling or blue/green across
  machines in the first release.
- Holding secret values in config files or in deployforge's own state.

## Success Measures

- A team moves an existing app to deployforge by writing config only.
- A failed deploy leaves the previous release serving, every time, without
  a person stepping in.
- Every running release can be traced from the ledger to a pushed, tagged
  commit.
- No secret value appears in any file in the operator's repository, in any
  log, or on the command line.
