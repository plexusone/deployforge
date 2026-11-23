# Hosts

This guide covers preparing a host for a stack and its reverse proxy: `host setup`, `unit`, `caddyfile`, `caddy install`, `next-port`, and addresses read from Pulumi outputs. The fields these commands read are in [Fleet document](fleet-document.md). Once a host is ready, see [Deploying](deploying.md). For certificates behind Cloudflare, see [Edge](edge.md).

## Print a unit

```bash
deployforge unit example-app prod
```

`unit` prints the stack's systemd unit, with its hardening sandbox and the limits from the fleet document. Use it to review what `host setup` will install.

## Set up a stack's place on its host

```bash
deployforge host setup example-app prod            # print the script
deployforge host setup example-app prod --apply    # run it on the host
```

The script makes the stack's Unix user, its `/opt` and `/etc` directories, its systemd unit, the rotation of its log files, and the `/etc/hosts` lines for its loopback host names. It is idempotent, so you can run it again. It never starts the service, because a stack has no release to run until its first deploy.

`--apply` runs the script on the stack's host over SSH, with sudo. `--ssh-key` names the host's SSH private key, used through the agent `SSH_AUTH_SOCK` names.

## Find a free port

```bash
deployforge next-port web-1
```

`next-port` prints the lowest port in `portRange` that no stack on the host uses. It fails when the range is full.

## Caddyfile

```bash
deployforge caddyfile web-1
```

`caddyfile` prints the host's Caddyfile, with one block per live stack on it. To include a stack that is not live yet, add `--with`:

```bash
deployforge caddyfile web-1 --with example-app/staging
```

Name each stack as `app/stack`, and repeat `--with` for each.

With `edge` set to `cloudflare`, the blocks use the zone's origin certificate and trust Cloudflare's addresses to name the client. With no edge, Caddy gets its own certificates, registered with `acmeEmail`.

## Install the Caddyfile

```bash
deployforge caddy install web-1
```

`caddy install` renders the Caddyfile, uploads it, and has Caddy validate it on the host before anything is replaced. It keeps the running one as `/etc/caddy/Caddyfile.previous`, installs the new one and reloads Caddy. It never restarts Caddy, so the other apps keep serving.

To go back, copy the previous file over the Caddyfile and reload Caddy.

Flags: `--with` as above, and `--ssh-key`.

## Addresses from Pulumi outputs

A host can take its address from a Pulumi stack output, so the fleet document holds no IP:

```json
{
  "name": "web-1",
  "addressFrom": { "pulumi": { "dir": "stack", "stack": "prod", "output": "staticIp" } }
}
```

Every command that reaches the host resolves the address when it needs it, by running `pulumi stack output` in `dir`. `pulumi` must be installed and logged in. The same form, `endpointFrom`, works for the database server's endpoint. The database administrator's password can come from a Pulumi secret with `adminPassword.pulumiConfig`. See [Databases](databases.md).
