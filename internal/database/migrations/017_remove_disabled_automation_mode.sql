-- +goose Up
-- Convert any rules using the removed "disabled" automation mode.
-- The Enabled toggle is the sole on/off control; disabled rows become manual + enabled=0.
UPDATE rules SET enabled = 0, automation_mode = 'manual' WHERE automation_mode = 'disabled';

-- +goose Down
-- No down migration (intent is irreversible; re-enabling is a user action)
