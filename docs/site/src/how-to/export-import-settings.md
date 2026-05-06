---
description: Move your Stillwater configuration to another instance with an encrypted bundle.
---

<!-- code: internal/settingsio/export.go (Payload, CurrentEnvelopeVersion 1.3, ConnectionExport, RuleExport, PriorityExport, UserPrefsExport, UserExport, ImportOptions.AdminFallbackTokens, pbkdf2Iterations 600_000), internal/settingsio/users.go, internal/settingsio/tokens.go (admin-fallback path), internal/api/router.go (POST /api/v1/settings/export, /api/v1/settings/import), web/templates/settings.templ maintenance tab (export passphrase + import upload + admin-fallback checkbox). -->

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
- **Users** -- usernames, roles, and stored password hashes for local accounts plus federated identity references (provider type and external id). Password hashes are bcrypt digests, never plaintext. The user list is included so that a backup taken on instance A can be restored on instance B without losing the API tokens or user preferences that are owned by users on A whose names B has not seen before.
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

1. On the target Stillwater instance, go to **Settings > Maintenance**.
2. Click **Import settings**.
3. Pick the `.json` file.
4. Enter the same passphrase that was used to export.
5. Click **Import**.

Stillwater decrypts, validates, and applies the bundle. The result shows what was imported and what was skipped. If the passphrase is wrong, decryption fails before anything is touched.

## Cross-version compatibility

The bundle carries a schema version (the **envelope version**). Newer Stillwater versions can import older bundles -- newer fields are simply absent in the older payload, and the receiving instance leaves its defaults in place for those.

You **cannot** import a newer bundle into an older Stillwater. If the bundle's envelope version is higher than what the receiving instance understands, the import fails with a clear error. Upgrade the receiving instance before attempting the import.

## What import does on conflict

For each item in the bundle:

- **New items** are inserted.
- **Existing items** (matched by name or URL where applicable) are updated.
- **Item types not in the bundle** are left untouched on the receiving instance.

So an import on a fresh instance fully populates it; an import on an instance with existing config merges -- the bundle's view wins where things overlap.

### How users are handled

Users get a softer treatment than other items because their state on the receiving instance often reflects local choices (rotated passwords, role changes) that should not be silently overwritten:

- **Users present on the receiving instance with the same username** are left exactly as they are. The bundle's row is ignored. This means an admin who has rotated their password on the target keeps the new password, even if the source is older.
- **Users absent on the receiving instance** are recreated from the bundle so any of their API tokens or preferences in the same import can attribute back to them.

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

## Troubleshooting

- **"Decryption failed."** Wrong passphrase, or the file was corrupted in transit. Re-export from the source.
- **"Envelope version too new."** The receiving instance is older than the source. Upgrade the receiving instance.
- **Connections show as enabled but won't connect.** API keys are imported but the platform-side might have rotated. Re-enter from the Connections tab.
