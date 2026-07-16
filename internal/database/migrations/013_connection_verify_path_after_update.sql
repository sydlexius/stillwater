-- +goose Up
-- Persistence for the Lidarr verify-after-PUT rename safety toggle.
--
-- Adds the verify_path_after_update boolean column to connections. Default 0
-- (opt-in) so existing rows keep today's single-PUT behavior; operators flip
-- it on per Lidarr connection when they suspect path coercion against the
-- Root Folder list. The client-side capability (the former
-- lidarr.Client.SetVerifyPathAfterUpdate) was removed in #2419: the publish
-- layer now read-backs every peer unconditionally, so the toggle no longer
-- gates any behavior. This migration is kept as applied history; retiring the
-- now-inert column is tracked separately.

-- +goose StatementBegin
ALTER TABLE connections ADD COLUMN verify_path_after_update INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE connections DROP COLUMN verify_path_after_update;
-- +goose StatementEnd
