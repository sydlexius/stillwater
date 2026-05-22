# Changelog

All notable changes to Stillwater are documented in this file.

## [1.3.0] - Unreleased

### Breaking changes

- **Removed Classical library type.** The `classical` library type, deprecated
  in v1.2.0 (PR #1605), has been fully removed. All existing Classical libraries
  are automatically converted to Regular by startup migration `010`. No data is
  moved or deleted; only the library classification changes. If you used the
  `rule.classical_mode` setting or the `/api/v1/rules/classical-mode` endpoint,
  remove those references before upgrading. The Classical option no longer
  appears in the UI, and the API returns HTTP 400 for any request that sets
  `type=classical` on a library.

### Migration notes

- Migration `010` runs at startup and converts any remaining `type=classical`
  library rows to `type=regular`. The migration is idempotent and takes effect
  before the application serves its first request.

## [1.2.0] - 2026-04-11

- Initial public release series. See GitHub releases for per-release notes.
