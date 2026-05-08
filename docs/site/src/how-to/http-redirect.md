---
description: Add a plain-HTTP listener that 301-redirects every request to the HTTPS port.
---

# HTTP-to-HTTPS redirect

When you serve HTTPS directly from Stillwater (see [Direct TLS setup](direct-tls-setup.md)), users who type `http://yourhost` in the address bar reach a closed port unless you also bind plain HTTP somewhere. The HTTP-to-HTTPS redirect listener fills that gap: it binds a second port (typically `80`) and answers every request with a `301 Moved Permanently` to the same path on HTTPS.

## When to enable it { #http-redirect-when }

Enable the redirect listener when:

- Stillwater terminates TLS itself (you have set `SW_TLS_CERT_FILE` and `SW_TLS_KEY_FILE`).
- The host is reachable on port 80 from the networks your users come from.
- You do not have a reverse proxy in front of Stillwater that already handles the redirect.

Skip it when:

- A reverse proxy (SWAG, Caddy, Traefik, Nginx) fronts Stillwater. The proxy should own port 80 and the redirect; doubling up either fails to bind or produces a redirect loop.
- Stillwater is reached only via direct HTTPS links (no human types the URL by hand).
- Port 80 is unavailable on the host (used by another service, blocked by the firewall, not forwarded by NAT).

## Enabling the redirect listener { #http-redirect-enable }

Set the redirect port via env var, command line, or config file. Port 80 is the standard HTTP port and what browsers assume when no scheme prefix is given.

```bash
export SW_TLS_CERT_FILE=/config/tls/fullchain.pem
export SW_TLS_KEY_FILE=/config/tls/privkey.pem
export SW_HTTP_REDIRECT_PORT=80
```

Or in `config.toml`:

```toml
[server.tls]
cert_file = "/config/tls/fullchain.pem"
key_file  = "/config/tls/privkey.pem"

[server.http_redirect]
port = 80
```

Stillwater rejects two misconfigurations at startup:

- The redirect port equals the HTTPS port. The two listeners cannot share a socket.
- The redirect port is set without TLS configured. There would be nowhere to redirect to.

## Port 80 reachability { #http-redirect-port-80 }

Binding port 80 has two practical wrinkles:

- **Privileged port.** On Linux ports below 1024 require root or the `CAP_NET_BIND_SERVICE` capability. The official container image runs with that capability when needed; native installs may need `setcap` or a higher port (e.g. `SW_HTTP_REDIRECT_PORT=8080` paired with a NAT rule on the gateway).
- **NAT and firewalls.** If users reach Stillwater across a router or cloud security group, port 80 must be forwarded just like the HTTPS port. Otherwise the redirect listener is bound but unreachable, and the typed-URL case still fails silently.

## Verifying the redirect { #http-redirect-verify }

Use `curl -I` to see the redirect headers without following them:

```bash
curl -I http://yourhost/
```

Expected response:

```
HTTP/1.1 301 Moved Permanently
Location: https://yourhost/
```

Query strings and paths are preserved. A request to `http://yourhost/artists?page=2` redirects to `https://yourhost/artists?page=2`.

## Settings page indicator { #http-redirect-settings }

Once the redirect listener is active the Settings General tab shows it on the TLS Status card under the Listening row, alongside the HTTPS port. If the row is missing after a restart, double-check the env var spelling and the startup logs for a validation error.

## Interaction with reverse proxies { #http-redirect-reverse-proxy }

Do not enable the redirect listener and a reverse proxy that handles TLS at the same time. Two patterns work; pick one:

- **Reverse proxy terminates TLS.** Stillwater stays on plain HTTP on its default port; the proxy listens on 80 and 443 and issues the redirect itself. Leave `SW_HTTP_REDIRECT_PORT` unset.
- **Stillwater terminates TLS.** Stillwater binds 443 and (with this feature enabled) 80; no reverse proxy is involved on those ports.

Mixing them either fails to bind port 80 (proxy and Stillwater contend for the same socket) or produces a redirect loop (proxy redirects to HTTPS, Stillwater redirects back, browser bails after a few hops).
