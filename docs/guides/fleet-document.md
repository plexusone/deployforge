# Fleet document

This guide describes every part of the fleet document, `fleet.json`: hosts, apps, stacks, the database server and the credentials section. deployforge refuses unknown keys, so a misspelled field is an error. Run `deployforge schema` to print the JSON Schema the document follows, and `deployforge validate` to check a document. See [Getting started](getting-started.md) for a minimal example, [Hosts](hosts.md) for what the host fields drive, and [Databases](databases.md) and [Credentials](credentials.md) for the sections they use.

## Top level

| Field | Meaning |
|---|---|
| `$schema` | The JSON Schema the file follows, for editors. |
| `formatVersion` | Required. Must be `"1"`. |
| `portRange` | Required. `min` and `max`, within 1024 to 65535. Stack ports come from here. |
| `hosts` | Required. The machines apps run on. At least one. |
| `primaryStack` | The stack named for the app alone. `prod` when empty. |
| `acmeEmail` | The address Caddy registers with for certificates it gets itself, for apps with no edge. |
| `apps` | The deployed apps. |
| `databaseServer` | The PostgreSQL server, when deployforge provisions roles. |
| `credentials` | The credential registry. |

The primary stack decides names. The unit and Unix user of the primary stack are named for the app: `example-app`. Any other stack's unit is `<app>-<stack>`, such as `example-app-staging`. The Unix user is the unit name without hyphens and must be at most 32 characters.

## Hosts

```json
{ "name": "web-1", "address": "203.0.113.10", "sshUser": "ubuntu" }
```

- `name`: lowercase letters, digits and hyphens. Stacks refer to the host by this name.
- `address`: the address for SSH.
- `addressFrom`: read the address from a Pulumi output instead. Give `address` or `addressFrom`, not both.
- `sshUser`: the user to connect as, with sudo. `ubuntu` when empty.

```json
{
  "name": "web-1",
  "addressFrom": { "pulumi": { "dir": "stack", "stack": "prod", "output": "staticIp" } }
}
```

`dir` is the Pulumi project's directory. deployforge runs `pulumi stack output` when it needs the address, so the document holds no IP. See [Hosts](hosts.md).

## Apps

```json
{
  "name": "example-app",
  "repo": "github.com/example-org/example-app",
  "domain": "example.com",
  "edge": "cloudflare",
  "secretPrefix": "/example/{stack}/example-app/",
  "stateDirectory": false,
  "selfcheck": false,
  "logFiles": ["security.jsonl"],
  "labels": { "owner": "example-org" },
  "notes": "Why the app is set up this way.",
  "stacks": {}
}
```

| Field | Meaning |
|---|---|
| `name` | The app's name: its binary, and with the stack, its unit, user and paths. |
| `repo` | The repository that builds the binary, as `<host>/<owner>/<repo>`. |
| `domain` | The DNS zone every host name of the app is in. Each stack host name must be the domain or end in `.<domain>`. |
| `edge` | `cloudflare`, or empty for none. With `cloudflare`, Caddy serves the zone's origin certificate and trusts Cloudflare's addresses to name the client. With none, Caddy gets its own certificates. An app behind `cloudflare` needs `domain`. |
| `secretPrefix` | Where the app's secrets are in the parameter store. Starts and ends with `/` and contains `{stack}`. |
| `stateDirectory` | The app keeps state on local disk, so its unit gets a private writable state directory. |
| `selfcheck` | A deploy runs the app's `selfcheck` subcommand before and after the switch. See [Deploying](deploying.md). |
| `logFiles` | Files the app writes under `/var/log/<unit>/`. The host rotates them daily by rename and keeps a week. Plain file names only. |
| `labels` | String facts deployforge keeps but never acts on, for tools around it. |
| `notes` | Why the app is set up this way. JSON has no comments. |
| `stacks` | The app's deployments by name. At least one. |

## Stacks

```json
{
  "host": "web-1",
  "hosts": ["app.example.com", "auth.example.com"],
  "loopbackHosts": ["auth.example.com"],
  "port": 8101,
  "databases": [
    { "name": "example_auth" },
    { "name": "example", "appRole": true, "appDsn": "EXAMPLE_PG_DSN" }
  ],
  "status": "live",
  "limits": { "memoryMax": "512M", "cpuQuota": "50%", "connectionLimit": 10 }
}
```

| Field | Meaning |
|---|---|
| `host` | The host the stack runs on, by name. |
| `hosts` | Host names the reverse proxy forwards to the stack. At least one. Each name is used by one stack only. |
| `loopbackHosts` | Names among `hosts` that the host resolves to itself, so the app's calls to them stay on the host. |
| `port` | The loopback port. Inside `portRange` and unused on its host. `deployforge next-port <host>` prints a free one. |
| `databases` | The stack's databases. See below. |
| `status` | `planned`, `live` or `elsewhere`. |
| `limits` | Required once the stack is live. |

Status means:

- `planned`: not deployed yet.
- `live`: runs on its host and is deployed by deployforge.
- `elsewhere`: runs outside the fleet until it moves in. [Databases](databases.md) describes how roles for such a stack are staged.

Limits:

- `memoryMax`: systemd `MemoryMax`, such as `512M` or `1G`.
- `cpuQuota`: systemd `CPUQuota`, such as `50%`.
- `connectionLimit`: each database role's `CONNECTION LIMIT`. Match your app's pool size to it.

## Databases

Each database has an owner role `<name>_owner`, which migrates it. With `appRole` set, the app runs as a separate role `<name>_app` with rows only. Otherwise it runs as the owner. Database names are lowercase letters, digits and underscores, and each name is unique across the fleet.

| Field | Meaning |
|---|---|
| `name` | The database's name. |
| `appRole` | The app runs as a separate role. |
| `ownerDsn` | The parameter, under the secret prefix, holding the owner's connection string, such as `migrate/EXAMPLE_PG_ADMIN_DSN`. |
| `appDsn` | The parameter holding the app role's connection string. |
| `appPassword` | A parameter holding the app role's password alone, for a migration that sets it. |

`appDsn` and `appPassword` need `appRole`. A parameter under `migrate/` reaches only the migrate step.

## Database server

```json
{
  "databaseServer": {
    "endpoint": "db.example.com",
    "port": 5432,
    "adminUser": "admin",
    "adminDatabase": "fleet",
    "adminPassword": { "parameter": "/example/admin/password" },
    "via": "web-1",
    "lockdown": ["fleet"],
    "reachable": ["template1"],
    "lightsailName": "example-pg"
  }
}
```

| Field | Meaning |
|---|---|
| `endpoint` | The server's host name. |
| `endpointFrom` | Read the endpoint from a Pulumi output, as for a host address. Give `endpoint` or `endpointFrom`. |
| `port` | `5432` when empty. |
| `adminUser`, `adminDatabase` | The administrator provisioning connects as, and its database. |
| `adminPassword` | Where the password is kept. Give exactly one of `parameter` (a full name starting with `/`) or `pulumiConfig` (`dir`, `stack`, `key`). |
| `via` | The fleet host that runs `psql` against the server. The operator's machine need not reach it. |
| `lockdown` | The server's other databases, which no app's role may reach. |
| `reachable` | Databases the provider owns whose `CONNECT` cannot be revoked, such as `template1`. |
| `lightsailName` | The server's name as a Lightsail database. Without it, no snapshots are taken. |

## Credentials

```json
{
  "credentials": {
    "registry": "credentials/registry.json",
    "events": "credentials/events.json",
    "reviews": "credentials/reviews.json",
    "targets": "credentials/targets.json"
  }
}
```

Only `registry` is required. Every path is relative to the fleet document. `events` and `reviews` default to `events.json` and `reviews.json` beside the registry. `targets` is a credforge targets file. See [Credentials](credentials.md).

## What validate checks

`validate` reports every problem at once. Among them: an unknown format version, a bad port range, a name, unit, host name or database used twice, a stack on an unknown host, a port outside the range or used on its host, a host name outside the app's domain, a secret prefix without `{stack}`, and a live stack without limits.
