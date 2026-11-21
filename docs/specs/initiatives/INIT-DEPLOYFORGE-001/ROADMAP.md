# ROADMAP: deployforge

**Initiative:** INIT-DEPLOYFORGE-001

Phases group the work; each phase's status follows from its items.

## Phase 1: Deploy a release

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-001 | Fleet document: types, named stacks, per-stack databases, port range, validation and generated schema | planned |
| RMI-DEPLOYFORGE-002 | Release packaging from a clean, tagged, pushed checkout, with revision and checksum | planned |
| RMI-DEPLOYFORGE-003 | Secrets: a stack's prefix as environment, migrate-only variables and key files, behind a `Store` interface with AWS SSM | planned |
| RMI-DEPLOYFORGE-004 | On-host deploy: unpack, install with backup, validate, migrate, atomic switch, readiness per host name, restore, prune | planned |
| RMI-DEPLOYFORGE-005 | Operator side: request, transfer over SSH, `deploy ship`, `release` and `rollback`, public readiness check, ledger | planned |
| RMI-DEPLOYFORGE-006 | Deploy lock and on-host audit log, with `deploy unlock` | planned |
| RMI-DEPLOYFORGE-007 | Hooks before and after each phase, with no secret values in their environment | planned |

## Phase 2: The host around the apps

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-008 | systemd units with sandbox and resource limits | planned |
| RMI-DEPLOYFORGE-009 | Idempotent host setup per app stack | planned |
| RMI-DEPLOYFORGE-010 | Caddyfile from the fleet document, validated on the host and reloaded | planned |
| RMI-DEPLOYFORGE-011 | Port allocation from the configured range, `next-port` | planned |
| RMI-DEPLOYFORGE-012 | Host addresses from a provisioning tool's outputs, Pulumi first | planned |

## Phase 3: The edge

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-013 | Zone document and verification against the CDN, with planned records | planned |
| RMI-DEPLOYFORGE-014 | Zone hardening baseline as data: plan, apply, verify | planned |
| RMI-DEPLOYFORGE-015 | Origin certificates made on the host and signed by the CDN | planned |
| RMI-DEPLOYFORGE-016 | Static sites on edge workers, deployed from a tag and recorded | planned |

## Phase 4: Databases

| ID | Item | Status |
|---|---|---|
| RMI-DEPLOYFORGE-017 | Owner and app roles per database with connection limits and SCRAM verifiers, passwords in the parameter store | planned |
| RMI-DEPLOYFORGE-018 | Snapshots before a prod deploy and on demand | planned |
