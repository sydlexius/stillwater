---
description: Configure Stillwater to obtain and renew TLS certificates automatically from Let's Encrypt or Buypass via ACME (HTTP-01), including DNS prerequisites, port-80 reachability, cache-directory persistence, staging-versus-production guidance, and troubleshooting.
---

# ACME (Let's Encrypt / Buypass)

Stillwater can fetch and renew TLS certificates automatically via ACME, the same protocol Caddy and certbot use. Set one environment variable and a fresh certificate appears on first start; renewals run quietly in the background.

ACME is the right answer when:

- You have a public DNS name pointing at this server.
- Port 80 on that public name reaches Stillwater (no firewall block, NAT forwards in place).
- You do not already terminate TLS at a fronting reverse proxy.

ACME is the wrong answer when:

- You want HTTPS on a private LAN host with no public DNS or open inbound ports. Use a self-signed certificate via [Direct TLS setup](direct-tls-setup.md) instead.
- You want a certificate for an IP address rather than a DNS name. That requires ZeroSSL with EAB credentials and is tracked separately ([#931](https://github.com/sydlexius/stillwater/issues/931)).
- You already run Caddy, Traefik, or similar in front of Stillwater. Let it handle TLS termination; do not double up.

## Prerequisites { #acme-overview }

Before you start, confirm the following:

- A DNS A or AAAA record for the hostname you plan to use resolves to this server's public IP address.
- Inbound TCP port 80 reaches the Stillwater process. ACME's HTTP-01 challenge does not negotiate ports; the certificate authority always fetches `http://your.domain/.well-known/acme-challenge/...` on port 80. Forward that port through any router or firewall sitting in front of Stillwater.
- Inbound TCP port 443 (or whatever you bind for HTTPS) reaches the Stillwater process so clients can actually connect once the certificate is issued.
- The Stillwater process can write to its cache directory (see below); ACME account keys and issued certificates are persisted there.

## Configure Stillwater

The minimum to enable ACME is one environment variable:

```sh
SW_ACME_DOMAIN=stillwater.example.com
```

Recommended additions:

```sh
SW_ACME_EMAIL=admin@example.com
SW_TLS_PORT=443
SW_HTTP_REDIRECT_PORT=80
```

`SW_ACME_EMAIL` lets the certificate authority send expiry notifications and recover your account. `SW_TLS_PORT=443` and `SW_HTTP_REDIRECT_PORT=80` are typical for an internet-facing deployment; without `SW_TLS_PORT`, HTTPS reuses `SW_PORT` (collapse mode) which is fine for split-port-style deployments behind a load balancer.

The same knobs are available in TOML:

```toml
[server.tls]
port = 443

[server.http_redirect]
port = 80

[acme]
domain = "stillwater.example.com"
email  = "admin@example.com"
```

`SW_ACME_DOMAIN` and `SW_TLS_CERT_FILE` / `SW_TLS_KEY_FILE` are mutually exclusive. Set one source of TLS certificates, not both; Stillwater rejects the combination at startup.

## Cache directory { #acme-cache-dir }

ACME issuers rate-limit certificate orders. Let's Encrypt allows roughly 50 orders per registered domain per week and only 5 duplicate orders per week. Burning through that quota is easy if you restart the binary repeatedly without a persistent cache, because every restart forces a fresh order.

Stillwater caches issued certificates and the ACME account key in:

```
<directory of SW_DB_PATH>/acme-cache
```

So a deployment with `SW_DB_PATH=/config/stillwater.db` caches certificates at `/config/acme-cache`. A bare-metal deployment with `SW_DB_PATH=/var/lib/stillwater/db.sqlite` caches at `/var/lib/stillwater/acme-cache`. Override with `SW_ACME_CACHE_DIR=/path/to/cache` if you need to put it elsewhere.

The cache directory is created with `0700` permissions on first start. Account keys live there; do not group-share or world-read it.

If you bind-mount your config directory in Docker (the standard `-v /host/config:/config` pattern), the ACME cache survives container recreation automatically. If you do not, add a bind mount for the cache directory before relying on ACME in production.

## Staging vs production { #acme-staging-vs-prod }

Use the Let's Encrypt staging directory while you are first setting things up. Staging issues real-looking certificates that browsers will reject (the staging CA is not in the public trust store) but does not consume production rate-limit quota.

```sh
SW_ACME_DOMAIN=stillwater.example.com
SW_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory
```

Once the staging cert is issued and the cache directory has the expected files, remove `SW_ACME_CA` (the unset value defaults to Let's Encrypt production), wipe the cache directory, and restart. The next start will order a real, browser-trusted certificate.

For Buypass (the European Let's Encrypt alternative), set:

```sh
SW_ACME_CA=https://api.buypass.com/acme/directory
```

Buypass certificates are valid for 180 days versus Let's Encrypt's 90; everything else works the same.

## Port 80 reachability { #acme-port-80-reachability }

Stillwater binds a small plain-HTTP listener on port 80 (or `SW_HTTP_REDIRECT_PORT` if you set it) whenever ACME is on. That listener does two things:

1. Serves `/.well-known/acme-challenge/...` so the certificate authority can validate the domain.
2. 301-redirects every other request to HTTPS, so an operator who hits `http://your.domain/` ends up at `https://your.domain/`.

If port 80 is held by another process (an old Apache or a system-managed proxy), Stillwater fails to start with a bind error. Free port 80 first.

If you cannot free port 80 publicly but can NAT-forward an arbitrary public port to a different internal port, set `SW_HTTP_REDIRECT_PORT` to whatever Stillwater binds internally. The certificate authority still fetches the challenge from the public port 80; your NAT does the translation.

## Verify it worked

After starting Stillwater with `SW_ACME_DOMAIN` set:

1. Watch the logs. You should see `ACME (autocert) configured` followed by no errors.
2. Open `https://your.domain/` in a browser. The certificate should be issued by Let's Encrypt (or your configured CA), not self-signed, and the address bar should show no warning.
3. Open Settings, scroll to the General tab. The TLS Status card should show "Active (ACME, your.domain)" and HTTPS on the bound port.

## Troubleshooting

- **Browser shows "connection refused" on port 443** -- HTTPS listener never bound. Check the logs for a `bind: address already in use` error or invalid `SW_TLS_PORT`.
- **Browser shows a self-signed warning** -- `SW_ACME_DOMAIN` is unset (and direct TLS is configured), or the cache directory contains an old self-signed cert from a previous run. Stop the binary, clear `<cache_dir>/<domain>` files, restart.
- **`urn:ietf:params:acme:error:rateLimited`** -- you ran out of staging or production quota. Wait one week, or switch to staging while debugging.
- **`urn:ietf:params:acme:error:connection` or `:dns`** -- the CA could not reach `http://your.domain/.well-known/acme-challenge/...`. Check that the DNS record resolves to this server, that port 80 is open externally, and that no fronting proxy is intercepting `/.well-known/acme-challenge/`.
- **TLS works but renewals do not** -- check the cache directory is writable by the Stillwater process. autocert renews 30 days before expiry; the first renewal cycle will tell you if persistence is broken.
- **Settings TLS Status card shows "Inactive"** -- restart the binary. Stillwater reads ACME configuration once at startup; toggling `SW_ACME_DOMAIN` without a restart does not flip the listener.

## What's not supported

- DNS-01 challenges (no port-80 dependency, supports wildcards).
- Custom HTTP-01 challenge ports below 1024 on hosts where Stillwater does not have `CAP_NET_BIND_SERVICE`.
- Automatic migration from BYO certificates to ACME -- you must clear `SW_TLS_CERT_FILE`/`SW_TLS_KEY_FILE` before setting `SW_ACME_DOMAIN`.
- Multiple domains on one Stillwater instance (`SW_ACME_DOMAIN` accepts a single hostname).

These can become follow-up issues once ACME has field experience.
