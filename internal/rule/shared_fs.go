package rule

import (
	"context"
	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
)

// LibraryQuerier is the subset of library.Service needed by SharedFSCheck.
// Using a narrow interface allows tests to provide a stub instead of
// constructing a full service with an in-memory database.
type LibraryQuerier interface {
	GetByID(ctx context.Context, id string) (*library.Library, error)
}

// SharedFSCheck centralizes the shared-filesystem detection that multiple
// fixers need. It wraps a LibraryQuerier and provides a single fail-closed
// method so that every fixer uses identical detection logic.
type SharedFSCheck struct {
	libs   LibraryQuerier
	logger *slog.Logger
}

// NewSharedFSCheck creates a SharedFSCheck. libs may be nil; IsShared will
// return true (fail-closed) when the querier is unavailable.
func NewSharedFSCheck(libs LibraryQuerier, logger *slog.Logger) *SharedFSCheck {
	return &SharedFSCheck{libs: libs, logger: logger}
}

// IsShared returns true if the artist's library shares its filesystem with a
// platform connection. Returns true (fail-closed) when:
//   - libs is nil (guard not configured)
//   - artist has no LibraryID
//   - DB lookup fails
//
// This prevents destructive operations (deleting images, renaming directories)
// when the system cannot confirm the filesystem is exclusively managed by
// Stillwater.
func (c *SharedFSCheck) IsShared(ctx context.Context, a *artist.Artist) bool {
	if c == nil || c.libs == nil || a == nil || a.LibraryID == "" {
		return true
	}

	lib, err := c.libs.GetByID(ctx, a.LibraryID)
	if err != nil {
		c.logger.Warn("library lookup failed; assuming shared filesystem",
			slog.String("library_id", a.LibraryID),
			slog.String("error", err.Error()))
		return true
	}

	return lib.IsSharedFS()
}
