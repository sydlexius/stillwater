-- +goose Up
-- Issue #1640: persistence for the Lidarr verify-after-PUT rename safety toggle.
--
-- Adds the verify_path_after_update boolean column to connections. Default 0
-- (opt-in) so existing rows keep today's single-PUT behavior; operators flip
-- it on per Lidarr connection when they suspect path coercion against the
-- Root Folder list. The client-side capability landed in PR16
-- (lidarr.Client.SetVerifyPathAfterUpdate); this column is the persistence
-- half that lets the rename pipeline reach the setter from a stored
-- connection. The matching UI toggle is tracked separately.

-- +goose StatementBegin
ALTER TABLE connections ADD COLUMN verify_path_after_update INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE connections DROP COLUMN verify_path_after_update;
-- +goose StatementEnd
