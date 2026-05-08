---
description: The mental model behind Stillwater -- artists, libraries, NFO files, images, providers, rules, locks, platform profiles, auto-provisioning, and the conflict gate.
---

# Core concepts

A short tour of the entities Stillwater is built around. If you've followed [Getting started](../getting-started/index.md), you've already used most of these in passing. This section explains the shape of each one and how they connect.

<div class="grid cards" markdown>

- __Artists and libraries__

    ---

    The two foundational entities. Libraries are folders (or remote sources) of music; artists are the people and groups inside them.

    [Read more](artists-and-libraries.md)

- __Auto-provisioning__

    ---

    How Stillwater creates a local account for a federated user on first sign-in, the guard rails that restrict who qualifies, and how roles are assigned.

    [Read more](auto-provisioning.md)

- __Conflict gate__

    ---

    How Stillwater detects when a connected media server is configured to write files that would collide with its own writes, and pauses output until the collision source is resolved.

    [Read more](conflict-gate.md)

- __Field locks__

    ---

    The mechanism that keeps your manual edits from being overwritten by refreshes, fixers, or connected platforms.

    [Read more](field-locks.md)

- __Images__

    ---

    Four image slots per artist -- thumb, fanart, logo, banner -- with platform-specific naming, multi-fanart, and resolution thresholds.

    [Read more](images.md)

- __NFO files__

    ---

    The XML metadata format every supported platform reads. Stillwater parses, edits, and writes Kodi-compatible artist.nfo files.

    [Read more](nfo-files.md)

- __Platform profiles__

    ---

    Named bundles of image filename conventions and NFO field-map settings that tell Stillwater how to write files for a specific media server.

    [Read more](platform-profiles.md)

- __Providers__

    ---

    Ten metadata providers, queried in per-field priority order with caching and ID propagation.

    [Read more](providers.md)

- __Rules__

    ---

    Checks that run against artists. Each rule has three meaningful states (disabled, manual, auto) and a fixer that resolves violations.

    [Read more](rules.md)

</div>

## How the pieces fit

```
Library  ---owns--->  Artists
   |                     |
   |                     +--- has an NFO file
   |                     +--- has 4 image slots
   |                     +--- can be locked (whole or per-field)
   |                     +--- is evaluated by Rules
   |                                    |
   |                                    +--- which call Providers
   |                                    +--- and produce fixable violations
   |
   +--- decides scan + watch behavior
   +--- holds the connection to Emby / Jellyfin / Lidarr (or "manual")
   +--- NFO + image writes shaped by the active Platform profile
   +--- NFO + image writes gated by the Conflict gate
```

Users sign in via a local account or a federated provider (Emby, Jellyfin, OIDC). Auto-provisioning creates a local account from the federated identity on first sign-in, subject to the provider's guard rail.

Most workflows touch three or four of these at once. A "refresh this artist's biography" run walks: artist → providers (in priority order) → field locks (which fields to skip) → platform profile (which NFO field map to use) → NFO file (atomic write) → conflict gate (paused if a platform is meddling). Knowing the pieces makes the workflow legible.
