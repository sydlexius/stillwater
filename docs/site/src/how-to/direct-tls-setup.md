---
description: Serve Stillwater over HTTPS without a reverse proxy by supplying your own TLS certificate or letting Stillwater obtain one via ACME, with notes on cert generation, Docker healthcheck behavior, and HTTP/3 support.
---

# Direct TLS setup

Stillwater can terminate TLS itself instead of relying on a fronting reverse proxy. Point it at a certificate and key, restart, and HTTPS comes up on the same port plain HTTP previously used.

When direct TLS is the wrong answer:

- You already run a proxy (Caddy, SWAG, Traefik, Nginx) for the rest of your stack. Adding Stillwater to it is usually less work than maintaining a separate cert lifecycle for one app. See [Run Stillwater behind a reverse proxy](reverse-proxy.md).
- You want a production-validated automatic certificate path today. Stillwater's native ACME (Let's Encrypt) integration is currently marked **Experimental** in the TLS Status card pending end-to-end validation against a real public deployment. For production right now, prefer bring-your-own certificates -- or a reverse proxy with its own ACME client -- until the Experimental marker is removed.

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

## Confirm it took effect { #settings-general-tls-status }

Open Settings, scroll to the General tab. The TLS Status card shows one of three states:

- **Inactive** -- plain HTTP, no cert configured.
- **Active (BYO certificate)** -- direct TLS using the cert and key supplied via env vars or config file.
- **Active (ACME, &lt;domain&gt;)** -- ACME-managed cert. An amber **Experimental** chip and a short caveat appear next to this state until the ACME path has been validated end-to-end against a real public deployment.

The card also lists the bound port so you can confirm whether you're in collapse mode (HTTPS on the original `SW_PORT`) or split-port mode (HTTPS on `SW_TLS_PORT`). When HTTP/3 is enabled, an additional `HTTP/3 on :<port>/UDP` row appears. The card is read-only; configure TLS via env vars or the config file.

## HSTS

Once Stillwater detects a TLS connection, it automatically sets a strict `Strict-Transport-Security` header on every response. There is nothing to configure; the same gating used for proxy-terminated HTTPS handles direct TLS too.

## Docker healthcheck

The container's healthcheck targets `localhost` over the configured protocol. When `SW_TLS_CERT_FILE` is set, the entrypoint exports `SW_HEALTH_URL` so the probe uses HTTPS on the right port. The probe runs with `wget --no-check-certificate` to skip cert verification -- this is intentional, because a localhost healthcheck against a self-signed cert would otherwise fail for the wrong reason. You do not need to set `SW_HEALTH_URL` yourself; setting `SW_TLS_CERT_FILE` is enough.

## ACME and HTTP redirect

Beyond bring-your-own certificates, the TLS surface supports two related features that share the same listener stack. All three of these are configured via environment variables or `config.toml` / `config.yaml`; **there is no UI for TLS configuration** today, and the TLS Status card in Settings is read-only by design.

### ACME (Let's Encrypt) -- Experimental

Configure via `SW_ACME_DOMAIN`, `SW_ACME_EMAIL`, and `SW_ACME_CA`:

```sh
SW_ACME_DOMAIN=stillwater.example.com
SW_ACME_EMAIL=admin@example.com
# SW_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory   # optional: use the LE staging directory for testing
```

Or in `config.toml`:

```toml
[acme]
domain    = "stillwater.example.com"
email     = "admin@example.com"
# ca      = "https://acme-staging-v02.api.letsencrypt.org/directory"
# cache_dir = "/config/acme-cache"
```

The domain must resolve to this server and port 80 must be reachable from the public internet so the ACME challenge succeeds. The TLS Status card flags this path as **Experimental** with an amber chip and caveat until it has been validated end-to-end against a real public deployment -- prefer BYO certificates for production until that marker is removed.

### ACME (ZeroSSL EAB and IP-SAN)

For CAs that require External Account Binding (notably ZeroSSL) and for ordering a certificate against a public IP address instead of a DNS name, Stillwater uses a second ACME implementation backed by [`go-acme/lego`](https://github.com/go-acme/lego). It is selected automatically at startup based on which variables you set, and satisfies the same HTTP-01 challenge flow as the Let's Encrypt path (port 80 must be reachable).

**ZeroSSL with EAB credentials** (against a DNS name):

```bash
SW_ACME_DOMAIN=stillwater.example.com
SW_ACME_EMAIL=admin@example.com
SW_ACME_EAB_KEY_ID=your-zerossl-eab-key-id
SW_ACME_EAB_MAC_KEY=your-zerossl-eab-hmac-key
# SW_ACME_CA defaults to ZeroSSL (https://acme.zerossl.com/v2/DV90) when EAB keys are set.
```

**IP-SAN order** (a routable public IP, no DNS name):

```bash
SW_ACME_IP=203.0.113.5
SW_ACME_EMAIL=admin@example.com
```

Notes and constraints:

- `SW_ACME_EAB_KEY_ID` and `SW_ACME_EAB_MAC_KEY` must be set together. The HMAC key is a secret; Stillwater encrypts the cached ACME account and certificate at rest (AES-256-GCM) using the application encryption key.
- `SW_ACME_DOMAIN` and `SW_ACME_IP` are mutually exclusive -- a single order is for a DNS name or an IP, not both.
- `SW_ACME_IP` must be a publicly routable address. Private (RFC 1918), loopback, link-local, CGNAT, and other reserved ranges are rejected at startup, because a CA cannot validate an IP it cannot reach.
- The CA must support the requested capability (EAB and/or IP identifiers per RFC 8738). ZeroSSL is the canonical EAB provider.
- The ACME path remains marked **Experimental** in the TLS Status card pending end-to-end validation against a real public deployment; prefer BYO certificates for production until that marker is removed.

### HTTP-to-HTTPS redirect listener

Setting `SW_HTTP_REDIRECT_PORT=80` makes Stillwater bind a second plain-HTTP listener that 301-redirects every request to the HTTPS port. See the [HTTP-to-HTTPS redirect](http-redirect.md) how-to for setup details and reverse-proxy interactions.

## HTTP/3 (QUIC) { #http3-quic-firewall }

HTTP/3 is the next-generation HTTP protocol. It runs over QUIC (a UDP-based transport) instead of TCP, which lets modern browsers establish connections faster and handle packet loss more gracefully on flaky networks. Stillwater can advertise HTTP/3 alongside the existing TCP HTTPS listener so capable clients upgrade automatically.

### Enable

Set the env var:

```bash
SW_HTTP3_ENABLED=true
```

Or in `config.toml`:

```toml
[server.http3]
enabled = true
# port = 0  # 0 reuses the effective HTTPS port (SW_TLS_PORT or SW_PORT).
```

HTTP/3 requires direct TLS to be configured (it mandates TLS 1.3). Stillwater refuses to start if `SW_HTTP3_ENABLED=true` is set without a cert and key.

When enabled, HTTPS responses carry an `Alt-Svc` header advertising HTTP/3 on the configured UDP port. Browsers cache this advertisement and try HTTP/3 on subsequent requests. There is no separate URL for HTTP/3; it is a transport-layer upgrade.

### Firewall and NAT

QUIC runs over UDP. Any firewall or NAT between your clients and Stillwater must forward the configured port for **both TCP and UDP**. The TCP rule is what the existing HTTPS listener already uses; HTTP/3 needs an additional UDP rule on the same port number.

If UDP is blocked anywhere in the path, HTTP/3 simply fails to connect and the client transparently falls back to HTTP/1.1 or HTTP/2 over TCP. No user-visible error appears; the upgrade is opportunistic. This makes HTTP/3 a graceful enhancement: leaving it enabled in environments with partial UDP support never breaks the site.

### Docker

The container image declares both `EXPOSE 1973/udp` and `EXPOSE ${SW_TLS_PORT}/udp` so `docker run -P` and orchestrators that scrape image metadata pick up the QUIC ports automatically. With explicit port mappings, add the UDP variant alongside each TCP one in `docker-compose.yml`:

```yaml
ports:
  - "1973:1973"
  - "1973:1973/udp"
```

### Confirm

The TLS Status card in the Settings General tab adds a `HTTP/3 on :<port>/UDP` listener row when HTTP/3 is enabled. From a client, a quick check is:

```bash
curl -k --http3-only https://your-host/api/v1/health
```

A successful response confirms the QUIC handshake works end-to-end. In Chrome devtools, the Network tab's Protocol column shows `h3` for requests served over HTTP/3.
