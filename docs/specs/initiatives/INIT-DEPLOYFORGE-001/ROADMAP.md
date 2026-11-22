# ROADMAP: deployforge

**Initiative:** INIT-DEPLOYFORGE-001

Phases group the work; each phase's status follows from its items.

## Phase 1: Deploy a release

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-001 | Fleet document: types, named stacks, per-stack databases, port range, validation and generated schema | done |
| RMI-DEPLOYFORGE-002 | Release packaging from a clean, tagged, pushed checkout, with revision and checksum | done |
| RMI-DEPLOYFORGE-003 | Secrets: a stack's prefix as environment, migrate-only variables and key files, behind a `Store` interface with AWS SSM | done |
| RMI-DEPLOYFORGE-004 | On-host deploy: unpack, install with backup, validate, migrate, atomic switch, readiness per host name, restore, prune | done |
| RMI-DEPLOYFORGE-005 | Operator side: request, transfer over SSH, `deploy ship`, `release` and `rollback`, public readiness check, ledger | done |
| RMI-DEPLOYFORGE-006 | Deploy lock and on-host audit log, with `deploy unlock` | done |
| RMI-DEPLOYFORGE-007 | Hooks around packaging, deploy and rollback, and on failure, with no secret values in their environment | done |

## Phase 2: The host around the apps

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-008 | systemd units with sandbox and resource limits | done |
| RMI-DEPLOYFORGE-009 | Idempotent host setup per app stack | done |
| RMI-DEPLOYFORGE-010 | Caddyfile from the fleet document, validated on the host and reloaded | done |
| RMI-DEPLOYFORGE-011 | Port allocation from the configured range, `next-port` | done |
| RMI-DEPLOYFORGE-012 | Host addresses from a provisioning tool's outputs, Pulumi first | done |

## Phase 3: The edge

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-013 | Zone document and verification against the CDN, with planned records | done |
| RMI-DEPLOYFORGE-014 | Zone hardening baseline as data: plan, apply, verify | done |
| RMI-DEPLOYFORGE-015 | Origin certificates made on the host and signed by the CDN | done |
| RMI-DEPLOYFORGE-016 | Static sites on edge workers, deployed from a tag and recorded | blocked: needs a public Cloudflare Workers upload client |

## Phase 4: Databases

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-017 | Owner and app roles per database with connection limits and SCRAM verifiers, passwords in the parameter store | done |
| RMI-DEPLOYFORGE-018 | Snapshots before a prod deploy and on demand | done |

## Phase 5: Credentials and access

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-019 | `credentials`: the credential registry the fleet document names, with credforge's check, install and review and systemspec-credential's commands; targets from the fleet | done |
| RMI-DEPLOYFORGE-020 | Pre-flight in `ship`: every secret the registry stores under the stack's prefix is in the parameter store before packaging | done |
| RMI-DEPLOYFORGE-021 | `iam`: a parameter-store IAM policy and operators' group per stack: plan, apply, verify | done |
| RMI-DEPLOYFORGE-022 | `credentials generate`: random values and keys made where missing, through credforge | done |

## Phase 6: Taking over running stacks

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-023 | Adopt a stack another tool deployed: `ledger import` with a renamed app field, and `deploy status` comparing the host's live release, releases kept and lock with the ledger | done |
| RMI-DEPLOYFORGE-024 | `deploy unpushed`: commits live releases were built from that are on no remote branch | done |
| RMI-DEPLOYFORGE-025 | Local npm packages in a local build, packed from their checkouts | done |
| RMI-DEPLOYFORGE-026 | Fleet checks: TLS minimum version and the like, each passing or failing | planned |
| RMI-DEPLOYFORGE-027 | `db copy`: a stack's databases copied between servers, with row counts compared, and its parameters promoted | done |

## Phase 7: The fleet document as the source

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-028 | Every field a converted fleet needs, so the fleet document can be the file a fleet commits | planned |
