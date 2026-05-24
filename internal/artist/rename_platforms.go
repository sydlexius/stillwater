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
// SyncRename MUST NOT return an error for a per-platform HTTP failure:
// per-platform failures land in the returned slice with Result ==
// PlatformRemapFailed and Error filled in. A non-nil error from SyncRename
// signals an enumeration-step failure (e.g. listing the artist's platform
// mappings from the DB failed); the rename has already committed on disk
// and in the artist row, so RenameDirectory does NOT surface it as an
// HTTP error. Instead it logs the error and, as a defensive belt-and-braces,
// synthesizes a single failed PlatformRemapResult entry when the
// implementation returned no results of its own so the HTTP response body
// always carries a concrete signal. The production publisher
// (publish.Publisher.SyncRename) self-synthesizes that entry too; the
// service-side synthesis exists for implementations that do not.
//
// See Service.RenameDirectory in service.go for the call site and
// Service.platformSyncer for where the configured implementation lives.
type PlatformRenameSyncer interface {
	SyncRename(ctx context.Context, artistID, oldPath, newPath string) ([]PlatformRemapResult, error)
}

// SetPlatformRenameSyncer attaches a syncer to the Service. Passing nil
// disables platform syncing: RenameDirectory then returns a nil platforms
// slice (not an empty []PlatformRemapResult{}). This is a setter rather
// than a constructor parameter so existing NewService / NewServiceWithRepos
// call sites do not have to thread a publisher; tests that do not care
// about platform sync leave the syncer unset and rely on the nil-slice
// shape to confirm the no-op path. See RenameDirectory in service.go for
// the conditional that gates on s.platformSyncer != nil.
func (s *Service) SetPlatformRenameSyncer(syncer PlatformRenameSyncer) {
	s.platformSyncer = syncer
}
