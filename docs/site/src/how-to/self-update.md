---
description: Apply a new version manually or have Stillwater check for updates.
---

<!-- code: internal/updater/updater.go (Channel: stable / prerelease / nightly; SettingChannel; SettingAutoCheck stored-but-no-background-scheduler-consuming-it; isDocker detection), internal/api/router.go (POST /updates/check, GET /updates/status, POST /updates/apply, GET/PUT /updates/config), web/templates/settings.templ updates tab. Channel selector is fully shipped (UI + persistence + drives pickLatest). Auto-check toggle UI ships but has no background scheduler yet; check-interval slider not in UI (W2.E #1117 if/when it lands). -->

# Update Stillwater

How you update depends on how you installed Stillwater.

## Docker / Compose installs

When Stillwater detects it's running in a container, the in-place updater is disabled. Updates flow through your container image.

1. Pull the new image:

    ```bash
    docker compose pull
    ```

2. Recreate the container:

    ```bash
    docker compose up -d
    ```

The Updates tab in Settings shows your current version and the latest available, plus a banner reading "Updates are managed by your container image."

If you pin a version tag in your compose file (`ghcr.io/sydlexius/stillwater:v1.0.0`), update the tag and run `docker compose pull && docker compose up -d` to apply.

### Switching channels (Docker)

Channels are tag conventions on the image:

- `:latest` -- stable releases.
- `:nightly` -- date-stamped builds tracked from main.
- `:vX.Y.Z` / `:vX.Y.Z-rc.N` -- specific stable / prerelease tags.

To switch channels, change the tag in your compose file and pull.

## Native binary installs

When Stillwater is running directly on the host (not in a container), it can apply updates in place.

### Check for updates

1. Go to **Settings > Updates**.
2. Click **Check now**.
3. Stillwater queries GitHub Releases for the latest version on your active channel.
4. The result shows current vs latest. If a newer version is available, an "Update available" badge appears next to the latest version.

### Apply an update

1. With an update available, click **Apply update**.
2. Status updates as the cycle runs: "Downloading...", "Verifying...", "Applying...".
3. When the new binary is staged, a **Restart required** banner appears with instructions for your environment.
4. Restart Stillwater. The new version comes up.

The apply step is atomic: the new binary is downloaded, verified, and swapped in only when the swap can complete. A failure during download or verification leaves the running version untouched.

<!-- SCREENSHOT: Settings > Updates | state: native install with update available + restart-required banner mid-flow | annotation: status display + restart prompt -->

### Channels (native)

Three channels:

- **Stable** -- non-prerelease semver tags (`v1.2.3`). Default.
- **Prerelease** -- includes prerelease tags (`v1.2.3-rc.1`, `v1.2.3-beta.1`). For people who want to test upcoming releases.
- **Nightly** -- date-stamped builds (`nightly-YYYYMMDD`). Built from main; expect the most churn.

Switch channels under Settings > Updates (channel selector). Switching does not roll your version backwards -- it adjusts which versions Stillwater considers "available" going forward.

### Updates settings auto-save

The Updates tab has no Save button. Channel, "enable updater", "auto-check on schedule", and the check-interval selector all persist on change with a confirmation toast. If a save fails, the controls snap back to the persisted server values so the UI never drifts from what is actually saved.

If the updater config fails to load on the server side (rare; typically a database read error), the tab renders with the in-code defaults shown in disabled-looking state and a banner across the top explains the read failed. Saves are blocked while the load-failed state is active so a click cannot overwrite real config with the displayed fallback values. Reload the page after fixing the underlying issue to clear the banner.

## Verifying releases

Each release publishes a `checksums.txt` (SHA256 hashes) and a cosign keyless signature for it (`checksums.txt.sigstore.json`). Docker images are also signed with cosign keyless and have SLSA build provenance attached.

### Native binaries

For a basic integrity check, compare SHA256:

1. Download the release's `checksums.txt` from the GitHub Release page.
2. Run `shasum -a 256 stillwater-...` against the binary you have.
3. Compare the result to the line in `checksums.txt`.

For end-to-end signature verification, also verify `checksums.txt` itself with [cosign](https://docs.sigstore.dev/cosign/installation/):

```bash
cosign verify-blob \
    --bundle checksums.txt.sigstore.json \
    --certificate-identity-regexp 'https://github.com/sydlexius/stillwater/' \
    --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
    checksums.txt
```

A `Verified OK` result confirms the checksums file was produced by the official release workflow.

### Docker images

Pull by digest for exact reproducibility:

```bash
docker pull ghcr.io/sydlexius/stillwater@sha256:<digest>
```

Or verify the cosign signature directly:

```bash
cosign verify ghcr.io/sydlexius/stillwater:<version> \
    --certificate-identity-regexp 'https://github.com/sydlexius/stillwater/' \
    --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

## Rolling back

### Docker

Pin the previous version's tag in your compose file and `docker compose pull && docker compose up -d`.

### Native

The auto-updater performs an in-place atomic replacement and removes its internal backup file as the final step of a successful update. **No rollback file is left on disk after `Apply update` completes.** To revert to an earlier version:

1. Identify the version you want from the [GitHub Releases page](https://github.com/sydlexius/stillwater/releases).
2. Stop Stillwater.
3. Download and install the older version's archive using the same steps as [install the binary](../getting-started/install-binary.md), substituting the older version number in the URL.
4. Start Stillwater.

Your library, database, and configuration directory are not affected by the binary swap, so rolling back preserves all your data. The version reported at the top of Settings > Updates will reflect the rollback after restart.

## What an update changes

Updates are intended to be drop-in. They:

- Replace the binary (native) or image (Docker).
- Run any pending database migrations on first start.
- Refresh built-in rule descriptions if any were edited upstream (your enable/automation/config choices are preserved).

They do **not** touch:

- Your music library.
- Your `/config` directory contents (database, secrets, settings).
- Provider API keys or connection credentials.

If you've followed the [install guides](../getting-started/index.md), the durable state is in `/config` and the music library mount; everything else can be replaced.

## Troubleshooting

- **"Update check failed."** Stillwater couldn't reach GitHub. Check outbound HTTPS to `api.github.com` and `objects.githubusercontent.com`.
- **"Apply failed."** The download or verification step didn't complete. The previous version is still running. Check the Logs tab for details; common causes are disk-full and write-permission issues on the install path.
- **Stillwater won't start after applying.** A migration may have failed. Check the logs at the configured log path. Roll back to the previous binary (see above) and report the issue.

## See also

- [Install with Docker Compose](../getting-started/install-docker-compose.md)
- [Install the binary](../getting-started/install-binary.md)
