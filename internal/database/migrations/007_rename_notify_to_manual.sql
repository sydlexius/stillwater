-- +goose Up
UPDATE rules SET automation_mode = 'manual' WHERE automation_mode = 'notify';

-- +goose Down
UPDATE rules SET automation_mode = 'notify' WHERE automation_mode = 'manual';
