# ROADMAP: deployforge

**Initiative:** `INIT-DEPLOYFORGE-001`
**Repository:** `github.com/plexusone/deployforge`

Phases group the work; each phase's status follows from its items. A
checked item is done; an unchecked one is planned unless its note says
otherwise.

## Phase 1 — Deploy a release

- [x] `RMI-DEPLOYFORGE-001` Fleet document: types, named stacks, per-stack databases, port range, validation and generated schema
- [x] `RMI-DEPLOYFORGE-002` Release packaging from a clean, tagged, pushed checkout, with revision and checksum
- [x] `RMI-DEPLOYFORGE-003` Secrets: a stack's prefix as environment, migrate-only variables and key files, behind a `Store` interface with AWS SSM
- [x] `RMI-DEPLOYFORGE-004` On-host deploy: unpack, install with backup, validate, migrate, atomic switch, readiness per host name, restore, prune
- [x] `RMI-DEPLOYFORGE-005` Operator side: request, transfer over SSH, `deploy ship`, `release` and `rollback`, public readiness check, ledger
- [x] `RMI-DEPLOYFORGE-006` Deploy lock and on-host audit log, with `deploy unlock`
- [x] `RMI-DEPLOYFORGE-007` Hooks around packaging, deploy and rollback, and on failure, with no secret values in their environment

## Phase 2 — The host around the apps

- [x] `RMI-DEPLOYFORGE-008` systemd units with sandbox and resource limits
- [x] `RMI-DEPLOYFORGE-009` Idempotent host setup per app stack
- [x] `RMI-DEPLOYFORGE-010` Caddyfile from the fleet document, validated on the host and reloaded
- [x] `RMI-DEPLOYFORGE-011` Port allocation from the configured range, `next-port`
- [x] `RMI-DEPLOYFORGE-012` Host addresses from a provisioning tool's outputs, Pulumi first

## Phase 3 — The edge

- [x] `RMI-DEPLOYFORGE-013` Zone document and verification against the CDN, with planned records
- [x] `RMI-DEPLOYFORGE-014` Zone hardening baseline as data: plan, apply, verify
- [x] `RMI-DEPLOYFORGE-015` Origin certificates made on the host and signed by the CDN
- [ ] `RMI-DEPLOYFORGE-016` Static sites on edge workers, deployed from a tag and recorded
  - Blocked: needs a public Cloudflare Workers upload client

## Phase 4 — Databases

- [x] `RMI-DEPLOYFORGE-017` Owner and app roles per database with connection limits and SCRAM verifiers, passwords in the parameter store
- [x] `RMI-DEPLOYFORGE-018` Snapshots before a prod deploy and on demand

## Phase 5 — Credentials and access

- [x] `RMI-DEPLOYFORGE-019` `credentials`: the credential registry the fleet document names, with credforge's check, install and review and systemspec-credential's commands; targets from the fleet
- [x] `RMI-DEPLOYFORGE-020` Pre-flight in `ship`: every secret the registry stores under the stack's prefix is in the parameter store before packaging
- [x] `RMI-DEPLOYFORGE-021` `iam`: a parameter-store IAM policy and operators' group per stack: plan, apply, verify
- [x] `RMI-DEPLOYFORGE-022` `credentials generate`: random values and keys made where missing, through credforge
- [x] `RMI-DEPLOYFORGE-029` `credentials setup` and `credentials page`: a stack's credentials as a runbook to run, and as an HTML page of what is needed, how each is made, what is recorded and how to set them up

## Phase 6 — Taking over running stacks

- [x] `RMI-DEPLOYFORGE-023` Adopt a stack another tool deployed: `ledger import` with a renamed app field, and `deploy status` comparing the host's live release, releases kept and lock with the ledger
- [x] `RMI-DEPLOYFORGE-024` `deploy unpushed`: commits live releases were built from that are on no remote branch
- [x] `RMI-DEPLOYFORGE-025` Local npm packages in a local build, packed from their checkouts
- [x] `RMI-DEPLOYFORGE-026` Fleet checks: `zones tls`, each live host name's minimum TLS version checked from outside
- [x] `RMI-DEPLOYFORGE-027` `db copy`: a stack's databases copied between servers, with row counts compared, and its parameters promoted

## Phase 7 — The fleet document as the source

- [x] `RMI-DEPLOYFORGE-028` Every field a converted fleet needs, so the fleet document can be the file a fleet commits: app `labels` for facts deployforge keeps but never acts on

## Phase 8 — Proving it

**Theme:** Deploy real stacks and record how it goes, before adding features.

- [ ] `RMI-DEPLOYFORGE-030` Acceptance tests on a throwaway host like production: a normal deploy, an unhealthy release, a deploy killed partway, and a migration the previous release cannot run against
- [x] `RMI-DEPLOYFORGE-031` `--format json` reports from `ship`, `release`, `rollback` and `status`: the outcome, the phase and step that failed, what was kept or restored, and whether the migration completed
- [x] `RMI-DEPLOYFORGE-032` Operator documentation: the expand-then-contract migration rule, local builds and JSON reports in the Quick Start
- [ ] `RMI-DEPLOYFORGE-033` The first stack deployed by deployforge in production
- [ ] `RMI-DEPLOYFORGE-034` A month of real use recorded: every deploy, failure and rollback in the ledger, with a note on each failure

## Records for a control plane

Two items let a control plane read deployforge's files; they are planned
with that work rather than in a phase here: `RMI-DEPLOYFORGE-035`, which
keeps failed deploys beside each app's ledger, and `RMI-DEPLOYFORGE-036`,
which maps each app to the site domain it is known by.
