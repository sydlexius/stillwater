package api

import (
	"context"
	"testing"
)

// TestSeedAuthProviderDefaults_FreshDB confirms that calling
// seedAuthProviderDefaults on a fresh DB writes a row at the canonical
// default for every key the page reads. This is the integration point for
// #1188: a value matching the code default must produce a real settings
// row so the export carries it.
func TestSeedAuthProviderDefaults_FreshDB(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	ctx := context.Background()

	// Pre-condition: the settings table has no auth.providers.* row at all.
	var preCount int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key LIKE 'auth.providers.%'`).Scan(&preCount); err != nil {
		t.Fatalf("pre-count: %v", err)
	}
	if preCount != 0 {
		t.Fatalf("expected fresh DB, got %d rows", preCount)
	}

	r.seedAuthProviderDefaults(ctx)

	// Every entry in authProviderDefaults must now have a row at the
	// declared default. We iterate the package-level slice so the test
	// stays in lock-step with the seed list.
	for _, d := range authProviderDefaults {
		var got string
		if err := r.db.QueryRowContext(ctx,
			`SELECT value FROM settings WHERE key = ?`, d.Key).Scan(&got); err != nil {
			t.Errorf("%s: row missing after seed: %v", d.Key, err)
			continue
		}
		if got != d.Default {
			t.Errorf("%s: got %q, want default %q", d.Key, got, d.Default)
		}
	}
}

// TestSeedAuthProviderDefaults_RespectExistingRows confirms that an
// existing row (e.g. user already changed the value) is NOT overwritten
// by a subsequent seed call. INSERT OR IGNORE is the load-bearing piece
// here -- a seed that clobbered user choices would be a regression worse
// than the original bug.
func TestSeedAuthProviderDefaults_RespectExistingRows(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	ctx := context.Background()

	// Manually set Emby default_role to administrator (the non-default).
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))`,
		"auth.providers.emby.default_role", "administrator"); err != nil {
		t.Fatalf("seeding existing row: %v", err)
	}

	r.seedAuthProviderDefaults(ctx)

	var got string
	if err := r.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`,
		"auth.providers.emby.default_role").Scan(&got); err != nil {
		t.Fatalf("post-seed query: %v", err)
	}
	if got != "administrator" {
		t.Errorf("seed clobbered existing row: got %q, want administrator", got)
	}
}

// TestSeedAuthProviderDefaults_Idempotent confirms a second seed call is a
// no-op (no spurious updated_at churn for callers that watch the settings
// table for changes).
func TestSeedAuthProviderDefaults_Idempotent(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	ctx := context.Background()

	r.seedAuthProviderDefaults(ctx)

	// Capture timestamps after first seed.
	type rowState struct{ value, updated string }
	first := map[string]rowState{}
	func() {
		rows, err := r.db.QueryContext(ctx,
			`SELECT key, value, updated_at FROM settings WHERE key LIKE 'auth.providers.%'`)
		if err != nil {
			t.Fatalf("first read: %v", err)
		}
		defer rows.Close() //nolint:errcheck
		for rows.Next() {
			var k, v, u string
			if err := rows.Scan(&k, &v, &u); err != nil {
				t.Fatalf("scan: %v", err)
			}
			first[k] = rowState{v, u}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("first rows iteration: %v", err)
		}
	}()

	// Second seed call.
	r.seedAuthProviderDefaults(ctx)

	rows2, err := r.db.QueryContext(ctx,
		`SELECT key, value, updated_at FROM settings WHERE key LIKE 'auth.providers.%'`)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	defer rows2.Close() //nolint:errcheck
	for rows2.Next() {
		var k, v, u string
		if err := rows2.Scan(&k, &v, &u); err != nil {
			t.Fatalf("scan2: %v", err)
		}
		prev, ok := first[k]
		if !ok {
			t.Errorf("second seed introduced new key: %s", k)
			continue
		}
		if prev.value != v {
			t.Errorf("%s: value drifted across idempotent seed: %q -> %q", k, prev.value, v)
		}
		if prev.updated != u {
			t.Errorf("%s: updated_at drifted across idempotent seed (INSERT OR IGNORE should not touch existing row): %q -> %q", k, prev.updated, u)
		}
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("second rows iteration: %v", err)
	}
}
