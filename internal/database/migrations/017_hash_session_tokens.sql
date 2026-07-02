-- +goose Up
-- Issue #2170: hash session tokens at rest (parity with API tokens).
--
-- Before #2170 the session token was stored in cleartext in sessions.id. After
-- the fix, CreateSession persists only the hex-encoded SHA-256 hash and
-- ValidateSession/Logout hash the incoming cookie before the lookup/delete.
--
-- Existing rows hold cleartext ids that will never match the hashed lookup, so
-- they are dead weight. This one-way security remediation clears them; affected
-- users benignly re-login (their next request fails ValidateSession and they are
-- redirected to the login page). No cleartext-to-hash in-place upgrade is
-- possible without knowing each raw token, which we deliberately no longer keep.
DELETE FROM sessions;

-- +goose Down
-- One-way migration. The cleared cleartext sessions cannot be reconstructed and
-- re-storing cleartext tokens would reintroduce the vulnerability, so Down is a
-- no-op rather than a fake restoration (following the 014_clear_imported_locks
-- precedent for irreversible data migrations).
SELECT 1;
