---
description: Move your Stillwater configuration to another instance with an encrypted bundle.
---

<!-- code: internal/settingsio/export.go (Payload, CurrentEnvelopeVersion 1.6, ConnectionExport, RuleExport, PriorityExport, UserPrefsExport, UserExport, ImportOptions.AdminFallbackTokens, pbkdf2Iterations 600_000, transactional Import wrapping the per-section apply), internal/settingsio/users.go (id-first probe with ErrUserIDCollision halt), internal/settingsio/tokens.go (admin-fallback path), internal/api/router.go (POST /api/v1/settings/export, /api/v1/settings/import, POST /api/v1/setup/restore), internal/api/handlers_setup_restore.go (pre-admin OOBE restore handler with HasUsers gate + serialization mutex), web/templates/settings.templ maintenance tab (export passphrase + import upload + admin-fallback checkbox), web/templates/setup.templ (Start fresh / Restore from backup mode cards). -->

# Export and import settings

Stillwater's settings export bundles your configuration into a single encrypted JSON file. Useful when:

- Migrating from one host to another.
- Standing up a staging instance that should mirror production.
- Backing up your configuration as part of a disaster-recovery plan.

The export is **encrypted with a passphrase you provide**. Without the passphrase, the bundle can't be imported.

## What's in the bundle

The export includes everything that lives in Stillwater's database that you'd want to recreate on a new host:

- **Application settings** -- the things you've set under Settings > General.
- **Connections** -- Emby, Jellyfin, Lidarr URLs and API keys (decrypted in the bundle, re-encrypted on import).
- **Platform profiles** -- built-in profiles plus any custom ones you've created.
- **Provider API keys** -- each provider's stored key.
- **Provider priorities** -- per-field priority lists, including any per-library overrides.
- **Webhooks** -- outbound webhook definitions.
- **Rules** -- enable state, automation mode, and config for each rule. Names and descriptions are *not* exported (the receiving instance keeps its own current copy).
- **Scraper configurations** -- custom scraper YAMLs you've added.
- **User preferences** -- per-user UI prefs.
- **Users** -- usernames and roles for every account, plus stored password hashes for local (non-federated) accounts and, separately, federated identity references (provider type and external id) for federated accounts. Federated identities have no password hashes. Password hashes are bcrypt digests, never plaintext. The user list is included so that a backup taken on instance A can be restored on instance B without losing the API tokens or user preferences that are owned by users on A whose names B has not seen before.
- **API tokens** -- the stored hash, scopes, and ownership metadata. The plaintext token value is not stored in the database and so is never carried in the bundle.

What's **not** in the bundle:

- Your library data (artist records, NFO files, images on disk). The library lives in your music directory; the export carries the configuration that drives Stillwater's behavior, not the catalog itself.
- Encryption / session secrets. The receiving instance has its own.
- Backup history. The receiving instance has its own backup retention.
- Logs. Same.

## Export

1. Go to **Settings > Maintenance**.
2. Find the **Settings export / import** section.
3. Enter a **passphrase** of at least 8 characters. Use something long and memorable -- the export is useless without it. Stillwater does not store the passphrase.
4. Click **Export settings**.
5. Stillwater builds the bundle, encrypts it, and downloads a `.json` file to your browser. The filename includes a timestamp.

<!-- SCREENSHOT: Settings > Maintenance > Settings export | state: passphrase entered, ready to export | annotation: passphrase length hint + export button -->

The bundle is portable; any Stillwater instance with a matching version can import it.

### Save the bundle somewhere durable

Treat the file like a backup. Encrypt your backup target (e.g., a password-managed cloud drive) so the bundle's encryption isn't the only line of defense.

## Import

There are two ways to import an exported bundle, depending on whether the receiving instance already has an admin account.

### Into an existing instance (Settings > Maintenance)

1. On the target Stillwater instance, go to **Settings > Maintenance**.
2. Click **Import settings**.
3. Pick the `.json` file.
4. Enter the same passphrase that was used to export.
5. Click **Import**.

Stillwater decrypts and validates the bundle first. It then applies the whole bundle in a single database transaction covering every section (connections, platform profiles, webhooks, provider keys and priorities, rules, scraper preferences, application settings, users, user preferences, libraries, and API tokens), so an error in any section rolls the entire import back and the destination is left in the state it had before the import began. If the passphrase is wrong, decryption fails before anything is touched. Each section's import is upsert-by-natural-key, so a retry after a fixed-up bundle is safe. The result shows what was imported and what was skipped.

### Into a fresh instance, before admin creation (Restore from backup)

If you're standing up a new instance and want it to come up with the source instance's users (so you sign in with your *original* credentials, not a throwaway admin):

1. Open the new instance for the first time -- the setup screen renders because no admin exists yet.
2. Click the **Restore from backup** card.
3. Pick the `.json` file and enter the passphrase.
4. Click **Restore**.

The fresh instance applies the bundle in the same single-transaction atomic import described above, then marks onboarding as complete. The page redirects to the login screen; sign in with credentials from the source instance.

This path is gated on the receiving instance being truly empty (no admin user yet, onboarding not completed). Once an admin exists, the only way to import is through Settings > Maintenance described above.

## Cross-version compatibility

The bundle carries a schema version (the **envelope version**). Newer Stillwater versions can import older bundles -- newer fields are simply absent in the older payload, and the receiving instance leaves its defaults in place for those.

You **cannot** import a newer bundle into an older Stillwater. If the bundle's envelope version is higher than what the receiving instance understands, the import fails with a clear error. Upgrade the receiving instance before attempting the import.

The envelope versions and what each one added:

--8<-- "docs/_generated/envelope-versions.md"

## What import does on conflict

For each item in the bundle:

- **New items** are inserted.
- **Existing items** (matched by name or URL where applicable) are updated.
- **Item types not in the bundle** are left untouched on the receiving instance.

So an import on a fresh instance fully populates it; an import on an instance with existing config merges -- the bundle's view wins where things overlap.

### How users are handled

Bundles produced by recent versions carry every user's stable UUID, so the import matches by id first rather than by username. Three cases:

- **Same id on the target.** The user's mutable fields (display name, password hash, role on non-protected rows) are updated from the bundle. The `is_protected` flag is never overwritten -- protected status is a per-install policy, not transferable across instances.
- **Id absent on the target and username is free.** A new user row is inserted carrying the source id so downstream rows (API tokens, preferences) attribute correctly.
- **Id absent on the target but the username is taken under a *different* id.** The import halts with a clear error. This prevents one operator's account from silently being overwritten by another with the same username from a different instance. Resolve manually by renaming the colliding account on either side before retrying.

Older bundles (envelope version 1.3 and below) do not carry user UUIDs. For those, the import falls back to username-based matching and leaves same-username users untouched on the target -- the older softer behavior. Newer bundles get the id-first treatment described above.

### When the source's owner is missing on the target

If an API token's original owner is not in the bundle (e.g., the bundle came from an older Stillwater that did not include users) and is also not present on the receiving instance, the import treats the token as orphaned. By default, orphaned tokens are skipped and the result reports them under `api_tokens_skipped` so you can see what was lost.

There is an optional **Reassign orphan tokens to me** checkbox on the import form for the case where you would rather inherit those tokens than lose them. With it checked, orphan tokens are reattributed to the admin running the import; the result reports the count under `ownership_reassigned` so the change is visible. Enable it deliberately -- it is silent ownership transfer if you forget you ticked it.

## Common workflows

### Migrating to a new host

1. On the old host: Export with a passphrase.
2. Save the file somewhere both hosts can reach (or transfer it directly).
3. On the new host: Run through [getting started](../getting-started/index.md) to install Stillwater, but stop before configuring anything.
4. Import the bundle on the new host. Skip the first-run wizard if it appears.
5. Re-attach your library directory in the new host's compose / config -- or, better, point the new host's library volume at the same backing storage.

### Cloning prod into staging

1. On prod: Export with a passphrase.
2. On staging: Import the bundle.
3. On staging only: rotate any provider API keys you don't want shared between environments. Connections to prod-only platforms should be disabled or repointed.

### Backing up configuration before risky changes

1. Export before the change.
2. Make the change.
3. If something goes wrong, import the export to roll back.

## What survives the round-trip

Connections carry the full set of per-connection write toggles through the export and back, including:

- Library import on/off
- NFO write on/off
- Image write on/off
- Metadata push on/off
- Trigger refresh on/off
- Manage server-side images and NFO files on/off
- Verify file paths after a peer update

A successful import restores every toggle on the destination exactly as it was set on the source. If a connection toggle changes silently after an import, that is a bug; file an issue with the source and destination versions.

The import is atomic across every section: connections, platform profiles, webhooks, provider keys, provider priorities, rules, scraper preferences, application settings, users, user preferences, libraries, and API tokens. A failure in any section rolls back the whole import; the destination is left in the state it had before the import began.

## Troubleshooting

- **"Decryption failed."** Wrong passphrase, or the file was corrupted in transit. Re-export from the source.
- **"Envelope version too new."** The receiving instance is older than the source. Upgrade the receiving instance.
- **Connections show as enabled but won't connect.** API keys are imported but the platform-side might have rotated. Re-enter from the Connections tab.
