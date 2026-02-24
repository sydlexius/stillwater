-- +goose Up

UPDATE rules SET automation_mode = 'notify' WHERE automation_mode = 'inbox';

-- +goose Down

UPDATE rules SET automation_mode = 'inbox' WHERE automation_mode = 'notify';
