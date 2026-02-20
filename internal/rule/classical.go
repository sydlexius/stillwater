package rule

import (
	"context"
	"database/sql"
)

// Classical evaluation modes.
const (
	ClassicalModeSkip      = "skip"      // Skip rules entirely, return 100% health
	ClassicalModeComposer  = "composer"  // Evaluate as a composer directory
	ClassicalModePerformer = "performer" // Evaluate as a performer directory
)

const classicalModeKey = "rule.classical_mode"

// GetClassicalMode reads the classical music evaluation mode from the settings table.
// Returns "skip" if no value is set (the default).
func GetClassicalMode(ctx context.Context, db *sql.DB) string {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, classicalModeKey).Scan(&value)
	if err != nil || value == "" {
		return ClassicalModeSkip
	}
	return value
}

// SetClassicalMode writes the classical music evaluation mode to the settings table.
func SetClassicalMode(ctx context.Context, db *sql.DB, mode string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, classicalModeKey, mode)
	return err
}
