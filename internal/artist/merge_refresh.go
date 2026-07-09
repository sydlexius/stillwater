package artist

import "context"

// PlatformRefreshResult records the outcome of a post-merge refresh on one
// connection. Mirrors PlatformRemapResult (rename sync) so the two platform
// fan-outs report in the same shape. Error is non-empty only when Result is
// PlatformRemapFailed.
type PlatformRefreshResult struct {
	ConnectionID string
	Result       string
	Error        string
}

// PlatformMergeRefresher is what MergeAndReconcile calls after a successful
// merge (and any chained canonical rename) to reconcile connected platforms.
// The reconciliation differs by platform: on Emby/Jellyfin a full library scan
// both indexes the survivor's newly absorbed albums AND drops stale loser items
// whose on-disk directories were removed; on Lidarr there is no server-wide scan
// primitive, so only the survivor is re-read -- the loser's Lidarr artist is left
// pointing at the deleted folder and must be removed on the Lidarr side manually
// (broader loser eviction is deferred to #2318). Implemented by *publish.Publisher.
//
// connectionIDs is MergeResult.AffectedConnectionIDs (survivor + losers,
// captured before the loser rows were deleted). Best-effort: per-connection
// failures land in the returned slice with Result == PlatformRemapFailed;
// today the outer error is always nil.
type PlatformMergeRefresher interface {
	SyncMergeRefresh(ctx context.Context, survivorID string, connectionIDs []string) ([]PlatformRefreshResult, error)
}

// SetPlatformMergeRefresher attaches a refresher used by MergeAndReconcile.
// Passing nil (the zero value) disables post-merge platform refresh; the
// orchestrator then records a manual-refresh warning instead.
func (s *Service) SetPlatformMergeRefresher(r PlatformMergeRefresher) {
	s.mergeRefresher = r
}
