---
description: Run Stillwater behind a reverse proxy. HTTPS termination, custom hostnames, subpath deployments, and SSE-aware proxy configuration for Caddy, Nginx, Traefik, and LSIO SWAG.
---

<!-- code: internal/api/handlers_sse.go (SSE endpoint, headers, write-deadline clear), internal/api/handlers_image.go (maxUploadSize = 25 << 20 = 25MB), internal/api/handlers_settings_io.go (maxImportSize = 10 << 20 = 10MB), internal/api/handlers.go + middleware/csrf.go (http.MaxBytesReader(w, req.Body, 1<<20) = 1MB generic body cap; Secure cookie flag from req.TLS or X-Forwarded-Proto), internal/api/handlers_oidc.go (OIDC redirect scheme also from X-Forwarded-Proto), internal/api/middleware/ratelimit.go (uses connection RemoteAddr; behind proxy all clients share proxy IP), internal/config/config.go (BasePath, BasePathFromEnv; Default port 1973), build/swag/stillwater.subdomain.conf.sample + build/swag/stillwater.subfolder.conf.sample (LSIO SWAG sample configs). -->

# Run Stillwater behind a reverse proxy

!!! info "Terminating TLS in Stillwater itself?"
    See [Direct TLS setup](direct-tls-setup.md). This page covers proxy-terminated TLS, where the proxy speaks HTTPS to clients and plain HTTP to Stillwater. Direct TLS skips the proxy entirely.

A reverse proxy lets you reach Stillwater over HTTPS, on a friendly hostname, alongside the rest of your self-hosted stack. Stillwater can also terminate TLS itself if you prefer a single-process deployment; the rest of this page assumes a fronting proxy.

This is an advanced topic. A localhost-only install doesn't need a reverse proxy at all.

## Decide: subdomain or subfolder

Two URL shapes work. Pick the one that matches your stack.

- **Subdomain** (recommended): `https://stillwater.example.com`. Stillwater serves at the root path. No environment variables to set on the container; the proxy just forwards `/` to `stillwater:1973`.
- **Subfolder**: `https://example.com/stillwater/`. Stillwater serves under a path prefix. Requires setting `SW_BASE_PATH=/stillwater` on the container so the application emits correctly-prefixed URLs and asset paths.

Subdomain is simpler operationally because the prefix coupling between proxy and app disappears. Subfolder fits when you're already running everything else under one apex domain (typical LSIO SWAG setup) and don't want to manage another DNS record.

## What Stillwater expects from a reverse proxy

Five requirements. Some proxies cover all of these by default; others need explicit configuration.

### 1. HTTP forwarding to `stillwater:1973`

Stillwater listens on port `1973` (configurable via `SW_PORT`). It speaks plain HTTP/1.1 internally; the proxy terminates TLS and forwards plaintext upstream. No mutual TLS, no HTTP/2 between proxy and app required.

### 2. Server-Sent Events (SSE) without buffering

Stillwater pushes live UI updates over SSE at `GET /api/v1/events/stream`. The endpoint sets `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, and `X-Accel-Buffering: no` (an Nginx-specific hint to disable response buffering). It clears the write deadline so connections stay open indefinitely.

Proxies that buffer responses by default (vanilla Nginx most notoriously) will hold SSE events back until the connection closes or the buffer fills, breaking real-time updates entirely. Each proxy section below shows the explicit directive for this.

Stillwater does not use WebSockets anywhere; SSE is the only long-lived bidirectional channel. Proxies that have WebSocket-specific gotchas (header upgrade dances) don't apply here.

### 3. Body size limit at least 25 MB

The largest body Stillwater accepts is a manual image upload at 25 MB. Settings imports cap at 10 MB. Most other endpoints cap at 1 MB.

Proxies with default body-size limits below 25 MB will return 413 Payload Too Large on artwork uploads. The configs below all set 25 MB explicitly to match the application limit.

### 4. Long read timeouts for scanner and bulk operations

The library scanner and bulk-action endpoints can run for a long time against large libraries. Stillwater's own server-side write timeout is 180 seconds, so set proxy read/send timeouts above that. The LSIO sample configs ship with `proxy_read_timeout 600s` (10 minutes) as conservative headroom; shorter timeouts (some proxies default to 30 or 60 seconds) will return 504 Gateway Timeout mid-operation while Stillwater is still working.

### 5. `X-Forwarded-Proto` header forwarded

Stillwater reads `X-Forwarded-Proto` to detect TLS termination upstream of itself. Three behaviors depend on it:

- The session cookie's `Secure` flag.
- The OIDC redirect URI scheme during federated login.
- The platform-self-URL Stillwater constructs in some surfaced links.

Without this header set, login cookies will not have `Secure`, OIDC redirects will use `http://` even when the user reached you over `https://`, and CSRF protection's secure-context check is weakened. Forward `X-Forwarded-Proto: https` from the proxy.

`X-Forwarded-For` is also worth forwarding for logs and rate limiting, though Stillwater currently uses the connection-level remote address for rate limiting; behind a proxy, all clients will share the proxy's IP for rate-limit purposes. A future enhancement may consume `X-Forwarded-For` directly.

## Proxy configurations

Each example assumes Stillwater is reachable at `stillwater:1973` (e.g., a Docker Compose service named `stillwater`). Substitute the hostname or IP appropriate to your network if you're not on Compose.

### Caddy (recommended for simplicity)

Caddy auto-provisions Let's Encrypt certificates, handles SSE flushing automatically, and forwards the standard `X-Forwarded-*` headers without configuration.

=== "Subdomain"

    ```caddyfile
    stillwater.example.com {
        reverse_proxy stillwater:1973
    }
    ```

    That's the entire config. Caddy:

    - Provisions a TLS certificate via Let's Encrypt (HTTP-01 or TLS-ALPN-01 challenge).
    - Sets `X-Forwarded-For`, `X-Forwarded-Proto`, and `X-Forwarded-Host` automatically.
    - Streams SSE without buffering (Caddy flushes on `Content-Type: text/event-stream`).
    - Has no default body size limit; 25 MB uploads work out of the box.
    - Defaults to a 30-second read timeout. Override for the scanner with the directive below.

    Add this to extend the timeout:

    ```caddyfile
    stillwater.example.com {
        reverse_proxy stillwater:1973 {
            transport http {
                read_timeout 600s
                write_timeout 600s
            }
        }
    }
    ```

=== "Subfolder"

    Set `SW_BASE_PATH=/stillwater` on the container, then:

    ```caddyfile
    example.com {
        handle_path /stillwater/* {
            reverse_proxy stillwater:1973 {
                transport http {
                    read_timeout 600s
                    write_timeout 600s
                }
            }
        }
        # ... your other handle blocks for the rest of example.com
    }
    ```

    Caddy's `handle_path` directive strips the prefix before forwarding; combined with `SW_BASE_PATH` on Stillwater, both sides agree on the prefix.

### Nginx (vanilla, without SWAG)

Vanilla Nginx requires explicit configuration for SSE because it buffers responses by default. The body-size limit, timeouts, and forwarded headers also need to be spelled out.

=== "Subdomain"

    ```nginx
    server {
        listen 443 ssl http2;
        server_name stillwater.example.com;

        ssl_certificate     /etc/letsencrypt/live/stillwater.example.com/fullchain.pem;
        ssl_certificate_key /etc/letsencrypt/live/stillwater.example.com/privkey.pem;

        client_max_body_size 25m;

        location / {
            proxy_pass http://stillwater:1973;

            proxy_http_version 1.1;
            proxy_set_header Connection "";

            # Forwarded headers Stillwater reads
            proxy_set_header Host              $host;
            proxy_set_header X-Real-IP         $remote_addr;
            proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_set_header X-Forwarded-Host  $host;

            # SSE: disable response buffering on the proxy side and chunked
            # encoding rewrites that would defeat real-time streaming.
            proxy_buffering off;
            proxy_cache off;

            # Long read timeout for the scanner and bulk operations.
            proxy_read_timeout 600s;
            proxy_send_timeout 600s;
        }
    }
    ```

    Why each block matters:

    - `client_max_body_size 25m`: matches Stillwater's image upload limit; without it, uploads larger than the Nginx default (1 MB) return 413.
    - `proxy_http_version 1.1` + `Connection ""`: required for HTTP keepalive and chunked encoding to work end-to-end.
    - `proxy_buffering off`: disables Nginx's response buffer for this location. Without it, SSE events queue in the buffer and arrive in batches when the buffer flushes or the connection closes. Stillwater's `X-Accel-Buffering: no` header is a hint, but Nginx ignores it under some configurations; the explicit directive here is authoritative.
    - `proxy_read_timeout 600s`: matches the scanner's worst case. Default Nginx is 60s, which truncates long scans.

=== "Subfolder"

    Set `SW_BASE_PATH=/stillwater` on the container. In your existing `server` block:

    ```nginx
    location ^~ /stillwater/ {
        proxy_pass http://stillwater:1973;

        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host  $host;

        proxy_buffering off;
        proxy_cache off;
        client_max_body_size 25m;
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
    }

    location = /stillwater {
        return 301 $scheme://$host/stillwater/;
    }
    ```

    The trailing redirect makes `https://example.com/stillwater` (no trailing slash) work the same as `https://example.com/stillwater/`.

### Traefik (Docker labels)

Traefik with Docker labels is configured directly on the Stillwater container in your `docker-compose.yml`. The example below assumes a Traefik instance is already running with an HTTPS entrypoint named `websecure` and a cert resolver named `letsencrypt`.

```yaml
services:
  stillwater:
    image: ghcr.io/sydlexius/stillwater:latest
    container_name: stillwater
    environment:
      - PUID=1000
      - PGID=1000
    volumes:
      - stillwater-data:/config
      - /path/to/your/music:/music:rw
    restart: unless-stopped
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.stillwater.rule=Host(`stillwater.example.com`)"
      - "traefik.http.routers.stillwater.entrypoints=websecure"
      - "traefik.http.routers.stillwater.tls.certresolver=letsencrypt"
      - "traefik.http.services.stillwater.loadbalancer.server.port=1973"
      # Long read timeout for scanner / bulk operations
      - "traefik.http.services.stillwater.loadbalancer.responseforwarding.flushinterval=100ms"

volumes:
  stillwater-data:
```

Notes:

- Traefik forwards `X-Forwarded-Proto`, `X-Forwarded-For`, and `X-Forwarded-Host` automatically when the request enters via an HTTPS entrypoint.
- `responseforwarding.flushinterval=100ms` ensures SSE chunks flush promptly; Traefik defaults to 100ms already, but setting it explicitly documents the intent.
- Traefik's default body size limits and read timeouts are usually generous enough for Stillwater. If you've tightened them globally elsewhere, raise the per-router timeout to at least 600s.

For subfolder mode, add a `StripPrefix` middleware:

```yaml
labels:
  - "traefik.http.routers.stillwater.rule=Host(`example.com`) && PathPrefix(`/stillwater`)"
  - "traefik.http.middlewares.stillwater-strip.stripprefix.prefixes=/stillwater"
  - "traefik.http.routers.stillwater.middlewares=stillwater-strip"
  # Plus SW_BASE_PATH=/stillwater in the environment block above
```

### LSIO SWAG (linuxserver/swag)

If you already run LSIO's SWAG container, Stillwater ships sample reverse-proxy configs in the repo at `build/swag/`:

- `stillwater.subdomain.conf.sample` -- ready-to-use subdomain config
- `stillwater.subfolder.conf.sample` -- subfolder config (paired with `SW_BASE_PATH=/stillwater`)

Drop the file you want into your SWAG `nginx/proxy-confs/` directory, rename it to remove `.sample`, and reload SWAG. Both samples include the LSIO `proxy.conf` macro (which handles `X-Forwarded-*` headers, HTTP/1.1 keepalive, and standard timeouts), explicitly raise the body limit to 25 MB, set `proxy_read_timeout 600s`, and assume the Stillwater container is reachable on the SWAG network as `stillwater` on port `1973`.

For subfolder mode, you also need `SW_BASE_PATH=/stillwater` on the Stillwater container so the application emits prefixed URLs.

## Verifying the proxy works

Three quick smoke tests cover the requirements above.

### TLS and basic forwarding

```bash
curl -I https://stillwater.example.com/
```

Expected: `HTTP/2 200` (or `HTTP/1.1 200`), `Server:` should NOT name Stillwater (the proxy's server header should win).

### SSE streaming

After signing in (so the session cookie is set), open the events stream:

```bash
curl -N -H "Cookie: <your_session_cookie>" https://stillwater.example.com/api/v1/events/stream
```

Expected: an immediate `event: connected` followed by periodic heartbeats. If you get a single response after the connection closes (or no output until you Ctrl-C), the proxy is buffering — recheck the `proxy_buffering off` / equivalent directive.

### Body size

```bash
dd if=/dev/zero of=/tmp/big.jpg bs=1M count=20
curl -X POST -H "Cookie: <your_session_cookie>" \
    -F "image=@/tmp/big.jpg" \
    "https://stillwater.example.com/api/v1/artists/<artist-id>/images/upload"
```

Substitute any artist ID from your library for `<artist-id>`. Expected: a Stillwater-side validation error or success, NOT a 413 from the proxy.

## Troubleshooting

- **Login appears to work but the session is not persisted.** Almost certainly `X-Forwarded-Proto: https` is missing. Stillwater sets the session cookie's `Secure` flag based on either `req.TLS != nil` (direct TLS) or `X-Forwarded-Proto: https` (proxy-terminated TLS); without one of those, the cookie is omitted on the next HTTPS request because the browser refuses to send a non-Secure cookie over HTTPS in some configurations. Forward the header.

- **Real-time UI updates lag or never arrive.** SSE is being buffered. Confirm the proxy directive that disables buffering is present and applies to the location/route handling Stillwater. For Nginx, `proxy_buffering off` must be in the same `location` block (not inherited from `http {}`) when the location explicitly sets other buffer settings.

- **Image uploads return 413 Payload Too Large.** The proxy's body limit is below 25 MB. Set `client_max_body_size 25m` (Nginx), or raise the equivalent setting on whichever proxy you use. Stillwater itself accepts up to 25 MB on image upload endpoints.

- **Long-running operations (scanner, bulk fix-all) abort with 504 Gateway Timeout.** Proxy read timeout is too short. Raise it above Stillwater's 180-second server write timeout; 600 seconds (matching the LSIO samples) is a conservative recommendation.

- **OIDC login redirects to `http://` instead of `https://` and the auth flow fails.** Stillwater is constructing the OIDC redirect URI from request scheme detection. With TLS terminated upstream, it depends entirely on `X-Forwarded-Proto` to know it's serving HTTPS. Forward the header.

- **Subfolder deployment: assets 404 even though the page loads.** `SW_BASE_PATH` is not set on the container, or its value doesn't match the proxy prefix. Both sides must agree: proxy under `/stillwater`, container has `SW_BASE_PATH=/stillwater`. Also: don't include a trailing slash in `SW_BASE_PATH`.

- **CSRF errors after deploying behind a proxy.** Stillwater's CSRF middleware checks for a secure context. The check passes when `X-Forwarded-Proto: https` is present OR the connection has direct TLS. Forward the header.

More problems live in [Troubleshooting](../troubleshooting/index.md).
