# ACME Certificate Management

Stillwater supports automatic TLS certificate management via the ACME protocol.
Two certificate modes are available:

- **Domain-based** (Let's Encrypt or ZeroSSL): `SW_ACME_DOMAIN`
- **IP-based** (ZeroSSL only): `SW_ACME_IP`

Both modes serve ACME HTTP-01 challenges on port 80 and renew certificates
automatically.

## Manual TLS (no ACME)

If you already have a certificate and key, provide them directly:

```
SW_TLS_CERT_FILE=/data/tls.crt
SW_TLS_KEY_FILE=/data/tls.key
```

## Domain-based certificates (Let's Encrypt)

Requires a domain name that resolves to your server's public IP, and port 80
reachable from the internet.

```
SW_ACME_DOMAIN=stillwater.example.com
SW_ACME_EMAIL=admin@example.com        # optional but recommended
SW_ACME_CACHE_DIR=/data/acme-cache     # default; certificates cached here
```

`SW_TLS_CERT_FILE` / `SW_TLS_KEY_FILE` are not required when ACME is active.

## Domain-based certificates (ZeroSSL)

ZeroSSL requires External Account Binding (EAB) credentials. See
[Obtaining ZeroSSL EAB credentials](#obtaining-zerossl-eab-credentials) below.

```
SW_ACME_CA=zerossl
SW_ACME_DOMAIN=stillwater.example.com
SW_ACME_EMAIL=admin@example.com
SW_ACME_EAB_KEY_ID=<key-id-from-zerossl>
SW_ACME_EAB_MAC_KEY=<mac-key-from-zerossl>
SW_ACME_CACHE_DIR=/data/acme-cache
```

## IP-based certificates (ZeroSSL)

Let's Encrypt does not issue certificates for IP addresses. ZeroSSL is the only
mainstream ACME CA that issues IP SAN certificates.

**Requirements:**

- A publicly reachable IP address (RFC1918 / private addresses are rejected)
- Port 80 accessible from the internet (for HTTP-01 challenge verification)
- ZeroSSL EAB credentials (see below)

```
SW_ACME_CA=zerossl
SW_ACME_IP=203.0.113.42            # your public IP address
SW_ACME_EMAIL=admin@example.com    # optional but recommended
SW_ACME_EAB_KEY_ID=<key-id-from-zerossl>
SW_ACME_EAB_MAC_KEY=<mac-key-from-zerossl>
SW_ACME_CACHE_DIR=/data/acme-cache
```

## Custom ACME CA

Set `SW_ACME_CA` to any ACME directory URL to use a CA other than Let's Encrypt
or ZeroSSL:

```
SW_ACME_CA=https://acme.buypass.com/acme/directory
SW_ACME_DOMAIN=stillwater.example.com
SW_ACME_EMAIL=admin@example.com
```

## Obtaining ZeroSSL EAB credentials

1. Create a free account at [zerossl.com](https://zerossl.com).
2. Go to **Developer** > **EAB Credentials**.
3. Click **Generate** to create a new credential pair.
4. Copy the **KID** (Key ID) into `SW_ACME_EAB_KEY_ID`.
5. Copy the **HMAC Key** into `SW_ACME_EAB_MAC_KEY`.

The EAB MAC key is stored encrypted at rest in the ACME cache directory using
the application encryption key (`SW_ENCRYPTION_KEY`).

## Configuration reference

| Environment variable   | Default               | Description                                                  |
|------------------------|-----------------------|--------------------------------------------------------------|
| `SW_TLS_CERT_FILE`     |                       | Path to PEM certificate file (manual TLS)                    |
| `SW_TLS_KEY_FILE`      |                       | Path to PEM private key file (manual TLS)                    |
| `SW_ACME_CA`           | `letsencrypt`         | ACME CA: `letsencrypt`, `zerossl`, or a custom directory URL |
| `SW_ACME_DOMAIN`       |                       | Domain name for certificate (domain-based mode)              |
| `SW_ACME_EMAIL`        |                       | Contact email registered with the CA                         |
| `SW_ACME_CACHE_DIR`    | `/data/acme-cache`    | Directory for certificates and ACME state                    |
| `SW_ACME_EAB_KEY_ID`   |                       | ZeroSSL EAB key identifier                                   |
| `SW_ACME_EAB_MAC_KEY`  |                       | ZeroSSL EAB MAC key (Base64URL-encoded)                      |
| `SW_ACME_IP`           |                       | Public IP address for IP SAN certificate (ZeroSSL only)      |

## Port requirements

| Port | Purpose                                                         |
|------|-----------------------------------------------------------------|
| 80   | ACME HTTP-01 challenge + HTTP-to-HTTPS redirect (ACME mode)    |
| 443  | (or `SW_PORT`) HTTPS (when TLS is active)                       |

If your server is behind a reverse proxy that terminates TLS (e.g., Nginx,
Traefik, SWAG), do not configure ACME in Stillwater -- let the proxy handle TLS.
