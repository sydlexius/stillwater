-- +goose Up
-- Persistence for the Lidarr host<->platform path-mapping list (#2303).
--
-- Adds the path_mappings column to connections. Stores a JSON array of
-- {host_prefix, platform_prefix} pairs; the empty-string default means the
-- artist path is sent to the platform verbatim (today's behavior), which is
-- correct for shared-mount deployments where Stillwater's container and the
-- Lidarr peer address the library under the same path. Operators on split
-- mounts fill in the pair(s) so a rename/merge propagates a path Lidarr can
-- resolve. Mirrors migration 013 (verify_path_after_update).

-- +goose StatementBegin
ALTER TABLE connections ADD COLUMN path_mappings TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE connections DROP COLUMN path_mappings;
-- +goose StatementEnd
