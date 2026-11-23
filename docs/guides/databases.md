# Databases

This guide covers the PostgreSQL side of a fleet: provisioning each stack's databases and roles, taking a snapshot of the database server, and moving a stack's databases to another server. It assumes the `databaseServer` section and each stack's `databases` in the [Fleet document](fleet-document.md). `deploy ship` takes a snapshot for you before a primary stack; see [Deploying](deploying.md). Connection strings are parameters in the stack's secret prefix; see [Credentials](credentials.md).

All `db` commands take `<app> <stack>` and read the fleet document. `psql` runs on the `via` host, with the administrator's password on its standard input, so your machine need not reach the server.

## Roles

```bash
deployforge db roles plan example-app prod
deployforge db roles apply example-app prod --confirm
deployforge db roles verify example-app prod
```

`roles` provisions each database of a stack with:

- an owner role, `<db>_owner`, which migrates the database;
- an app role, `<db>_app`, when the database sets `appRole`, with rows only;
- the stack's `connectionLimit` and statement and idle-in-transaction timeouts, on the role the app runs as;
- `CONNECT` for the stack's own roles only.

Each role's password is kept in the stack's parameters. deployforge reuses a password that is there, makes and stores one that is not, and sends it to the server only as a SCRAM verifier.

The commands:

- `plan` shows the parameters it would create or reuse, and the SQL, without passwords. It changes nothing.
- `apply --confirm` creates the missing parameters, provisions the databases and roles, then verifies. It needs `--confirm`.
- `verify` checks the server against the plan and exits non-zero on any difference. It reads no app's secret.

All three take `--ssh-key` for the via host.

### Staging for a stack that runs elsewhere

A stack with status `elsewhere` keeps its live parameters until it moves. Its new values wait under `<prefix>next/`, which a deploy never reads. The move sequence below uses them.

## Snapshots

`deploy ship` takes a snapshot before the primary stack of an app with databases. You can take one by hand before a deploy whose migrate changes a schema. This applies when `databaseServer.lightsailName` is set.

```bash
deployforge db snapshot plan example-app prod --label v1.2.0
deployforge db snapshot apply example-app prod --label v1.2.0 --confirm
deployforge db snapshot list
deployforge db snapshot verify <snapshot>
```

- `plan` shows the server, its state, the snapshot `apply` would take and the recent ones.
- `apply --confirm` takes a manual snapshot and waits until it is available. `--timeout` sets how long to wait; the default is 30 minutes.
- `list` lists the server's manual snapshots, newest first.
- `verify <snapshot>` exits non-zero unless the snapshot is available.

A server holds every app's databases, so the snapshot does too. The app, the stack and `--label` name it and are kept as tags. deployforge deletes nothing: remove old snapshots in the Lightsail console, since each is billed for its storage.

## Copy a stack to another server

`db copy` reads each database's owner connection under `--from` and `--to`, each one of `live`, `next` or `previous` under the stack's secret prefix. It runs `pg_dump | psql` on the via host, in one transaction that first drops what the target's owner has. No owners or grants come from the source, so the target ends with exactly the source's objects, all owned by its owner. The connections stay in memory and reach the host on ssh's standard input.

```bash
deployforge db copy run example-app prod --from live --to next --confirm
deployforge db copy verify example-app prod --from live --to next
deployforge db copy promote example-app prod --confirm
```

- `run --confirm` copies each database from `--from` to `--to`, replacing what the target has, then compares row counts.
- `verify` compares each database's tables and exact row counts, and exits non-zero on any difference.
- `promote --confirm` checks that every staged parameter exists, keeps each live one under `previous/` (unless one is there already), and replaces it with its staged value. Deploys then use the new server.

Flags on all three: `--from` (default `live`), `--to` (default `next`), `--skip <database>` to leave a database for the app's migrate to build (repeat for each), and `--ssh-key`.

### Moving a stack

1. Run `db roles apply --confirm`. It makes the new server's roles and keeps their parameters under `next/`.
2. Run `db copy run --from live --to next --confirm`.
3. Run `db copy promote --confirm`.
4. Set the stack's status to `live` in the fleet document, and deploy.
5. For writes made on the old server meanwhile, run `db copy run --from previous --to live --confirm`.

```bash
deployforge db roles apply example-app prod --confirm
deployforge db copy run example-app prod --from live --to next --confirm
deployforge db copy promote example-app prod --confirm
deployforge deploy ship example-app prod --tag v1.2.0
deployforge db copy run example-app prod --from previous --to live --confirm
```
