# TRD: deployforge

**Initiative:** INIT-DEPLOYFORGE-001
**Status:** proposed

## Architecture

deployforge is library first: each capability is a package with no
command-line code, and the `deployforge` command (Cobra) is a thin layer
over them. A host tool can import the packages, or mount the command tree
under its own command and add its own commands beside it.

The operator's machine does the work that needs credentials: it reads the
fleet document, the parameter store and the CDN's API, and builds the
release. The host does only what must happen there, by running the same
binary as root over SSH with a JSON request on standard input. There is no
agent or daemon on the host.

```
operator machine                         host
────────────────                         ────
deployforge deploy ship app prod
  ├─ release package (clean tag)
  ├─ secrets from parameter store
  ├─ scp: deployforge (linux), release.tgz
  └─ ssh: sudo deployforge on-host  ───▶  lock
                                         unpack releases/<id>
                                         install /etc/<unit>/*
                                         <app> validate, <app> migrate
                                         switch current, restart unit
                                         wait for readiness
                                         restore on failure, prune
                                         audit log, unlock
  ├─ ledger entry
  └─ readiness through public host names
```

## Packages

| Package | Responsibility |
|---|---|
| `fleet` | The fleet document's types, loading, validation and schema; units, users and paths derived from names |
| `schema` | The fleet and zones documents' generated JSON Schemas, embedded |
| `release`, `provenance` | Package a clean, tagged checkout into an archive with its revision and checksum; name upstream commits |
| `deploy/target` | Resolve an app stack to its host, unit, user, paths and ports |
| `deploy/secrets`, `deploy/secrets/awsssm` | Read a stack's prefix into environment, migrate-only variables and key files, and mask them; AWS SSM |
| `deploy/archive` | Verify an archive and extract it safely |
| `deploy/deployer` | The operator side: build the request, dial the stack's host, transfer, run on the host, record |
| `deploy/onhost` | The host side: lock, unpack, install, validate, migrate, switch, health, restore, prune, audit |
| `deploy/ship` | Package a tag or local commit in a temporary worktree with the app's release script |
| `deploy/readiness` | Check every host name's /readyz from outside |
| `deploy/hooks` | Run the operator's hooks around packaging, deploy and rollback |
| `deploy/hostaddr` | Resolve a host's or server's address, and Pulumi configuration values |
| `deploy/systemd`, `deploy/host` | Render units and their sandbox, and the idempotent host-setup script |
| `deploy/caddy`, `deploy/cfranges` | Render a host's Caddyfile; Cloudflare's published proxy ranges |
| `ledger` | Append and read the per-stack JSON Lines ledger |
| `zones`, `zoneharden` | The zones document and its verification; the security baseline through Cloudflare's API |
| `origincert` | Issue, list and revoke origin certificates, made on the host |
| `dbroles`, `dbsnapshot` | Database and role plans with SCRAM verifiers and connection limits; snapshots of a Lightsail server |
| `deploy/preflight` | Before a deploy, the registry's secrets under the stack's prefix checked by name in the parameter store |
| `stackiam` | Each stack's parameter-store IAM policy and operators' group: plan, apply, verify |
| `cli` | The Cobra command tree, mountable by a host tool |

Each CDN or cloud API is behind an interface in the package that uses it,
with Cloudflare and AWS implementations in subpackages, so tests use fakes
and a second provider can be added without touching the callers.

## Configuration

JSON, camelCase keys, unknown keys refused, schemas generated from the Go
types with `invopop/jsonschema` and embedded.

### Fleet document

```json
{
  "formatVersion": "1",
  "hosts": [{ "name": "web-1", "addressFrom": { "pulumi": { "dir": "stack", "stack": "prod", "output": "staticIp" } } }],
  "databaseServer": {
    "endpointFrom": { "pulumi": { "dir": "stack", "stack": "prod", "output": "dbEndpoint" } },
    "adminUser": "admin", "adminDatabase": "fleet",
    "adminPassword": { "pulumiConfig": { "dir": "stack", "stack": "prod", "key": "fleet:dbMasterPassword" } },
    "via": "web-1", "lightsailName": "fleet-prod-pg"
  },
  "portRange": { "min": 8101, "max": 8199 },
  "apps": [{
    "name": "example-app",
    "repo": "github.com/example/example-app",
    "domain": "example.com",
    "edge": "cloudflare",
    "secretPrefix": "/example/{stack}/example-app/",
    "logFiles": ["security.jsonl"],
    "stateDirectory": false,
    "selfcheck": false,
    "stacks": {
      "prod": {
        "host": "web-1",
        "hosts": ["app.example.com", "auth.example.com"],
        "loopbackHosts": ["auth.example.com"],
        "port": 8101,
        "databases": [
          { "name": "example_auth", "ownerDsn": "AUTH_DB_DSN" },
          { "name": "example", "appRole": true, "ownerDsn": "migrate/EXAMPLE_PG_ADMIN_DSN", "appDsn": "EXAMPLE_PG_DSN" }
        ],
        "status": "live",
        "limits": { "memoryMax": "512M", "cpuQuota": "50%", "connectionLimit": 10 }
      }
    }
  }]
}
```

- **Stacks are named, not fixed.** Any number of stacks per app; the unit
  is the app's name for the stack named `prod` (configurable) and
  `<app>-<stack>` otherwise.
- **Databases belong to the stack,** each saying whether the app runs as
  a separate app role (`appRole`) or as the owner; nothing about databases
  is compiled in.
- **Credentials are described, not configured here:** `credentials`
  names a registry file in systemspec-credential's format, with its event
  log, review decisions and an optional credforge targets file, each
  relative to the fleet document. Without a targets file, GitHub is
  reached with `GITHUB_TOKEN` or `GH_TOKEN` and SSM with the default AWS
  configuration, and every app's repository is scanned.
- **Addresses can come from outputs** of the tool that made the hosts,
  read through a provider (Pulumi first), so the document holds no IPs
  (RMI-DEPLOYFORGE-012; until then an address is an IP or host name).

### Per-app files

Each app stack has `stacks/<app>/<stack>/config.yaml`, the app's own
config, installed as is. deployforge never edits it.

### Zones, hardening and workers

`zones.json`, `hardening.json` and `workers.json`, each with its schema.
The hardening baseline is data: rules, rate limits and settings, with
per-zone overrides.

## Deploy Protocol

- **Request:** action, target, release ID (`<UTC timestamp>-<revision>`),
  the config bytes, the secrets bundle, health timeout, releases to keep,
  self-check mode. Sent on standard input; never written to disk on the
  operator's machine; written on the host only as the installed files.
- **Lock:** a lock file under the app's directory, created exclusively,
  holding who and when; `deploy unlock` removes a stale one.
- **Install:** each file under `/etc/<unit>/` is written to a temporary
  name and renamed; the previous set and a `.managed` list are kept for
  restore.
- **Migrate:** runs once per deploy as the app's user with the migrate-only
  variables in a temporary environment file removed afterwards. Migrations
  must be backward compatible with the previous release (expand, then
  contract), because rollback does not reverse them.
- **Switch:** `ln -s` to a temporary name, then `rename` over `current`;
  `systemctl daemon-reload` and restart.
- **Health:** the app's readiness endpoint on its loopback port, once per
  host name in the `Host` header, until the timeout.
- **Restore:** the previous `current`, the previous `/etc` files, restart,
  and readiness again; the failure is reported with the step that failed.
- **Audit:** one JSON line per action in the app's directory on the host.
- **Ledger:** on the operator's machine, after success.

## Hooks

Hooks run on the operator's machine, around what the operator starts:
`before-package` and `after-package` around packaging, `before-deploy`
and `after-deploy` around a release, `before-rollback` and
`after-rollback` around a rollback, and `failed` after any of them fails.
The steps on the host (migrate, switch, health) run inside one request,
so they have no hooks of their own.

A hook is an executable named for it in `.deployforge/hooks/` (or
`--hooks`), run with `DEPLOYFORGE_HOOK`, `_APP`, `_STACK`, `_HOST`,
`_OPERATOR`, `_RELEASE`, `_REVISION`, `_TAG` and, for `failed`, `_ERROR`
in its environment, and never a secret value. A missing hook is skipped.
A failing before-hook stops what it precedes; an after-hook runs once the
change is done and recorded, so its failure is reported without undoing
it; the `failed` hook's own failure is only logged.

## Security

- No secret value in config files, arguments, logs or the ledger.
- The on-host binary runs as root only for the request it is given; the
  app runs as its own user with a sandboxed unit.
- SSH uses the operator's agent or a named key; host keys are checked.
- Origin certificate keys are made on the host and never leave it.
- Every write the CDN or the database server sees has a plan step first,
  and `--confirm` to apply.

## Testing

Unit tests for every package, with fakes for SSH, the parameter store, the
CDN and the database server. The on-host steps run against a temporary
directory tree with a fake `systemctl`. An integration test deploys a
sample app to a disposable VM in CI on demand.

## Compatibility

Go 1.26. Linux hosts with systemd. Caddy 2. PostgreSQL 16 or later for
database roles.
