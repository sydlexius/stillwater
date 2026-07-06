# Manage-artwork candidate grid - image provenance (#1756)

The candidate thumbnails in the hero's Manage-artwork modal are committed here so
the mock renders offline/deterministically. Every image is a verified
**public-domain** portrait of J.S. Bach from Wikimedia Commons (Bach died 1750;
each file's own Commons license was checked free-to-redistribute at fetch time via
`docs/hero/fetch-bach-candidates.py`). No attribution is legally required, and no
CC BY-SA / restricted image is used.

The provider badges in the grid (Fanart.tv, TheAudioDB, Deezer, Wikipedia,
Discogs) are **illustrative demo labels**: they show how Stillwater pulls
candidates from multiple providers and sorts them. They are NOT the real source of
each file - the real source is Wikimedia Commons for all five (below). `c01` and
`c02` are the same Haussmann portrait, intentionally shown as if returned by two
providers (a realistic multi-source result).

| File | Grid badge (demo) | License | Real source (Wikimedia Commons) |
|---|---|---|---|
| `c01.jpg` | Fanart.tv | Public domain | File:Johann Sebastian Bach (1746) - E. G. Haussmann |
| `c02.jpg` | TheAudioDB | Public domain | File:Johann Sebastian Bach (Haussmann portrait) |
| `c04.jpg` | Deezer | Public domain | File:Johann Sebastian Bach - Google Art Project |
| `c08.jpg` | Wikipedia | Public domain | File:Bach 1740 |
| `c09.jpg` | Discogs | Public domain | File:JSBach1 (engraving) |

Regenerate the grid fragment with `docs/hero/mocks/gen-mocks.sh` (reads these
committed images). To refresh or re-verify the pool, re-run
`docs/hero/fetch-bach-candidates.py` (re-checks each license).
