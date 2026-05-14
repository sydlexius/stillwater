---
description: Configure inbound webhooks to trigger Stillwater scans and metadata refreshes from external services, with HMAC-SHA256 signature verification on every request and graceful shutdown handling for in-flight workers.
---

# Inbound webhooks

Stillwater accepts inbound webhook calls so external services can trigger scans and metadata refreshes without polling the API. Each endpoint can be locked down with an HMAC-SHA256 shared secret so only callers that know the secret can post.

Inbound webhooks are the right answer when:

- A media manager (Lidarr, Sonarr, or similar) wants to nudge Stillwater the moment it imports a new album.
- A reverse-proxy or upstream automation wants Stillwater to re-scan a specific library on a schedule it owns.
- You need a low-latency signal that does not depend on Stillwater's own scheduler.

Pair HMAC verification with TLS termination at every step: HMAC protects against unsigned or replayed requests, TLS protects the secret in transit. Skipping either weakens the other.

## How signature verification works

Configure an HMAC secret per inbound endpoint in **Settings -> Connections**. Stillwater stores the secret encrypted at rest using the same key that protects provider API tokens.

When a request arrives:

1. Stillwater looks up the configured secret for the matching endpoint.
2. If the lookup fails for any reason other than "no row" (transient database error, decrypt failure on a corrupted secret, etc.), the request is **rejected**. The endpoint does not silently downgrade to unverified.
3. If no secret is configured for the endpoint, the request is accepted without verification. This is the documented opt-in behavior; if you want all requests verified, configure a secret on every endpoint.
4. If a secret is configured, the request must carry an `X-Hub-Signature-256` header of the form `sha256=<hex>`, where `<hex>` is the lowercase hex HMAC-SHA256 of the raw request body using the configured secret. Constant-time comparison is used.

Callers can compute the signature with one line of OpenSSL:

```sh
echo -n "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print "sha256=" $2}'
```

Or in Python:

```python
import hashlib, hmac
sig = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
```

The header format matches GitHub's webhook convention, so any tooling that already speaks that dialect needs no changes.

## Configure a webhook

In the web UI, open **Settings -> Connections** and add an inbound webhook. You give it a name, pick the action (library scan, artist refresh, etc.), and paste a secret. Stillwater encrypts the secret on save; you can copy it elsewhere before saving, but the value is not readable after.

Rotate by replacing the value and saving. The new secret applies to the next request that arrives; no restart is needed.

## Graceful shutdown

Inbound webhook handlers run their work on a background goroutine so the HTTP response can return immediately. On shutdown:

1. The HTTP listener stops accepting new requests.
2. Existing webhook goroutines are given up to five minutes to finish whatever scan or refresh they kicked off. They receive a shutdown signal on their request context and are expected to honor it.
3. After five minutes, the drain returns and Stillwater proceeds with database close and exit. Any worker that ignored its context is abandoned.

A stuck or runaway webhook worker can no longer hang Stillwater's exit indefinitely; the bounded drain enforces a ceiling.

## Operational notes

- The HMAC secret is **per-endpoint**, not global. A leaked secret invalidates only the endpoint it was scoped to.
- Failed signature verifications are logged with the source IP and endpoint name. Repeated failures from one source usually mean a misconfigured caller, not an attack; check that the caller is signing the **raw** request body (not pretty-printed JSON, no trailing whitespace, no charset transcoding).
- Database hiccups during secret lookup return an HTTP error rather than silently allowing the request. If you see one of these in your logs, treat it like any other database availability issue.

## Troubleshooting

- **Caller reports 401/403 but the secret matches**: confirm the caller computed the HMAC over the **raw** request body. Many HTTP clients re-encode JSON before sending, which changes the bytes and therefore the signature.
- **No `X-Hub-Signature-256` header**: callers that did not opt in to signing get rejected the moment a secret is configured for the endpoint. Either configure them or remove the secret.
- **Worker still processing after Stillwater exits**: the drain timed out. The worker either ignored its context or was blocked on something outside its control (DNS, filesystem). Look at the last log lines for the worker before shutdown.
