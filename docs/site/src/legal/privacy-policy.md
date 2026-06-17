---
description: What Stillwater sends over the network, and what it does not collect.
---

# Privacy Policy

**Effective date:** 2026-06-17

Stillwater is a self-hosted application. It runs entirely on hardware you control. This policy describes every outbound network connection Stillwater makes and states what data it does and does not transmit.

---

## What Stillwater does not do

- **No telemetry.** Stillwater sends no usage data, diagnostics, crash reports, or analytics to the project maintainers or any third party.
- **No user accounts.** There is no Stillwater cloud service. Your credentials stay on your machine.
- **No library uploads.** Your music files, library metadata, and scan results are never transmitted to the project or to any external service.
- **No tracking.** Stillwater contains no advertising IDs, tracking pixels, or session identifiers that leave your network.

---

## Outbound connections Stillwater makes

Stillwater makes outbound connections only for the purposes described below. All connections originate from the machine where Stillwater is running.

### Self-updater

When the self-updater is enabled (`updater.enabled = true`, which is the default), Stillwater periodically contacts the GitHub Releases API to check for new versions:

- **Version check:** `https://api.github.com/repos/sydlexius/stillwater/releases`
- **Binary download** (only when you trigger an update or Auto Update is on): files downloaded directly from `https://github.com/sydlexius/stillwater/releases/...`

The request includes a standard HTTP User-Agent header and the current Stillwater version string. No other identifying data is sent.

To disable update checks entirely, set `updater.enabled` to `false` in your configuration or via the UI under **Settings > Updater**.

### Metadata providers

When you fetch metadata for an artist, Stillwater queries one or more of the following external services. The only data sent is the artist name, a MusicBrainz identifier (MBID) where known, and any API keys you have configured. No user account data, no library paths, and no file content ever leave your machine.

| Provider | Endpoint(s) |
|---|---|
| TheAudioDB | `https://www.theaudiodb.com/api/v1/json`, `https://www.theaudiodb.com/api/v2/json` |
| Deezer | `https://api.deezer.com` |
| Discogs | `https://api.discogs.com` |
| DuckDuckGo (image search) | `https://duckduckgo.com`, `https://html.duckduckgo.com` |
| Fanart.tv | `https://webservice.fanart.tv/v3/music` |
| Genius | `https://api.genius.com` |
| Last.fm | `https://ws.audioscrobbler.com/2.0` |
| MusicBrainz | `https://musicbrainz.org/ws/2` |
| Spotify | `https://accounts.spotify.com/api/token`, `https://api.spotify.com/v1` |
| Wikidata | `https://query.wikidata.org/sparql`, `https://commons.wikimedia.org/w/api.php`, image files from `https://upload.wikimedia.org/` |
| Wikipedia | `https://{lang}.wikipedia.org/w/api.php` (language-specific; defaults to `en`), `https://www.wikidata.org/w/api.php` |

Providers are queried only when you or an automated rule triggers a metadata fetch. They are not contacted passively in the background outside of scheduled refresh tasks you configure.

### TLS certificate issuance (ACME)

When ACME is enabled (`SW_ACME_DOMAIN` / `acme.domain` is set), Stillwater uses `golang.org/x/crypto/acme/autocert` to obtain and renew a TLS certificate automatically. During certificate issuance and renewal, Stillwater contacts the configured ACME Certificate Authority:

- **Default CA:** Let's Encrypt production (`https://acme-v02.api.letsencrypt.org/directory`)
- **Configurable CA:** set `SW_ACME_CA` / `acme.ca` to any ACME-compliant directory URL (for example Buypass, or the Let's Encrypt staging endpoint)

The following data is transmitted to the CA during certificate issuance:

- The domain name (`SW_ACME_DOMAIN`)
- An ACME account key (generated locally on first run, cached in `acme-cache` alongside the database; never sent in plaintext - only the public key is registered with the CA)
- A Certificate Signing Request (CSR) containing the domain name
- The contact email address (`SW_ACME_EMAIL` / `acme.email`), if configured - the CA uses it for expiry notifications and account recovery; it is optional

ACME is disabled by default. It is only active when `SW_ACME_DOMAIN` is explicitly set. No certificate traffic occurs when Stillwater is running without ACME (plain HTTP, BYO TLS, or no TLS).

### Webhooks

Stillwater supports outbound webhooks that fire on configurable events (artist updated, sync completed, and others). Webhooks POST event payloads to URLs you specify. The destinations, content, and trigger conditions are entirely under your control. No webhook traffic goes to the project maintainers.

### Platform connections (Emby, Jellyfin, Lidarr)

Stillwater connects to the media server instances you configure under Settings. These connections go to servers on your own network (or a server you choose). No data is routed through the project or any Stillwater-operated infrastructure.

---

## Data stored on your machine

Stillwater stores its state in a local SQLite database (`SW_DB_PATH`). This file contains:

- Artist metadata you have curated
- Configuration and connection settings (API keys encrypted at rest with AES-256-GCM)
- Session data and authentication tokens

This data never leaves your machine unless you explicitly back it up or copy it somewhere.

---

## Contact

Questions about this policy can be directed to the project issue tracker at
<https://github.com/sydlexius/stillwater/issues> or to the maintainer via GitHub.
