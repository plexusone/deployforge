# Credentials

This guide covers how a fleet describes its credentials, the `credentials` commands that check, report on, generate, install and record them, the check `deploy ship` runs before packaging, and the `iam` command that gives each stack's parameter-store prefix its own IAM policy. The registry is in [systemspec-credential](https://github.com/plexusone/systemspec-credential) format, and the commands run through [credforge](https://github.com/plexusone/credforge). For the fleet fields, see [Fleet document](fleet-document.md). For the deploy that uses the check, see [Deploying](deploying.md).

No command here prints a secret value. Checks read names, not values.

## The credentials section

The fleet document names the registry and the files beside it:

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

- `registry`: required. The credential registry file.
- `events`: the event log. `events.json` beside the registry when empty.
- `reviews`: review decisions. `reviews.json` beside the registry when empty.
- `targets`: a credforge targets file that says how each store is reached. Without it, GitHub Actions secrets are reached with `GITHUB_TOKEN` or `GH_TOKEN`, and SSM with the default AWS configuration. Either way, every app's repository is scanned.

Paths are relative to the fleet document. The `credentials` commands take `--events` to name another event log; the default is `events.json`.

Print the JSON Schema of the registry or the event log:

```bash
deployforge credentials schema registry
deployforge credentials schema events
```

## Validate

```bash
deployforge credentials validate
```

`validate` fails on a registry or event log that is invalid. It then prints advice about entries that are valid but likely to mislead whoever sets them up. `--strict` fails on advice too.

## Status

```bash
deployforge credentials status --system example-app --env prod
```

`status` lists every credential whose state is not `ok`, `planned` or `retired`, most urgent first. It exits non-zero when a listed credential is in a `--fail-on` state, so scripts and CI can use it as a gate. The default is `expired,overdue`.

Flags: `--all`, `--as-of <date>`, `--env`, `--system`, `--manual`, `--progress <to-create,to-record,to-deploy,deployed,retired>`, `--state`, and `--warn-days` (default 30).

## Record an event

```bash
deployforge credentials record 'example-app/prod/*' deployed --by operator
```

`record <credential-id-or-pattern> <event>` adds an event to the log. The event is one of `created`, `rotated`, `expires`, `deployed`, `verified` or `revoked`. In a pattern, `*` matches any text, slashes included. Flags: `--at` (a date or RFC 3339 time; default now), `--by`, `--note`, and `--expires` (required for `expires`, optional for `created` and `rotated`).

## Check the stores

```bash
deployforge credentials check --fail-on missing,stale,unregistered
```

`check` lists the names in every store the targets file reaches, never their values, and the secrets the scanned repositories' workflows read. It compares them with the registry and the event log. The states, most urgent first:

- `missing`: a listed store does not hold the credential.
- `stale`: a store was set before the current value was made.
- `unregistered`: a workflow reads a secret the registry stores nowhere it can be read.
- `changed`: a store was set after the log's latest event, and nothing was recorded.
- `unrecorded`: a store holds a credential the log has no event for.
- `unlisted`: a store holds a name no credential in use names.
- `unobserved`: no binding reaches the store.
- `present`: as recorded.

The exit status is 1 when a finding is in a `--fail-on` state (default `missing,stale,unregistered`), and 2 on an error. `--record` logs unrecorded stores as created and changed ones as deployed.

Other flags: `--all`, `--format` (`text` or `json`), `--grace` (default 24h), `--include-planned`, and `--only <pattern>`, such as `'example-app/prod/*'`.

## Generate

```bash
deployforge credentials generate --dry-run
deployforge credentials generate --only 'example-app/prod/*'
```

`generate` makes each credential whose `generation.format` is set and whose provisioner is `credforge generate` or none. A format is a random value (`hex32`, `base64url32`) or a private key (`p256-pem`, `ed25519-pem`, `rsa3072-pem`). It writes the value to every primary store and records a `created` event. It never prints a value.

It makes a credential only when none of its primary stores holds a value, and writes a parameter only if the parameter still does not exist, so it never replaces a value. It lists what it will make and asks first. Flags: `--dry-run`, `--yes`, `--by`, `--include-planned`, `--only`.

## Install

```bash
deployforge credentials install example-app/prod/api-token --expires 2027-01-31
```

`install <credential>` sets one credential's value in each store the registry lists for it that the targets file reaches: GitHub Actions secrets and SSM SecureString parameters. It lists the writes and asks before reading the value. The value is read once, hidden, from the terminal, or from standard input with `--stdin` and `--yes`. It is never an argument, never written to disk, and never printed.

Before it writes, it checks the value: a GitHub token must read each repository its entitlements name. Then it records a `created` event (`rotated` when the log has one) with the expiry, and a `verified` event for the checks that passed. A credential whose provisioner stores it is refused; run its command instead.

Flags: `--dry-run`, `--yes`, `--stdin`, `--no-probe` (write without checking first), `--expires` (`YYYY-MM-DD` or RFC 3339), `--by`.

## Reports

```bash
deployforge credentials report -f html -o credentials.html
deployforge credentials report -f plan --environment-order prod,staging -o plan.md
```

`report` writes one of five formats with `-f`:

- `html`: one self-contained page to search and filter. This is the default.
- `markdown`: a reference for documentation, from the registry only.
- `plan`: the management plan: summary, launch checklist, rotation calendar, protections, exposure.
- `ics`: the rotation calendar as an iCalendar file.
- `json`: the rows with their state, for other tools.

Flags: `-o`, `--title`, `--as-of`, `--warn-days`, `--months` (calendar span; default 12), `--remind-days`, `--environment-order`, `--system-order`.

`credentials export [-o file]` writes the registry as JSON.

## Review

```bash
deployforge credentials review
```

`review` serves a local page, on `127.0.0.1:8484` unless `--addr` says otherwise. It lists every credential with its stores, consumers, rotation and events. You record decisions (`approved`, `change`, `remove`) in the reviews file, and events in the event log. The registry is never written, and no value is read or shown. A live check from the page needs a targets file.

## Set up a system

```bash
deployforge credentials setup --system example-app --env prod list
deployforge credentials setup --system example-app --env prod run <step>
deployforge credentials setup --system example-app --env prod verify --all
```

`setup` builds a runbook from the registry for one system's credentials: what is set up at issuers, what tools make, and what a person gets from an issuer and installs, in order. Its subcommands are `list`, `show`, `run`, `verify`, `record`, `validate`, `doc`, `export` and `schema`. Each step is checked with credforge, which reads names and never values. Progress is kept in its own log beside the event log, such as `setup-example-app-prod.json`, unless `--progress` names another.

Common flags: `--system`, `--env`, `--need` (`launch`, `later`, `optional`; default `launch`), `--progress`, `--root`.

`run` refuses a step whose dependencies are not done unless you pass `--force`. It asks before each command unless you pass `--yes`, and runs commands without a shell. Give variables with `--var name=<value>`.

## Page for one stack

```bash
deployforge credentials page example-app prod -o example-app-prod.html
```

`page <app> <stack>` writes an HTML page of the credentials the stack needs, how each is made, where it is stored, whether the log records it, and the setup runbook. It holds no value. `--need` picks the needs to show; the default is `launch,later,optional`.

## Preflight for one stack

```bash
deployforge credentials preflight example-app prod
deployforge credentials preflight example-app prod --record
```

`preflight <app> <stack>` lists the stack's secret prefix and compares it with every active credential the registry stores there, as the check in `deploy ship` does, reading no value. It shows:

- each credential present, with when its store last set it, and whether the event log records it;
- each credential missing. One that is optional, by tag or by `need`, or needed only while another rotates (`need: rotation`), is never missing;
- each name under the prefix no credential lists. A database move's values under `next/` and `previous/` are counted, not listed.

Flags:

- `--record`: record each present credential the log lacks as created, at its store's time.
- `--include-planned`: compare planned credentials too, such as those of a stack not yet deployed.
- `--require-recorded`: exit non-zero while a present credential is unrecorded.

It exits non-zero when a credential is missing.

## The pre-flight in ship

When the fleet document has a `credentials` section, `deploy ship` checks that every secret the registry stores under the stack's prefix is in the parameter store, by name, before it packages anything. A missing secret stops the deploy. Skip the check with `--no-preflight`.

## IAM for each stack

```bash
deployforge iam plan
deployforge iam policy example-app prod --account 123456789012
deployforge iam apply example-app prod --confirm
deployforge iam verify
```

`iam <plan|apply|verify|policy> [<app> <stack>]...` makes, for each named stack or every live one:

- a policy `ssm-<app>-<stack>` that reads and writes the SSM parameters under the stack's secret prefix and lists parameter names;
- a group `<app>-<stack>-operators` with the policy attached.

Group members are reported, never changed. `plan` lists what is missing, `apply` creates it and checks again, and `verify` fails unless everything is in place. Nothing is deleted.

`apply` changes the account's IAM, so it needs `--confirm`: without it, it shows the plan, changes nothing, and says to run again with `--confirm`. `policy` prints one stack's policy document for review, with `--account` naming the account the document is for; it reads no AWS account.

Flags:

- `--path`: the IAM path of the policies and groups. Default `/deployforge/`.
- `--account`: refuse to act unless the AWS profile is this account; with `policy`, the account the document names.
- `--confirm`: with `apply`, make the changes the plan shows.
- `--region`: the region of the parameters. Default `$AWS_REGION`.
