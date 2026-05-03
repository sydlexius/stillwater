package settingsio

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/dbutil"
)

// LibraryExport carries the persistent configuration of a single library row.
// Connection IDs are not exported because they are generated per-instance;
// instead the owning connection's (type, url) is carried alongside so the
// import step can remap to the local connection_id. Runtime-detected
// shared-FS state (status / evidence / peer ids) is intentionally omitted --
// the target instance must re-detect on its own scan to avoid carrying stale
// cluster geometry across the boundary.
type LibraryExport struct {
	Name           string `json:"name"`
	Path           string `json:"path"`
	Type           string `json:"type"`                      // "regular" | "classical"
	Source         string `json:"source"`                    // "manual" | "emby" | "jellyfin" | "lidarr"
	ConnectionType string `json:"connection_type,omitempty"` // for remap on import
	ConnectionURL  string `json:"connection_url,omitempty"`  // for remap on import
	ExternalID     string `json:"external_id,omitempty"`
	FSWatch        int    `json:"fs_watch"`
	FSPollInterval int    `json:"fs_poll_interval"`
	NFOLockData    bool   `json:"nfo_lock_data,omitempty"`
}

// exportLibraries reads every row from the libraries table joined to its
// owning connection (when present) so the export can remap connection_id on
// the target instance. Runtime shared-FS columns are not selected.
func (s *Service) exportLibraries(ctx context.Context) ([]LibraryExport, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.name, l.path, l.type, l.source,
		       COALESCE(c.type, ''), COALESCE(c.url, ''),
		       l.external_id, l.fs_watch, l.fs_poll_interval, l.nfo_lock_data
		FROM libraries l
		LEFT JOIN connections c ON c.id = l.connection_id
		ORDER BY l.name
	`)
	if err != nil {
		return nil, fmt.Errorf("querying libraries: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []LibraryExport
	for rows.Next() {
		var (
			le         LibraryExport
			nfoLockInt int
		)
		if err := rows.Scan(
			&le.Name, &le.Path, &le.Type, &le.Source,
			&le.ConnectionType, &le.ConnectionURL,
			&le.ExternalID, &le.FSWatch, &le.FSPollInterval, &nfoLockInt,
		); err != nil {
			return nil, fmt.Errorf("scanning library row: %w", err)
		}
		le.NFOLockData = nfoLockInt != 0
		out = append(out, le)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating library rows: %w", err)
	}
	return out, nil
}

// importLibraries upserts library rows by name (which is UNIQUE in the schema).
// For non-manual libraries, the owning connection is resolved on the target
// via (type, url); a missing connection causes the library to be skipped with
// a warning rather than failing the entire import (same defensive pattern as
// importUserPreferences). Path validation is intentionally bypassed: the
// target filesystem may not yet contain the directory at restore time, and
// rejecting absent paths would defeat the disaster-recovery use case.
func (s *Service) importLibraries(ctx context.Context, libs []LibraryExport, result *ImportResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, le := range libs {
		if le.Name == "" {
			// A blank name cannot satisfy the UNIQUE constraint and would
			// collide on a second import. Skip with a warning rather than
			// fail, mirroring the defensive posture used elsewhere in import.
			slog.Warn("import: skipping library with empty name")
			result.LibrariesSkipped++
			continue
		}

		// Normalize the inbound source first so the remap decision and the
		// persisted row use the same value. validLibrarySource clamps unknown
		// future values to "manual"; using the raw le.Source for the remap
		// check while writing the normalized value would otherwise leave the
		// row in an inconsistent state (e.g., source="manual" with a non-NULL
		// connection_id, or a missing-connection skip for a row that would
		// have persisted as manual).
		source := validLibrarySource(le.Source)

		// Resolve connection_id for non-manual libraries by remapping (type, url)
		// to a local connection. Manual libraries carry no connection reference.
		var connectionID string
		if source != "manual" {
			// Defensive guard: NewService always wires connectionSvc today, but
			// a nil here would panic on the GetByTypeAndURL call below. Surface
			// a clear error instead so a misconfigured Service fails fast and
			// loud rather than crashing mid-import.
			if s.connectionSvc == nil {
				return fmt.Errorf("importing library %q: connection service is required for source %q", le.Name, source)
			}
			if le.ConnectionType == "" || le.ConnectionURL == "" {
				slog.Warn("import: skipping library missing connection reference",
					"library", le.Name, "source", source)
				result.LibrariesSkipped++
				continue
			}
			conn, err := s.connectionSvc.GetByTypeAndURL(ctx, le.ConnectionType, le.ConnectionURL)
			if err != nil {
				return fmt.Errorf("looking up connection for library %q: %w", le.Name, err)
			}
			if conn == nil {
				// Connection URLs may embed credentials (e.g. http://user:pass@host)
				// or sensitive query params, so the URL value itself is omitted from
				// the warning. A boolean presence flag is enough to disambiguate
				// "connection reference absent on target" from "no reference given".
				slog.Warn("import: skipping library whose connection is absent on target",
					"library", le.Name,
					"connection_type", le.ConnectionType,
					"connection_url_present", le.ConnectionURL != "",
				)
				result.LibrariesSkipped++
				continue
			}
			connectionID = conn.ID
		}

		// Look up an existing library by name (UNIQUE) to drive upsert. We do
		// not use INSERT ... ON CONFLICT because the libraries table also has
		// a partial unique index on (connection_id, external_id) and a name
		// match must take precedence over an external_id collision.
		var existingID string
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM libraries WHERE name = ?`, le.Name,
		).Scan(&existingID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Insert new
			id := uuid.New().String()
			if _, err := s.db.ExecContext(ctx, `
				INSERT INTO libraries (
					id, name, path, type, source, connection_id, external_id,
					fs_watch, fs_poll_interval, nfo_lock_data, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				id, le.Name, le.Path, validLibraryType(le.Type),
				source, dbutil.NullableString(connectionID), le.ExternalID,
				validFSWatch(le.FSWatch), validPollInterval(le.FSPollInterval),
				boolToInt(le.NFOLockData), now, now,
			); err != nil {
				return fmt.Errorf("inserting library %q: %w", le.Name, err)
			}
		case err != nil:
			return fmt.Errorf("looking up library %q: %w", le.Name, err)
		default:
			// Update existing
			if _, err := s.db.ExecContext(ctx, `
				UPDATE libraries SET
					path = ?, type = ?, source = ?, connection_id = ?, external_id = ?,
					fs_watch = ?, fs_poll_interval = ?, nfo_lock_data = ?, updated_at = ?
				WHERE id = ?
			`,
				le.Path, validLibraryType(le.Type),
				source, dbutil.NullableString(connectionID), le.ExternalID,
				validFSWatch(le.FSWatch), validPollInterval(le.FSPollInterval),
				boolToInt(le.NFOLockData), now, existingID,
			); err != nil {
				return fmt.Errorf("updating library %q: %w", le.Name, err)
			}
		}
		result.Libraries++
	}
	return nil
}

// validLibraryType clamps an inbound type to one of the schema's allowed
// values. The CHECK constraint on libraries.type would otherwise reject
// payloads carrying unrecognized values from a tampered or future-version
// export.
func validLibraryType(t string) string {
	switch t {
	case "regular", "classical":
		return t
	default:
		return "regular"
	}
}

// validLibrarySource clamps an inbound source value to one of the recognized
// constants. An empty or unknown source is normalised to "manual" so the
// library still loads (with no connection linkage) rather than being dropped
// outright.
func validLibrarySource(s string) string {
	switch s {
	case "manual", "emby", "jellyfin", "lidarr":
		return s
	default:
		return "manual"
	}
}

// validPollInterval mirrors library.IsValidPollInterval without importing the
// library package (which would create an import cycle once settingsio is
// consumed by code under internal/library). The list must stay in sync with
// library.ValidPollIntervals.
func validPollInterval(v int) int {
	switch v {
	case 60, 300, 900, 1800:
		return v
	default:
		return 60
	}
}

// validFSWatch clamps the imported fs_watch flag to the canonical 0/1 the
// schema stores. The column is declared INTEGER without a CHECK, so a tampered
// or future-version export carrying any non-zero value would otherwise be
// persisted verbatim and could surprise consumers that compare against 1
// rather than truthiness. Mirrors the boundary-clamping pattern used by
// validLibraryType / validLibrarySource / validPollInterval.
func validFSWatch(v int) int {
	if v != 0 {
		return 1
	}
	return 0
}

// boolToInt converts a Go bool to the integer representation SQLite expects
// for nfo_lock_data. Local helper to avoid leaking the same conversion across
// every call site.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
