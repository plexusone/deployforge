# Edge

This guide covers Cloudflare as the edge in front of your apps: checking DNS zones against their name servers, hardening zones through Cloudflare's API, checking the minimum TLS version, and issuing origin certificates. Set `edge` to `cloudflare` on an app to use it; see [Fleet document](fleet-document.md). The host side, including the Caddyfile that serves the certificate, is in [Hosts](hosts.md).

Commands that call Cloudflare read the API token from `CLOUDFLARE_API_TOKEN`. Zone commands read the zones document, `zones.json` by default. Use `--zones <path>` for another file.

## The zones document

`zones.json` lists every DNS zone the fleet owns. Print its schema with `deployforge zones schema`.

```json
{
  "formatVersion": "1",
  "zones": [
    {
      "name": "example.com",
      "registrar": "example-registrar",
      "dns": "cloudflare",
      "records": [
        {
          "name": "app",
          "type": "A",
          "values": ["203.0.113.10"],
          "proxied": true,
          "purpose": "app",
          "source": "example-app/prod",
          "host": "web-1",
          "status": "present",
          "check": { "path": "/readyz", "status": 200 }
        }
      ],
      "hardening": {}
    }
  ]
}
```

Zone fields: `name`, `registrar`, `dns` (`cloudflare` or `registrar`), `records`, `hardening` and `notes`.

Record fields:

- `name`: the label under the zone, or `@` for the zone itself.
- `type`: `A`, `AAAA`, `CNAME`, `MX`, `TXT` or `WORKER`.
- `values`: the values. For a proxied record, the origin.
- `proxied`: whether Cloudflare proxies the record.
- `purpose`: `static-site`, `app`, `redirect`, `mail`, `verification` or `parked`.
- `source`, `host`, `notes`: where it is served from, what serves it, and anything else to know.
- `status`: `present`, or `planned`. `verify` skips planned records.
- `check`: an HTTPS request to the host that must answer as given: `path`, `status` and an optional `location` prefix.

## Check zones

```bash
deployforge zones validate
deployforge zones verify
deployforge zones get https://app.example.com/readyz
deployforge zones page -o zones.md
```

- `zones validate` checks the document, and that every live host name of the fleet has a record.
- `zones verify [zone...]` asks each zone's name servers for every listed record and makes the records' HTTPS checks.
- `zones get <https-url>` resolves the host at its zone's own name servers, as `verify` does, so a record added moments ago is reached even while a resolver caches its absence. It exits non-zero unless the answer has `--status` (default 200), and a `Location` starting with `--location` when you set it.
- `zones page` writes a Markdown page of the zones document, to `-o` or standard output.
- `zones schema` writes the zones document's JSON Schema, to `-o` or standard output.

`verify` has two flags for staged changes. `--records-only` checks the records but not their HTTPS checks, as before a planned change while a host is down. `--fail-planned` fails a planned record instead of skipping it, as after a step that adds records.

## Harden zones

`zones harden` sets each zone's TLS settings and writes its custom rules, rate limit, Free Managed Ruleset, cache rule and www redirect. The baseline is the same for every zone. Each zone's `hardening` section holds only what differs. The rules carry refs beginning with `fleet-`; other rules in a phase are kept.

```bash
deployforge zones harden plan
deployforge zones harden apply --confirm
deployforge zones harden verify
```

- `plan [zone...]` says what differs from the baseline and what `apply` would do. It changes nothing.
- `apply [zone...] --confirm` changes what differs, then verifies. It needs `--confirm`.
- `verify [zone...]` checks every zone matches and exits non-zero if not.

Name zones to act on them, or leave them out for every zone with hardening. The token needs Zone Read, Zone Settings Edit, Zone WAF Edit, Cache Rules Edit, Single Redirect Edit and Bot Management Edit. Zone Read is enough for `plan` and `verify`.

### The hardening section

```json
{
  "hardening": {
    "allowCountries": ["US", "CA"],
    "countryHosts": ["app", "auth"],
    "botFightModeOff": true,
    "wwwRedirect": true,
    "rateLimit": { "path": "/login-token", "requests": 5, "whenRecord": "auth" },
    "noCachePrefixes": ["/api/"]
  }
}
```

- `allowCountries` and `countryHosts`: the only countries allowed (ISO 3166-1 alpha-2) on the listed labels under the zone.
- `botFightModeOff`: turn Bot Fight Mode off.
- `wwwRedirect`: redirect `www` to the apex with a 301, keeping the path and query.
- `rateLimit`: limit one exact path per client address. `requests` is how many one address may make in 10 seconds; the default is 5. `whenRecord` names a record label that must exist before the limit applies.
- `noCachePrefixes`: path prefixes never cached, besides `/` and `.html` documents.

## Check the minimum TLS version

```bash
deployforge zones tls
deployforge zones tls app.example.com
```

`zones tls [<host>...]` checks from outside that each host refuses TLS 1.1 and accepts TLS 1.2. With no host it checks every live stack's host names.

## Origin certificates

An app behind Cloudflare serves a Cloudflare Origin CA certificate for its zone and `*.zone`. The key is made on the host and never leaves it.

```bash
deployforge origin-cert install example.com web-1
deployforge caddy install web-1
```

- `origin-cert install <zone> <host>` makes a key and CSR on the host with openssl, has Cloudflare sign it, and installs the certificate and key as `/etc/caddy/certs/<zone>.pem` and `.key`, owned by `root:caddy`, mode 0640. Reload Caddy to serve it. `--ssh-key` names the host's key.
- `origin-cert issue <zone>` reads a PEM CSR on standard input and writes the signed certificate to `-o` or standard output. Its ID and expiry go to standard error. `--days` sets the validity: 7, 30, 90, 365, 730, 1095 or 5475 (the default, fifteen years).
- `origin-cert list <zone>` lists the zone's certificates: ID, expiry and host names.
- `origin-cert revoke <id> --confirm` revokes a certificate. Cloudflare then refuses an origin that still serves it, so revoke a certificate you replaced only once nothing serves it.

An example of `issue` with a CSR you made elsewhere:

```bash
deployforge origin-cert issue example.com --days 365 -o example.com.pem < example.com.csr
```

## Cloudflare's ranges

Caddy and the host's firewall trust Cloudflare's proxy ranges, which deployforge carries in `deploy/cfranges/ips-v4.txt` and `ips-v6.txt`. Cloudflare changes them rarely.

```bash
deployforge cloudflare-ips --check
deployforge cloudflare-ips --dir deploy/cfranges
```

- `cloudflare-ips --check` compares the ranges this build carries with the lists Cloudflare publishes, and fails when Cloudflare has added or removed one. Run it in a scheduled job.
- `cloudflare-ips` writes the published lists to `--dir` (default `deploy/cfranges`), for review and a release of deployforge.
