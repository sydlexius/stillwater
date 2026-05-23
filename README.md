# Stillwater

[![CI](https://github.com/sydlexius/stillwater/actions/workflows/ci.yml/badge.svg)](https://github.com/sydlexius/stillwater/actions/workflows/ci.yml)
[![Release](https://github.com/sydlexius/stillwater/actions/workflows/release.yml/badge.svg)](https://github.com/sydlexius/stillwater/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/sydlexius/stillwater/branch/main/graph/badge.svg)](https://codecov.io/gh/sydlexius/stillwater)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/sydlexius/stillwater/badge)](https://securityscorecards.dev/viewer/?uri=github.com/sydlexius/stillwater)
[![License: GPL-3.0](https://img.shields.io/badge/License-GPL--3.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/sydlexius/stillwater)](go.mod)

Stillwater is a self-hosted web app for keeping your music library's artist
and composer metadata clean and consistent. It reads and writes NFO files,
manages artwork, and pulls metadata from MusicBrainz and Fanart.tv — all
from one web UI that works alongside Emby, Jellyfin, and Kodi.

**Full documentation:** <https://sydlexius.github.io/stillwater/>

## Features

- Browse and edit artist and composer NFO metadata from one place
- Fetch, crop, and organize artist artwork
- Pull metadata from MusicBrainz and Fanart.tv
- Works alongside Emby, Jellyfin, and Kodi
- Web UI plus a REST API at `/api/v1/` for automation and integration
- Session-based authentication and webhook notifications

## Quick start

```yaml
# docker-compose.yml
services:
  stillwater:
    image: ghcr.io/sydlexius/stillwater:latest
    container_name: stillwater
    ports:
      - "1973:1973"
    environment:
      - PUID=99
      - PGID=100
    volumes:
      - stillwater-data:/config
      - /path/to/your/music:/music:rw
    restart: unless-stopped

volumes:
  stillwater-data:
```

```bash
docker compose up -d
```

Then open <http://localhost:1973>.

Looking for a different install path, configuration knobs, or details on
how Stillwater plays with media-server NFO write-back? It's all on the
**[project site](https://sydlexius.github.io/stillwater/)** — install
guides (Docker, binary, Unraid Community Applications, reverse proxy),
environment-variable reference, encryption-key handling, the
round-trip-conflict gate, and the full OpenAPI reference.

Binary downloads live on the
[Releases](https://github.com/sydlexius/stillwater/releases) page.
Nightly builds (`nightly-YYYYMMDD`) ship from `main` each day there are
new commits — see the
[self-update guide](https://sydlexius.github.io/stillwater/how-to/self-update/)
for the nightly channel.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
dev environment setup, code style, and pull-request workflow. Participants
are expected to follow the project
[Code of Conduct](CODE_OF_CONDUCT.md).

## License

[GPL-3.0](LICENSE)
