---
description: Serve Stillwater over HTTPS without a reverse proxy by supplying your own TLS certificate, with notes on cert generation, Docker healthcheck behavior, and ACME / HTTP/3 support arriving in v1.1.0.
---

# Direct TLS setup

Stillwater can terminate TLS itself instead of relying on a fronting reverse proxy. Point it at a certificate and key, restart, and HTTPS comes up on the same port plain HTTP previously used.

When direct TLS is the wrong answer:

- You already run a proxy (Caddy, SWAG, Traefik, Nginx) for the rest of your stack. Adding Stillwater to it is usually less work than maintaining a separate cert lifecycle for one app. See [Run Stillwater behind a reverse proxy](reverse-proxy.md).
- You want automatic certificate management. v1.0 only supports bring-your-own (BYO) certificates; ACME-managed certs land in v1.1.

When direct TLS is the right answer:

- A single-app VPS or homelab box where adding a proxy is overhead rather than convenience.
- An on-LAN deployment where you generate one self-signed cert per host and accept the browser warning once.
- Any environment that already has a cert provisioning pipeline (corporate PKI, Tailscale Funnel certs, manual Let's Encrypt via certbot) that drops PEM files in a known location.

## Generate a self-signed cert (local testing)

For local testing, a one-line `openssl` invocation is enough:

```sh
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout /config/tls/privkey.pem \
  -out    /config/tls/fullchain.pem \
  -days 365 \
  -subj "/CN=localhost"
```

Browsers will warn the first time you visit; accept the cert (or use `curl -k`) and Stillwater works normally. Self-signed certs are fine for trusted-LAN access and trivially bad for anything internet-facing -- use a real cert in production.

For a real internet-facing deployment today, generate a cert with `certbot certonly --standalone` (or any other ACME client) and point Stillwater at the resulting `fullchain.pem` and `privkey.pem`. Renewal is your responsibility; restart Stillwater after each renewal so the new cert is loaded.

## Configure Stillwater

Two environment variables turn TLS on:

```sh
SW_TLS_CERT_FILE=/config/tls/fullchain.pem
SW_TLS_KEY_FILE=/config/tls/privkey.pem
```

Both must be readable by the Stillwater process. With those set, Stillwater serves HTTPS on the same port that previously served plain HTTP -- if you previously used `SW_PORT=1973`, the app now answers `https://localhost:1973`.

Optional split-port deployment with HTTPS on its own port:

```sh
SW_TLS_CERT_FILE=/config/tls/fullchain.pem
SW_TLS_KEY_FILE=/config/tls/privkey.pem
SW_TLS_PORT=443
```

The same knobs are available in TOML:

```toml
[server.tls]
cert_file = "/config/tls/fullchain.pem"
key_file  = "/config/tls/privkey.pem"
port      = 0  # 0 reuses [server].port; set to 443 for split-port deploys.
```

## TLS status { #settings-general-tls-status }

Open Settings, scroll to the General tab. The TLS Status card shows one of three states:

- **Inactive** -- plain HTTP, no cert configured.
- **Active (BYO certificate)** -- direct TLS using the cert and key supplied via env vars or config file.
- **Active (ACME, &lt;domain&gt;)** -- ACME-managed cert (arrives in v1.1).

The card also lists the bound port so you can confirm whether you're in collapse mode (HTTPS on the original `SW_PORT`) or split-port mode (HTTPS on `SW_TLS_PORT`). The card is read-only; configure TLS via env vars or the config file.

## HSTS

Once Stillwater detects a TLS connection, it automatically sets a strict `Strict-Transport-Security` header on every response. There is nothing to configure; the same gating used for proxy-terminated HTTPS handles direct TLS too.

## Docker healthcheck

The container's healthcheck targets `localhost` over the configured protocol. When `SW_TLS_CERT_FILE` is set, the entrypoint exports `SW_HEALTH_URL` so the probe uses HTTPS on the right port. The probe runs with `curl -k` to skip cert verification -- this is intentional, because a localhost healthcheck against a self-signed cert would otherwise fail for the wrong reason. You do not need to set `SW_HEALTH_URL` yourself; setting `SW_TLS_CERT_FILE` is enough.

## What's next in v1.1

The TLS surface is built so the following land without further configuration churn:

### ACME (Let's Encrypt / Buypass)

Added in v1.1.0; see issue [#930](https://github.com/sydlexius/stillwater/issues/930). Configure via `SW_ACME_DOMAIN`, `SW_ACME_EMAIL`, and `SW_ACME_CA`.

### ACME (ZeroSSL, IP-SAN)

Added in v1.1.0; see issue [#931](https://github.com/sydlexius/stillwater/issues/931). Lets you obtain certificates for a public IP address rather than a DNS name.

### HTTP/3

Added in v1.1.0; see issue [#932](https://github.com/sydlexius/stillwater/issues/932). Toggle via `SW_HTTP3_ENABLED=true`. Requires direct TLS to be configured. Note that HTTP/3 runs over UDP, so any firewall or NAT in front of Stillwater must forward the configured port for both TCP and UDP.

### HTTP-to-HTTPS redirect listener

Added in v1.1.0. Setting `SW_HTTP_REDIRECT_PORT=80` makes Stillwater bind a second plain-HTTP listener that 301-redirects every request to the HTTPS port. See the [HTTP-to-HTTPS redirect](http-redirect.md) how-to for setup details and reverse-proxy interactions.
