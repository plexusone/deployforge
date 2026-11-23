# Taking over a stack

This guide moves a stack that another tool deployed into deployforge, without a redeploy. It works when the other tool used the same layout on the host: `/opt/<unit>/releases`, a `current` link and `/etc/<unit>/`. You bring in its ledger, check that the host and the ledger agree, and deploy. For the commands used afterwards, see [Deploying](deploying.md). For the fleet entry the stack needs, see [Fleet document](fleet-document.md).

## 1. Describe the stack

Add the app and stack to the fleet document, with the same host, port and host names the running stack uses, and run `deployforge validate`.

## 2. Import the ledger

The old tool must have kept a JSON Lines ledger of the same entries. Import it:

```bash
deployforge ledger import example-app prod \
  --from old/example-app.jsonl --app-field site
```

`--app-field` names the field that holds the app when it is not called `app`. The default is `app`. `import` renames that field, checks every entry as `ledger record` checks it, in order, and writes nothing unless all pass. The stack's ledger must be empty, or be the file imported, which is replaced. The ledger lands in `--dir`, default `deployments`.

## 3. Compare the host with the ledger

```bash
deployforge deploy status example-app prod
```

`status` reads the host and changes nothing. It compares the live release with the ledger's. They must agree. A rollback also needs a second release kept on the host.

If they do not agree, fix the ledger or the host before you go on.

## 4. Deploy

```bash
deployforge deploy ship example-app prod --tag v1.2.3
```

The first deploy takes the deploy lock and starts deployforge's audit log on the host.

## The lock caveat

The other tool takes no deploy lock. deployforge's lock protects only deploys made with deployforge. Once a stack has moved, deploy it only with deployforge, and stop using the old tool for that stack.
