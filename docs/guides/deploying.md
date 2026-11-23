# Deploying

This guide covers deploying a release to a stack, rolling it back, clearing a stale lock, reading status, what runs on the host, the deploy ledger, hooks, and packaging a release archive. For a first deploy, see [Getting started](getting-started.md). To move a stack from another tool, see [Taking over a stack](taking-over.md). For the fields these commands read, see [Fleet document](fleet-document.md).

## How a deploy runs

The operator's machine does the work that needs credentials: it reads the fleet document and the parameter store, and builds the release. It copies a linux/amd64 build of deployforge and the archive to the host, then runs deployforge there as root over SSH. There is no agent on the host.

Hosts are reached as `<sshUser>@<address>` with your system's `ssh` and `scp`, so your SSH configuration, agent and known hosts apply.

Secrets come from the stack's secret prefix:

- `<prefix><VAR>` becomes `/etc/<unit>/env`.
- `<prefix>migrate/<VAR>` reaches the migrate step only.
- `<prefix>files/<name>` becomes `/etc/<unit>/<name>`.

They travel to the host on ssh's standard input, never on a command line or your disk.

These flags and variables apply to every `deploy` command:

| Flag | Meaning |
|---|---|
| `--ssh-key` | The hosts' SSH private key, used through the agent `SSH_AUTH_SOCK` names. Also `DEPLOYFORGE_SSH_KEY`. |
| `--operator` | Who is deploying, for the lock, the audit log and the ledger. Also `DEPLOYFORGE_OPERATOR`; defaults to `$USER`. |
| `--host-binary` | deployforge's linux/amd64 build, which runs on the host. Default `bin/deployforge-linux-amd64`. Also `DEPLOYFORGE_HOST_BINARY`. |
| `--health-timeout` | Seconds to wait for every host name to answer `/readyz` after a restart. Default 60. |
| `--keep` | Releases kept on the host, at least 2. Default 5. |
| `--ledger-dir` | Directory of the ledger files. Default `deployments`. |
| `--hooks` | Directory of hook scripts. Default `.deployforge/hooks`. |

`AWS_PROFILE` and `AWS_REGION` select the AWS profile and region when secrets are in SSM.

## deploy ship

`ship` is the routine deploy. It packages the app with its release script in a temporary git worktree, so your checkout is left alone. It deploys the archive as `deploy release` does, with the stack's config, and checks `https://<host>/readyz` for each of the stack's host names from outside.

```bash
deployforge deploy ship example-app prod --tag v1.2.0
```

The config defaults to `stacks/<app>/<stack>/config.yaml`; set `--config` for another. The release script defaults to `ops/host/release.sh`; set `--script` for another. The app's checkout defaults to `$GOPATH/src/<repo>`; set `--repo` for another.

With `--tag`, `ship` fetches the tag from origin and packages it with `GOWORK` off. `--no-fetch` skips the fetch.

The script receives the deployforge binary in `DEPLOYFORGE` and the fleet document in `DEPLOYFORGE_FLEET`. It builds the app for linux/amd64 and runs `$DEPLOYFORGE release package`. See [Release package](#release-package).

### Local builds

`--local` packages a local commit, untagged, instead of a tag. `--ref` names the app's branch or commit; the default is `main`.

```bash
deployforge deploy ship example-app staging --local \
  --with github.com/example-org/example-lib \
  --with-web github.com/example-org/example-web@my-branch
```

- `--with <host/org/repo>[@ref]` builds an upstream Go module from its local checkout (`$GOPATH/src/<repo>`, `main` unless `@ref` is given) through a generated `go.work`.
- `--with-web <host/org/repo>[@ref]` builds and packs the repository's npm packages with pnpm, and installs them in place of the front end's own. This happens in the temporary worktree only.

The archive and the ledger name every upstream commit used. Every commit a local build uses must be on a remote branch. `--allow-unpushed <repo>` excuses a repository from that rule; repeat it for each.

### Snapshot before a primary stack

Before it deploys an app's primary stack that has databases, `ship` takes a snapshot of the database server and waits until it is available. This applies when the server is a Lightsail database, which means the fleet document sets `databaseServer.lightsailName`. A migrate that goes wrong can then be undone by restoring the snapshot. A snapshot that fails stops the deploy. `--no-db-snapshot` skips it. See [Databases](databases.md).

### Credential pre-flight

When the fleet document names a credential registry, `ship` first checks that every secret the registry stores under the stack's prefix is in the parameter store, by name. It stops before packaging if one is missing. `--no-preflight` skips it. See [Credentials](credentials.md).

### Other ship flags

| Flag | Meaning |
|---|---|
| `--selfcheck` | `enforce`, `report` or `off`: run the app's self-check, if the fleet document turns it on. Default `report`. |
| `--skip-public-check` | Do not check the host names from outside afterwards. |

## deploy release

`release` takes an archive you already have. It checks the archive, ships it to the stack and records it in the ledger.

```bash
deployforge deploy release example-app prod \
  --archive dist/example-app-0123abc.tgz \
  --config stacks/example-app/prod/config.yaml \
  --tag v1.2.0
```

`--sha256` gives the archive's checksum; by default deployforge reads the first field of `<archive>.sha256`. `--selfcheck` takes the same values as for `ship`.

## deploy rollback

```bash
deployforge deploy rollback example-app prod
```

Rollback switches the stack back to the release before the current one and records it in the ledger. The host must keep a second release. Rollback does not reverse migrations, so write migrations that the previous release can run with.

## deploy unlock

```bash
deployforge deploy unlock example-app prod
```

A deploy that is cut off can leave its lock behind. `unlock` removes it and changes nothing else. Check `deploy status` first to see who holds the lock.

## deploy status

```bash
deployforge deploy status example-app prod
```

`status` reads the host and changes nothing. It shows the release `current` points at, the release directories kept, who holds the deploy lock, and whether the host has deployforge's audit log. It compares the live release with the ledger's.

## deploy unpushed

```bash
deployforge deploy unpushed --fetch
```

`unpushed` reads each stack's live release from the ledger and checks the app's commit and every upstream input of a local build against the remote branches of its checkout under `$GOPATH/src`. It prints what is not pushed or cannot be checked, and exits non-zero if anything is. `--fetch` fetches each checkout's origin first.

## On-host steps

For one release, the host runs these steps in order:

1. Lock. The lock file records who and when. It is created exclusively, so a second deploy fails.
2. Unpack the archive as `/opt/<unit>/releases/<id>`. The id is `<UTC timestamp>-<revision>`.
3. Install the config, the environment file and the key files under `/etc/<unit>/`. Each is written to a temporary name and renamed. The previous set is kept for restore.
4. Validate: run the new binary's `validate` as the stack's user.
5. Migrate: run the new binary's `migrate` as the stack's user, with the migrate-only variables in a temporary environment file removed afterwards.
6. Switch: point `/opt/<unit>/current` at the new release and restart the unit.
7. Readiness: wait for `/readyz` on the loopback port, once for each host name in the `Host` header, until `--health-timeout`.
8. Restore: if any step fails, put back the previous release and files, restart, and check readiness again. The error names the step that failed. Completed migrations stay applied.
9. Prune: keep the newest `--keep` releases, and always the live one and the one it replaced; remove the rest.
10. Audit: write one JSON line for the action in the app's directory on the host, then unlock.

When the fleet document turns on `selfcheck`, the app's `selfcheck` subcommand runs before and after the switch, as `--selfcheck` says.

## The ledger

The ledger is a JSON Lines file for each stack in `--dir` (default `deployments`), kept in your repository. A successful deploy or rollback appends an entry. It never holds a secret value. Commit the file after each deploy.

```bash
deployforge ledger show example-app prod -n 5
deployforge ledger current example-app prod
```

- `ledger show <app> <stack>` prints the most recent entries. `-n` sets how many; `0` prints all; the default is 10.
- `ledger current <app> <stack>` prints the release the stack runs, and fails if it was never deployed.
- `ledger record` appends an entry by hand, after checking it against the history. Its flags are `--app`, `--stack`, `--action` (`deploy` or `rollback`, default `deploy`), `--release`, `--previous`, `--revision`, `--tag`, `--sha256` (required for a deploy), `--time`, `--operator`, `--input` and `--selfcheck`.
- `ledger import <app> <stack> --from <file>` brings in a ledger written by another tool. See [Taking over a stack](taking-over.md).

Every ledger command takes `--dir` to name the ledger directory.

## Hooks

Hooks are executables in the hooks directory (`.deployforge/hooks` unless `--hooks` says otherwise). They run on your machine, named for the moment they run:

| Hook | Runs |
|---|---|
| `before-package` | Before packaging. |
| `after-package` | After packaging. |
| `before-deploy` | Before a release is deployed. |
| `after-deploy` | After a release is deployed and recorded. |
| `before-rollback` | Before a rollback. |
| `after-rollback` | After a rollback is done and recorded. |
| `failed` | After any of the above fails. |

A hook that does not exist is skipped. A hook gets these environment variables, and never a secret value:

- `DEPLOYFORGE_HOOK`, the hook's name
- `DEPLOYFORGE_APP`
- `DEPLOYFORGE_STACK`
- `DEPLOYFORGE_HOST`
- `DEPLOYFORGE_OPERATOR`
- `DEPLOYFORGE_RELEASE`
- `DEPLOYFORGE_REVISION`
- `DEPLOYFORGE_TAG`
- `DEPLOYFORGE_ERROR`, for `failed`

Failure rules:

- A failing `before-` hook stops what it precedes.
- An `after-` hook runs once the change is done and recorded. Its failure is reported but does not undo the change.
- A failure of the `failed` hook is only logged.

The host steps run inside one request, so they have no hooks of their own.

## Release package

`release package` writes a reproducible archive and its `.sha256` file. Your app's release script normally calls it:

```bash
deployforge release package example-app \
  --repo . --from build/ --out dist
```

It checks that:

- the app's checkout is clean, is the repository the fleet document names, and has no `replace` directives;
- the checkout is at a release tag;
- the `--from` directory holds a static linux/amd64 binary named for the app, plus its web files.

Flags:

- `--repo`: the app's checkout.
- `--from`: the directory the app's build wrote.
- `--out`: where to write the archive and checksum. Default `dist`.
- `--allow-untagged`: package a revision with no release tag, for a trial or a local build.
- `--input <repo@commit>`: an upstream commit the build took from a local commit. Repeat for each.
