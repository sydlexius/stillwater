package artist

import "context"

// PlatformRemapResult records the outcome of a single per-connection path
// remap attempt after Service.RenameDirectory has moved the on-disk directory.
// One value per connection that has an artist_platform_ids row for the artist.
// Result is either "ok" or "failed"; on "failed" Error carries a user-safe
// summary suitable for surfacing in an HTTP response.
//
// The slice produced by RenameDirectory is best-effort: a single platform
// failure does NOT roll back the rename or short-circuit the remaining
// platforms, mirroring publish.Publisher.PushLocks' partial-failure pattern.
type PlatformRemapResult struct {
	ConnectionID string `json:"connection_id"`
	Result       string `json:"result"`
	Error        string `json:"error,omitempty"`
}

// Result values for PlatformRemapResult.Result. Constants live here so the
// HTTP handler, the syncer implementation, and tests all share one vocabulary
// (the same shape PushLocks settled on; see publish/publisher.go).
const (
	PlatformRemapOK     = "ok"
	PlatformRemapFailed = "failed"
)

// PlatformRenameSyncer is what Service.RenameDirectory calls after a
// successful on-disk + DB rename to re-issue the artist's path on every
// connected platform (Emby/Jellyfin/Lidarr). Implemented by
// publish.Publisher; the indirection keeps the artist package free of
// connection/HTTP-client imports.
//
// SyncRename MUST NOT return an error for a per-platform failure: failures
// land in the returned slice with Result == PlatformRemapFailed. A non-nil
// error from SyncRename indicates a problem looking up the artist's platform
// mappings (i.e. the DB read itself failed), which the caller surfaces as a
// 500 since the rename has already committed.
type PlatformRenameSyncer interface {
	SyncRename(ctx context.Context, artistID, oldPath, newPath string) ([]PlatformRemapResult, error)
}

// SetPlatformRenameSyncer attaches a syncer to the Service. Nil disables
// platform syncing (the rename then returns an empty platforms slice). This
// is a setter rather than a constructor parameter so existing NewService /
// NewServiceWithRepos call sites do not have to thread a publisher; tests
// that do not care about platform sync leave the syncer unset.
func (s *Service) SetPlatformRenameSyncer(syncer PlatformRenameSyncer) {
	s.platformSyncer = syncer
}
