# PLAN: deployforge

**Initiative:** INIT-DEPLOYFORGE-001
**Status:** proposed

## Approach

deployforge starts from a working deploy engine rather than a blank page:
the packages, their tests and their behavior come from a tool already used
for production deploys, generalized where it assumed a particular fleet.
Each phase ends with a release a team can use.

| Phase | Delivers | Release |
|---|---|---|
| 1. Deploy a release | Fleet document, release packaging, secrets, the on-host switch with health and restore, ledger, lock and audit | v0.1.0 |
| 2. The host around the apps | Units, host setup, Caddy, ports | v0.2.0 |
| 3. The edge | Zones, hardening, origin certificates, edge workers | v0.3.0 |
| 4. Databases | Roles with connection limits, snapshots | v0.4.0 |

## Order of Work

1. **Types and schema first.** The fleet document's Go types, validation
   and generated schema, with named stacks and per-stack databases.
2. **Move packages with their tests,** one at a time, each building and
   passing before the next: `release`, `provenance`, `ledger`,
   `deploy/target`, `deploy/archive`, `deploy/secrets`, `deploy/onhost`,
   `deploy/deployer`.
3. **Add what was missing:** the lock, the audit log, hooks, and the
   secrets `Store` interface.
4. **The command tree,** mountable, with `validate`, `schema`,
   `release package`, `deploy ship|release|rollback|unlock` and `ledger`.
5. **A sample app** in `examples/` that deploys end to end against a
   disposable VM.

Phases 2 to 4 repeat the pattern for their packages.

## Design Decisions

- **Binaries, not containers.** A Go app is one static file; a registry
  and a runtime add cost and moving parts without adding safety.
- **Build on the operator's machine or CI, never on the server.** The
  server runs only what was built from a pushed, tagged commit.
- **Health-gated switch with automatic restore,** built in, not a hook.
- **Secrets stay in the parameter store** and travel only inside the
  request to the host.
- **No agent on the host.** The same binary runs there on demand, so there
  is nothing to keep running or patch.
- **Config is data with a schema.** Anything that was compiled in for one
  fleet, such as database names, port ranges or a hardening baseline,
  becomes a field.
- **The CDN is optional,** behind interfaces, Cloudflare first.

## Risks

| Risk | Mitigation |
|---|---|
| Generalizing breaks behavior a fleet relies on | Packages move with their tests; a fleet already deployed by the engine is moved last, after a sample app runs end to end |
| Rollback after an incompatible migration | Documented expand-then-contract rule; a snapshot before a primary stack's deploy; a failed deploy reports whether its migration completed |
| Root on the host | The on-host command accepts only a typed request, validates every path under the app's directories, and runs nothing from the release before `validate` as the app's user |
| Provider lock-in | Interfaces per provider, with fakes in tests |
