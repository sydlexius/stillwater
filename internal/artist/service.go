package artist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

// ctxKey is the type for context value keys used by the artist package.
type ctxKey string

// sourceKey carries the history source through context so callers (e.g.
// refresh handlers) can tag changes with "provider:musicbrainz" etc. without
// changing method signatures.
const sourceKey ctxKey = "history_source"

// historyIDKey carries a pre-generated metadata change ID through context so
// callers (e.g. the revert handler) can deterministically locate the change
// row that an in-flight UpdateField/ClearField call will record. Without this,
// the handler would have to do a "fetch most recent change for field X" lookup
// after the mutation, which races against any other writer that touches the
// same field at the same instant.
const historyIDKey ctxKey = "history_change_id"

// ContextWithSource returns a child context that carries the given history
// source string. When the Service records metadata changes, it reads this
// value to populate the MetadataChange.Source field.
func ContextWithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey, source)
}

// sourceFromContext extracts the history source from ctx, defaulting to
// "manual" when no source has been set.
func sourceFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sourceKey).(string); ok && v != "" {
		return v
	}
	return "manual"
}

// ContextWithHistoryID returns a child context that pre-assigns the metadata
// change ID for history records written via this context. The HistoryService
// reads the value on every Record call and uses it as-is, so this context
// must only be used with code paths that write at most one history row;
// otherwise the second insert collides with the same primary key. Use it for
// single-mutation flows like the revert handler so the resulting change row
// can be fetched deterministically by ID instead of via a racy "most recent
// matching change" lookup.
func ContextWithHistoryID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, historyIDKey, id)
}

// HistoryIDFromContext extracts the pre-assigned change ID from ctx, returning
// the empty string when none is set. Exported so the HistoryService (in the
// same package) and tests can read it.
func HistoryIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(historyIDKey).(string); ok {
		return v
	}
	return ""
}

// trackableFields lists the metadata fields that are tracked by the history
// system when Update() is called. These correspond to the editable fields
// exposed by the field-level API.
var trackableFields = []string{
	"biography", "genres", "styles", "moods",
	"formed", "born", "disbanded", "died",
	"years_active", "type", "gender", "origin",
}

// HydrateOpts selects which side-table hydrations a Get*/batch call should
// perform. Fields default to false; the zero value yields "no hydration"
// which is useful for callers that only need the core Artist row (path
// existence, NFO presence, image-flag bookkeeping, etc.) and can save 3
// per-artist round-trips by skipping the side tables.
//
// The historical Get* methods on Service hydrate every side table by default
// for API back-compat. Pass a HydrateOpts value to opt into a leaner load:
//
//	a, err := svc.GetByID(ctx, id)                      // full hydration (default)
//	a, err := svc.GetByID(ctx, id, artist.HydrateOpts{}) // no hydration
//	a, err := svc.GetByID(ctx, id, artist.HydrateOpts{ProviderIDs: true})
//
// All-true is the legacy behavior; HydrateAll is a convenience preset for
// callers that want to make the choice explicit.
type HydrateOpts struct {
	// ProviderIDs hydrates the artist_provider_ids side table into the
	// Artist's MusicBrainzID, AudioDBID, DiscogsID, etc. fields.
	ProviderIDs bool
	// Images hydrates the artist_images side table into the Artist's
	// ThumbExists, FanartExists, *Placeholder, *Width/Height fields.
	Images bool
	// PrimaryLibrary hydrates the M:N artist_libraries membership into
	// Artist.LibraryID (earliest membership wins).
	PrimaryLibrary bool
}

// HydrateAll is the back-compat preset that turns on every hydration.
// The bare Get* methods (no opts) apply this preset implicitly so existing
// callers do not need to be updated.
var HydrateAll = HydrateOpts{
	ProviderIDs:    true,
	Images:         true,
	PrimaryLibrary: true,
}

// resolveHydrateOpts returns the effective HydrateOpts for a Get*/batch call.
// When no opts are supplied the caller gets HydrateAll (the legacy behavior
// so existing callers do not need to be touched). When at least one opts
// value is passed, only the first one is honored -- variadic is used purely
// as a back-compat shim so optional opts can be added without breaking
// existing callers.
func resolveHydrateOpts(opts []HydrateOpts) HydrateOpts {
	if len(opts) == 0 {
		return HydrateAll
	}
	return opts[0]
}

// Service provides artist and band member data operations.
type Service struct {
	artists      Repository
	providers    ProviderIDRepository
	members      MemberRepository
	aliases      AliasRepository
	images       ImageRepository
	platformIDs  PlatformIDRepository
	completeness CompletenessRepository
	history      *HistoryService
	mbSnapshots  MBSnapshotRepository
	memberships  MembershipRepository

	// renameMu serializes the destination-conflict check and the on-disk
	// rename in RenameDirectory so two concurrent rename requests targeting
	// the same parent cannot both pass their os.Lstat(newPath) check and
	// race into filesystem.RenameDirAtomic, which assumes dst does not
	// already exist. Held only across the Lstat+rename critical section,
	// not the surrounding validation or DB writes; rename is a rare,
	// user-driven operation, so the coarse single-mutex contention cost is
	// negligible.
	renameMu sync.Mutex
}

// SetHistoryService attaches a HistoryService to the artist Service so that
// metadata mutations automatically record change history. This is a setter
// rather than a constructor parameter to avoid breaking existing NewService
// and NewServiceWithRepos call sites.
//
// Must be called before the HTTP server starts accepting requests.
func (s *Service) SetHistoryService(h *HistoryService) {
	s.history = h
}

// SetMBSnapshotRepository attaches a MBSnapshotRepository to the artist Service
// for tracking last-known MusicBrainz field values. This is a setter rather than
// a constructor parameter to avoid breaking existing call sites.
func (s *Service) SetMBSnapshotRepository(repo MBSnapshotRepository) {
	s.mbSnapshots = repo
}

// UpsertMBSnapshots stores or updates MusicBrainz value snapshots for the given artist.
// No-op if no MBSnapshotRepository has been configured.
func (s *Service) UpsertMBSnapshots(ctx context.Context, artistID string, snapshots []MBSnapshot) error {
	if s.mbSnapshots == nil {
		return nil
	}
	return s.mbSnapshots.UpsertAll(ctx, artistID, snapshots)
}

// GetMBSnapshots returns the last-known MusicBrainz values for the given artist,
// keyed by field name. Returns an empty map if no snapshots exist or no repository
// is configured.
func (s *Service) GetMBSnapshots(ctx context.Context, artistID string) (map[string]MBSnapshot, error) {
	if s.mbSnapshots == nil {
		return make(map[string]MBSnapshot), nil
	}
	return s.mbSnapshots.GetForArtist(ctx, artistID)
}

// NewService creates an artist service backed by SQLite.
func NewService(db *sql.DB) *Service {
	return &Service{
		artists:      newSQLiteArtistRepo(db),
		providers:    newSQLiteProviderIDRepo(db),
		members:      newSQLiteMemberRepo(db),
		aliases:      newSQLiteAliasRepo(db),
		images:       newSQLiteImageRepo(db),
		platformIDs:  newSQLitePlatformIDRepo(db),
		completeness: newSQLiteCompletenessRepo(db),
		mbSnapshots:  newSQLiteMBSnapshotRepo(db),
		memberships:  newSQLiteMembershipRepo(db),
	}
}

// SetMembershipRepository attaches a MembershipRepository to the artist
// Service for the M:N artist-libraries surface. Setter form
// matches SetMBSnapshotRepository so existing NewServiceWithRepos callers
// (and their test fakes) keep working without a signature break.
func (s *Service) SetMembershipRepository(repo MembershipRepository) {
	s.memberships = repo
}

// NewServiceWithRepos creates an artist service using the provided repository
// implementations, enabling dependency injection for tests and alternative backends.
func NewServiceWithRepos(
	artists Repository,
	providers ProviderIDRepository,
	members MemberRepository,
	aliases AliasRepository,
	images ImageRepository,
	platformIDs PlatformIDRepository,
	completeness CompletenessRepository,
) *Service {
	return &Service{
		artists:      artists,
		providers:    providers,
		members:      members,
		aliases:      aliases,
		images:       images,
		platformIDs:  platformIDs,
		completeness: completeness,
	}
}

// NewDefaultRepos constructs the default SQLite-backed repository set used by
// NewService. It is exported so tests in sibling packages can wrap one or more
// repositories with a decorator (e.g. to inject a forced Update failure) while
// reusing the real implementations for the remaining interfaces, then build a
// Service via NewServiceWithRepos. Returning the interface types (not the
// concrete sqlite structs) keeps the call site honest about what the Service
// actually depends on.
//
//nolint:gocritic // tooManyResultsChecker: returns one entry per repo interface so tests can swap individual repos via NewServiceWithRepos; collapsing into a struct would force every test call site to thread fields.
func NewDefaultRepos(db *sql.DB) (
	Repository,
	ProviderIDRepository,
	MemberRepository,
	AliasRepository,
	ImageRepository,
	PlatformIDRepository,
	CompletenessRepository,
) {
	return newSQLiteArtistRepo(db),
		newSQLiteProviderIDRepo(db),
		newSQLiteMemberRepo(db),
		newSQLiteAliasRepo(db),
		newSQLiteImageRepo(db),
		newSQLitePlatformIDRepo(db),
		newSQLiteCompletenessRepo(db)
}

// Create inserts a new artist and persists its provider IDs and image metadata.
// If normalized data persistence fails, the artist row is deleted as a
// best-effort rollback (CASCADE handles child tables).
//
// When a.LibraryID is set, an artist_libraries membership is inserted
// alongside the artist row so the artist appears in per-library queries.
// The source is derived from the target library: a connection-backed
// library uses the connection type (emby / jellyfin / lidarr); otherwise
// the artist is recorded as filesystem-sourced.
//
// AddDerivingSource silently no-ops when the target library does not
// exist (its SELECT-driven INSERT yields zero rows in that case), so any
// error returned here is a real DB-level failure (locked, FK violation
// on the artist row, etc.) and is treated as fatal. Memberships are
// load-bearing under M:N -- an artist row without a corresponding
// membership disappears from per-library views -- so we roll the
// artist back rather than leaving a half-created record.
func (s *Service) Create(ctx context.Context, a *Artist) error {
	if err := s.artists.Create(ctx, a); err != nil {
		return err
	}
	if err := s.persistNormalized(ctx, a); err != nil {
		_ = s.artists.Delete(ctx, a.ID) // best-effort rollback
		return err
	}
	if err := s.recordInitialMembership(ctx, a); err != nil {
		_ = s.artists.Delete(ctx, a.ID) // keep create atomic for required data
		return fmt.Errorf("recording initial library membership: %w", err)
	}
	return nil
}

// recordInitialMembership inserts an artist_libraries row derived from
// a.LibraryID. The membership repo deduces the source from the target
// library (filesystem when no connection, otherwise the connection's
// type) and is a no-op when the library does not exist.
func (s *Service) recordInitialMembership(ctx context.Context, a *Artist) error {
	if s.memberships == nil || a.LibraryID == "" {
		return nil
	}
	return s.memberships.AddDerivingSource(ctx, a.ID, a.LibraryID)
}

// GetHealthStats returns aggregate health metrics for non-excluded artists.
// When libraryID is non-empty, only artists in that library are included.
func (s *Service) GetHealthStats(ctx context.Context, libraryID string) (HealthStatsResult, error) {
	return s.artists.HealthStats(ctx, libraryID)
}

// UpdateHealthScore sets only the health_score column for the given artist,
// avoiding a full row overwrite that could clobber concurrent mutations.
func (s *Service) UpdateHealthScore(ctx context.Context, id string, score float64) error {
	return s.artists.UpdateHealthScore(ctx, id, score)
}

// ListUnevaluatedIDs returns IDs of non-excluded artists that have never been evaluated
// (health_evaluated_at IS NULL), used by the bootstrap process to identify artists
// needing initial health score calculation.
func (s *Service) ListUnevaluatedIDs(ctx context.Context) ([]string, error) {
	return s.artists.ListUnevaluatedIDs(ctx)
}

// MarkDirty stamps dirty_since for one artist. The rule pipeline picks up
// artists whose dirty_since is newer than rules_evaluated_at on the next
// "Run Rules" invocation.
func (s *Service) MarkDirty(ctx context.Context, id string, ts time.Time) error {
	return s.artists.MarkDirty(ctx, id, ts)
}

// markDirtyBestEffort stamps dirty_since with the current UTC time and
// warn-logs any failure instead of propagating it. Called after every
// successful artist mutation (Update, UpdateField, ClearField) so the rule
// pipeline sees the change even when the ArtistUpdated event bus notification
// is dropped under backpressure. The best-effort contract is deliberate:
// the artist row has already been written, and the DirtySubscriber is still
// a secondary signal; failing the caller because of a dirty-mark write
// would invert error semantics relative to the main write.
func (s *Service) markDirtyBestEffort(ctx context.Context, id string) {
	if err := s.artists.MarkDirty(ctx, id, time.Now().UTC()); err != nil {
		slog.Warn("marking artist dirty after mutation",
			"artist_id", id, "error", err)
	}
}

// MarkAllDirty stamps dirty_since on every non-excluded, non-locked artist.
// Returns the number of rows affected. Called when a new rule is added so
// existing artists are scheduled for re-evaluation against it.
func (s *Service) MarkAllDirty(ctx context.Context, ts time.Time) (int64, error) {
	return s.artists.MarkAllDirty(ctx, ts)
}

// MarkRulesEvaluated stamps rules_evaluated_at for one artist after the
// rule pipeline finishes processing it.
func (s *Service) MarkRulesEvaluated(ctx context.Context, id string, ts time.Time) error {
	return s.artists.MarkRulesEvaluated(ctx, id, ts)
}

// ListDirtyIDs returns IDs of non-excluded, non-locked artists that need
// rule re-evaluation: those that have never been evaluated, or whose
// dirty_since is strictly after rules_evaluated_at.
func (s *Service) ListDirtyIDs(ctx context.Context) ([]string, error) {
	return s.artists.ListDirtyIDs(ctx)
}

// CountEligibleArtists returns the number of non-excluded, non-locked
// artists. Used as the denominator for incremental "Run Rules" progress
// reporting (e.g. "evaluating 12 of 800 (788 unchanged)").
func (s *Service) CountEligibleArtists(ctx context.Context) (int, error) {
	return s.artists.CountEligibleArtists(ctx)
}

// GetByID retrieves an artist by primary key. Without opts every side-table
// hydration runs (provider IDs, images, primary library) for API back-compat.
// Pass a HydrateOpts value to opt into a leaner load that skips one or more
// side-table queries; see HydrateOpts.
func (s *Service) GetByID(ctx context.Context, id string, opts ...HydrateOpts) (*Artist, error) {
	a, err := s.artists.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.applyHydration(ctx, a, resolveHydrateOpts(opts)); err != nil {
		return nil, err
	}
	return a, nil
}

// GetByMBID retrieves an artist by MusicBrainz ID. See GetByID for opts
// semantics. Returns nil, nil when no artist matches.
func (s *Service) GetByMBID(ctx context.Context, mbid string, opts ...HydrateOpts) (*Artist, error) {
	a, err := s.artists.GetByMBID(ctx, mbid)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.applyHydration(ctx, a, resolveHydrateOpts(opts)); err != nil {
		return nil, err
	}
	return a, nil
}

// GetByProviderID retrieves an artist by a provider-specific ID. See GetByID
// for opts semantics. Supported providers: "musicbrainz", "audiodb",
// "discogs", "wikidata", "deezer", "spotify".
func (s *Service) GetByProviderID(ctx context.Context, provider, id string, opts ...HydrateOpts) (*Artist, error) {
	a, err := s.providers.GetByProviderID(ctx, provider, id)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.applyHydration(ctx, a, resolveHydrateOpts(opts)); err != nil {
		return nil, err
	}
	return a, nil
}

// GetByName retrieves an artist by case-insensitive exact name match,
// without library scope. See GetByID for opts semantics.
// Returns nil, nil when no match is found.
func (s *Service) GetByName(ctx context.Context, name string, opts ...HydrateOpts) (*Artist, error) {
	a, err := s.artists.GetByName(ctx, name)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.applyHydration(ctx, a, resolveHydrateOpts(opts)); err != nil {
		return nil, err
	}
	return a, nil
}

// FindByMBIDOrNameUnscoped tries MBID first, then case-insensitive name,
// without library scope. See GetByID for opts semantics.
// Used by connection populate paths to dedupe across all libraries.
// Returns nil, nil when no match is found.
func (s *Service) FindByMBIDOrNameUnscoped(ctx context.Context, mbid, name string, opts ...HydrateOpts) (*Artist, error) {
	a, err := s.artists.FindByMBIDOrNameUnscoped(ctx, mbid, name)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.applyHydration(ctx, a, resolveHydrateOpts(opts)); err != nil {
		return nil, err
	}
	return a, nil
}

// AddLibraryMembership records that an artist is observed by the given
// library. Idempotent
func (s *Service) AddLibraryMembership(ctx context.Context, artistID, libraryID, source string) error {
	if s.memberships == nil {
		return nil
	}
	return s.memberships.Add(ctx, artistID, libraryID, source)
}

// RemoveLibraryMembership removes a single (artist, library) pair from the
// membership table
func (s *Service) RemoveLibraryMembership(ctx context.Context, artistID, libraryID string) error {
	if s.memberships == nil {
		return nil
	}
	return s.memberships.Remove(ctx, artistID, libraryID)
}

// LibrariesForArtist returns every library this artist is currently a
// member of
func (s *Service) LibrariesForArtist(ctx context.Context, artistID string) ([]LibraryMembership, error) {
	if s.memberships == nil {
		return nil, nil
	}
	return s.memberships.ListForArtist(ctx, artistID)
}

// CountLibrariesForArtist returns the number of libraries this artist is
// currently a member of. Used by the unlink path to decide whether to
// prune the artist after a library detachment
func (s *Service) CountLibrariesForArtist(ctx context.Context, artistID string) (int, error) {
	if s.memberships == nil {
		return 0, nil
	}
	return s.memberships.CountForArtist(ctx, artistID)
}

// GetByPath retrieves an artist by filesystem path. See GetByID for opts
// semantics.
func (s *Service) GetByPath(ctx context.Context, path string, opts ...HydrateOpts) (*Artist, error) {
	a, err := s.artists.GetByPath(ctx, path)
	if err != nil || a == nil {
		return a, err
	}
	if err := s.applyHydration(ctx, a, resolveHydrateOpts(opts)); err != nil {
		return nil, err
	}
	return a, nil
}

// applyHydration runs the selected side-table hydrations on a single Artist.
// Each branch is a separate DB round-trip; the AC for HydrateOpts depends on
// the zero value yielding exactly one query (the original Get* call) since
// every branch here is gated.
func (s *Service) applyHydration(ctx context.Context, a *Artist, opts HydrateOpts) error {
	if opts.ProviderIDs {
		if err := s.hydrateProviderIDs(ctx, a); err != nil {
			return err
		}
	}
	if opts.Images {
		if err := s.hydrateImages(ctx, a); err != nil {
			return err
		}
	}
	if opts.PrimaryLibrary {
		if err := s.hydratePrimaryLibrary(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// applyHydrationBatch runs the selected side-table hydrations on a slice of
// Artists, using the *Batch hydrate helpers so each enabled branch issues
// exactly one DB round-trip regardless of slice length. Mirrors the gating
// logic in applyHydration.
func (s *Service) applyHydrationBatch(ctx context.Context, artists []Artist, opts HydrateOpts) error {
	if len(artists) == 0 {
		return nil
	}
	if opts.ProviderIDs {
		if err := s.hydrateProviderIDsBatch(ctx, artists); err != nil {
			return err
		}
	}
	if opts.Images {
		if err := s.hydrateImagesBatch(ctx, artists); err != nil {
			return err
		}
	}
	if opts.PrimaryLibrary {
		if err := s.hydratePrimaryLibrariesBatch(ctx, artists); err != nil {
			return err
		}
	}
	return nil
}

// List returns a paginated list of artists and the total count, with provider IDs
// and image metadata hydrated.
func (s *Service) List(ctx context.Context, params ListParams) ([]Artist, int, error) {
	artists, total, err := s.artists.List(ctx, params)
	if err != nil {
		return nil, 0, err
	}
	if err := s.hydrateProviderIDsBatch(ctx, artists); err != nil {
		return nil, 0, err
	}
	if err := s.hydrateImagesBatch(ctx, artists); err != nil {
		return nil, 0, err
	}
	if err := s.hydratePrimaryLibrariesBatch(ctx, artists); err != nil {
		return nil, 0, err
	}
	return artists, total, nil
}

// Count returns the total number of artists matching the given filters without
// fetching or hydrating any row data. Prefer this over List when only the count
// is needed (e.g., sidebar badge).
func (s *Service) Count(ctx context.Context, params CountParams) (int, error) {
	return s.artists.Count(ctx, params)
}

// Update modifies an existing artist and persists its provider IDs and image metadata.
// Note: if provider or image persistence fails after the artist row is updated,
// we cannot rollback the artist update (the old data is already overwritten).
// In practice this is acceptable because the normalized tables use
// delete-then-insert which is idempotent, and the next Update call will retry.
//
// Update (along with UpdateField and ClearField) calls markDirtyBestEffort
// after the write succeeds so the rule pipeline sees the mutation even when
// the ArtistUpdated event bus notification is dropped under backpressure.
// The event-bus DirtySubscriber remains as a secondary signal for mutation
// paths that bypass Service entirely (watcher, bulk_executor, image bridge).
//
// When a HistoryService is attached, Update diffs every trackable field
// between the old and new artist values and records a MetadataChange for
// each field that changed. History recording is best-effort: failures are
// logged but do not cause the update to fail.
func (s *Service) Update(ctx context.Context, a *Artist) error {
	return s.update(ctx, a, true)
}

// UpdateAfterRuleEvaluation persists the artist without stamping dirty_since.
// Intended for the rule pipeline's own writeback (health score, fixer-applied
// fields) at the end of an evaluation pass: the pipeline is about to stamp
// rules_evaluated_at and the artist is by definition freshly evaluated, so
// marking it dirty would race the stamp.
//
// Concretely, both dirty_since and rules_evaluated_at are stored via
// time.RFC3339 (second precision). The walker captures startedAt before fn
// runs and uses it as the stamp. If markDirtyBestEffort inside a regular
// Update happened to cross a wall-clock second boundary relative to
// startedAt, dirty_since would land one second after rules_evaluated_at and
// the artist would re-appear in ListDirtyIDs immediately, flaking the next
// scheduled sweep. Skipping the dirty mark on the pipeline's self-writeback
// closes that race without weakening the protection for external mutations:
// those still go through Update (or the event bus DirtySubscriber) and get
// dirty_since stamped normally.
func (s *Service) UpdateAfterRuleEvaluation(ctx context.Context, a *Artist) error {
	return s.update(ctx, a, false)
}

func (s *Service) update(ctx context.Context, a *Artist, markDirty bool) error {
	// Snapshot the old state before writing, so we can diff after the update.
	var old *Artist
	if s.history != nil {
		var err error
		old, err = s.artists.GetByID(ctx, a.ID)
		if err != nil {
			// Artist may not exist yet (first insert via Update), or DB error.
			// Either way, skip history recording rather than blocking the update.
			slog.Warn("history: could not fetch old artist for diff",
				"artist_id", a.ID, "error", err)
			old = nil
		}
	}

	if err := s.artists.Update(ctx, a); err != nil {
		return err
	}

	if markDirty {
		s.markDirtyBestEffort(ctx, a.ID)
	}

	// Record field-level changes after the successful update.
	if s.history != nil && old != nil {
		source := sourceFromContext(ctx)
		for _, field := range trackableFields {
			oldVal := FieldValueFromArtist(old, field)
			newVal := FieldValueFromArtist(a, field)
			if oldVal != newVal {
				if err := s.history.Record(ctx, a.ID, field, oldVal, newVal, source); err != nil {
					slog.Warn("history: failed to record change",
						"artist_id", a.ID, "field", field, "error", err)
				}
			}
		}
	}

	return s.persistNormalized(ctx, a)
}

// Errors returned by RenameDirectory. Callers (HTTP handlers, tests) inspect
// these to distinguish bad-input cases from server-side failures so they can
// surface appropriate status codes and messages without parsing error strings.
var (
	// ErrRenameInvalidName indicates the requested directory name is empty,
	// contains a path separator, or resolves to "." or "..". The handler
	// translates this to HTTP 400.
	ErrRenameInvalidName = errors.New("invalid directory name")
	// ErrRenameNoPath indicates the artist has no on-disk path to rename
	// (e.g. it was created via a virtual library). HTTP 409.
	ErrRenameNoPath = errors.New("artist has no filesystem path")
	// ErrRenameLocked indicates the artist is locked, so automated and
	// destructive operations are skipped. HTTP 409.
	ErrRenameLocked = errors.New("artist is locked")
	// ErrRenameDestExists indicates the target directory already exists, so
	// the rename would clobber another artist's directory. HTTP 409.
	ErrRenameDestExists = errors.New("destination directory already exists")
	// ErrRenameNoChange indicates the requested name matches the current
	// directory name, so there is nothing to do. HTTP 400.
	ErrRenameNoChange = errors.New("new directory name matches current")
)

// RenameDirectory renames the artist's on-disk directory to newDirName and
// persists the new path to the database. This is intentionally decoupled from
// metadata edits: editing an artist's display name only touches the DB row
// and NFO file, while a directory rename is a separate, explicit action that
// can break platform mappings (Emby/Jellyfin item-to-path) and must be
// initiated by the user via a dedicated endpoint.
//
// newDirName must be a single path segment (no separators) and may not be "."
// or "..". The new path is computed by replacing the leaf of the artist's
// current Path with newDirName, preserving the parent directory.
//
// On success, the artist row's path column is updated to the new path. The
// in-memory Artist passed in to the caller is not mutated; callers should
// re-fetch via GetByID if they need the updated value.
//
// Side effects intentionally NOT performed here (caller's responsibility):
//   - Re-issuing platform-id mappings on Emby/Jellyfin so connected platforms
//     pick up the new directory. The API handler (or a future workflow)
//     drives that step.
//   - Rule re-evaluation. The caller's normal Update path stamps dirty_since;
//     for a path-only change we do the same so the rule pipeline picks it up
//     on the next sweep.
func (s *Service) RenameDirectory(ctx context.Context, artistID, newDirName string) (newPath string, err error) {
	newDirName = strings.TrimSpace(newDirName)
	if newDirName == "" || newDirName == "." || newDirName == ".." {
		return "", ErrRenameInvalidName
	}
	// Reject any path separator (forward or back slash) so callers cannot
	// escape the parent directory by smuggling a relative path through.
	if strings.ContainsAny(newDirName, `/\`) {
		return "", ErrRenameInvalidName
	}
	// filepath.IsLocal is the canonical path-traversal sanitizer (Go 1.20+):
	// it rejects absolute paths, paths containing "..", and reserved Windows
	// names. The literal and separator checks above already cover the cases
	// we care about, but calling IsLocal here clears the CodeQL taint flow
	// from this user input through filesystem.RenameDirAtomic into the
	// downstream os.Stat / os.Rename / copyFile sinks.
	if !filepath.IsLocal(newDirName) {
		return "", ErrRenameInvalidName
	}

	// Path-only rename: only Locked, Path, and Name are read from the
	// loaded artist, and persistence goes through UpdatePath (single-column
	// UPDATE), not s.update + persistNormalized. The repo-level lookup
	// returns the same ErrNotFound wrapping as Service.GetByID and skips
	// the unnecessary provider/image hydration, removing two extra DB
	// reads and two failure points per rename.
	a, err := s.artists.GetByID(ctx, artistID)
	if err != nil {
		return "", err
	}
	if a.Locked {
		return "", ErrRenameLocked
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", ErrRenameNoPath
	}

	parent := filepath.Dir(a.Path)
	newPath = filepath.Clean(filepath.Join(parent, newDirName))

	// Defense-in-depth: confirm the joined path is still a direct child of
	// parent. The combined input checks above (literal "."/".." reject and
	// the separator reject via strings.ContainsAny) should already prevent
	// any newDirName that could escape, but encoding the invariant
	// explicitly here satisfies the path-traversal lint and guards against
	// any future regression of those input-validation steps.
	if filepath.Dir(newPath) != parent {
		return "", ErrRenameInvalidName
	}

	// Short-circuit when the names match exactly. We do not attempt a
	// Unicode-equivalence check here (the rule fixer does that for the
	// canonical-name workflow); this endpoint is user-driven and the user
	// asked for an exact name.
	if newPath == a.Path {
		return "", ErrRenameNoChange
	}

	// Refuse to clobber an existing directory. The fixer does the same
	// safety check; mirror it here so the explicit-rename endpoint is no
	// looser than the rule-engine path. Use Lstat (not Stat) so a dangling
	// symlink at newPath still trips the conflict guard: Stat follows the
	// link and reports IsNotExist for a broken target, which would let us
	// silently rename over the user's symlink. Same pattern used by
	// internal/filesystem/symlink.go's existence checks.
	//
	// Hold renameMu across the entire Lstat -> RenameDirAtomic -> UpdatePath
	// -> rollback sequence. The lock cannot be released between the on-disk
	// rename and the DB write: once oldPath is empty, a concurrent rename
	// could claim it as its own destination, and our rollback (newPath ->
	// oldPath) would then either fail with "destination exists" or clobber
	// the concurrent operation. Holding the mutex across the DB write also
	// keeps two callers from interleaving partial state for the same parent.
	//
	// The closure pins the lock scope so any future early-return added
	// inside it cannot deadlock the service. External processes that mutate
	// newPath without taking this lock are still possible, but the in-process
	// path is the realistic concurrency vector. UpdatePath is a single-column
	// SQLite write, so widening the critical section to cover it does not
	// add meaningful contention given how rare user-driven renames are.
	oldPath := a.Path
	if err := func() error {
		s.renameMu.Lock()
		defer s.renameMu.Unlock()

		if _, statErr := os.Lstat(newPath); statErr == nil {
			return ErrRenameDestExists
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("checking destination %q: %w", newPath, statErr)
		}

		if err := filesystem.RenameDirAtomic(oldPath, newPath); err != nil {
			return fmt.Errorf("renaming %q to %q: %w", oldPath, newPath, err)
		}

		// Persist the new path with a single-column UPDATE so a concurrent
		// edit to any other field (Name, Locked, etc.) landing between our
		// hydrated load and this write is not silently reverted. A full-row
		// s.artists.Update would rewrite every column from the snapshot we
		// loaded earlier, which could clobber concurrent mutations;
		// UpdatePath touches only artists.path (and updated_at). We also
		// avoid s.update() entirely so persistNormalized is not invoked
		// here -- a path-only rename has no normalized state to refresh,
		// and skipping it keeps DB and FS in lockstep on the rollback.
		if updErr := s.artists.UpdatePath(ctx, a.ID, newPath); updErr != nil {
			// The on-disk rename succeeded but the DB write failed.
			// Attempt to roll the directory back so the next scan does
			// not find a directory whose path no longer matches the
			// artist row. A failed rollback is logged but not returned:
			// the original error is what the caller needs to surface,
			// and the operator can reconcile by re-running a scan.
			if rollbackErr := filesystem.RenameDirAtomic(newPath, oldPath); rollbackErr != nil {
				slog.Error("rename directory: db update failed and rollback also failed",
					"artist_id", artistID,
					"new_path", newPath,
					"old_path", oldPath,
					"db_error", updErr,
					"rollback_error", rollbackErr)
			}
			return fmt.Errorf("persisting renamed path: %w", updErr)
		}
		return nil
	}(); err != nil {
		return "", err
	}
	s.markDirtyBestEffort(ctx, a.ID)

	slog.Info("renamed artist directory",
		"artist_id", artistID,
		"artist", a.Name,
		"new_path", newPath)

	return newPath, nil
}

// persistNormalized writes provider IDs and image metadata to the normalized
// tables for the given artist.
func (s *Service) persistNormalized(ctx context.Context, a *Artist) error {
	if err := s.providers.UpsertAll(ctx, a.ID, extractProviderIDs(a)); err != nil {
		return err
	}
	return s.images.UpsertAll(ctx, a.ID, extractImageMetadata(a))
}

// UpdateImageProvenance updates the provenance-related fields (phash, source,
// file_format, last_written_at) on an existing image row without touching display
// fields like exists_flag, low_res, or placeholder. This is called after an image
// save to record evidence of what was written and when.
func (s *Service) UpdateImageProvenance(ctx context.Context, artistID, imageType string, slotIndex int, phash, source, fileFormat, lastWrittenAt string) error {
	return s.images.UpdateProvenance(ctx, artistID, imageType, slotIndex, phash, source, fileFormat, lastWrittenAt)
}

// ClearImageFlag sets the exists flag to false for a single image slot.
// This is called when a request to serve an image discovers the file is
// missing on disk, clearing the stale flag so subsequent UI renders show a
// placeholder instead of a broken image tag.
func (s *Service) ClearImageFlag(ctx context.Context, artistID, imageType string, slotIndex int) error {
	return s.images.ClearExistsFlag(ctx, artistID, imageType, slotIndex)
}

// ReconcileImages converges the artist_images registry to match the image
// fields on the provided Artist. The caller is responsible for populating the
// Artist's *Exists, *LowRes, *Placeholder, *Width, *Height, and FanartCount
// fields from filesystem-truth (e.g. the scanner's detectFiles output) before
// calling this method.
//
// Unlike Update(), ReconcileImages performs ONLY the artist_images upsert and
// stale-row removal, without touching any other artist columns or normalized
// tables. The scanner calls this on every directory visit so the registry
// stays in sync with disk even when the artist row's flags would otherwise
// show "no change" (issue #1225). Idempotent: replaying with the same Artist
// is a no-op.
//
// Returns true when the registry was actually mutated so the caller can
// mirror Update()'s ArtistUpdated event fanout only on real repairs.
func (s *Service) ReconcileImages(ctx context.Context, a *Artist) (bool, error) {
	if a == nil || a.ID == "" {
		return false, fmt.Errorf("ReconcileImages: artist ID is required")
	}
	desired := extractImageMetadata(a)
	current, err := s.images.GetForArtist(ctx, a.ID)
	if err != nil {
		return false, fmt.Errorf("loading current images for reconciliation: %w", err)
	}
	if !imageRegistryDrift(current, desired) {
		return false, nil
	}
	if err := s.images.UpsertAll(ctx, a.ID, desired); err != nil {
		return false, err
	}
	return true, nil
}

// imageRegistryDrift reports whether the writable fields of `desired` differ
// from `current`. Provenance columns (PHash, Source, FileFormat,
// LastWrittenAt) are intentionally ignored because UpsertAll preserves them
// and they cannot drift from filesystem detection alone.
func imageRegistryDrift(current, desired []ArtistImage) bool {
	type key struct {
		imageType string
		slot      int
	}
	type row struct {
		exists      bool
		lowRes      bool
		placeholder string
		width       int
		height      int
	}
	index := func(rows []ArtistImage) map[key]row {
		out := make(map[key]row, len(rows))
		for i := range rows {
			r := &rows[i]
			out[key{r.ImageType, r.SlotIndex}] = row{
				exists:      r.Exists,
				lowRes:      r.LowRes,
				placeholder: r.Placeholder,
				width:       r.Width,
				height:      r.Height,
			}
		}
		return out
	}
	c, d := index(current), index(desired)
	if len(c) != len(d) {
		return true
	}
	for k, want := range d {
		got, ok := c[k]
		if !ok || got != want {
			return true
		}
	}
	return false
}

// IsEditableField reports whether the given field name can be updated via
// the field-level API. This includes both direct-column fields (in
// fieldColumnMap) and provider-ID fields (in providerFieldMap).
func IsEditableField(field string) bool {
	if _, ok := fieldColumnMap[field]; ok {
		return true
	}
	_, ok := providerFieldMap[field]
	return ok
}

// IsProviderIDField reports whether the given field name maps to the
// artist_provider_ids normalized table rather than a direct artists column.
func IsProviderIDField(field string) bool {
	_, ok := providerFieldMap[field]
	return ok
}

// UpdateField updates a single metadata field on an artist record.
// For slice fields (genres, styles, moods), the value is a comma-separated
// string that gets marshaled to a JSON array for storage.
//
// When a HistoryService is attached, the old value is read before the update
// and a MetadataChange is recorded if the value changed. History recording is
// best-effort and will not cause the update to fail.
func (s *Service) UpdateField(ctx context.Context, id, field, value string) error {
	// Capture old value before the mutation so we can record the diff.
	// Track whether the read succeeded so we don't fabricate history from
	// an unknown old state if GetByID fails transiently.
	var oldValue string
	oldKnown := false
	if s.history != nil {
		a, err := s.artists.GetByID(ctx, id)
		if err != nil {
			slog.Warn("history: could not fetch artist for UpdateField diff",
				"artist_id", id, "field", field, "error", err)
		} else {
			oldValue = FieldValueFromArtist(a, field)
			oldKnown = true
		}
	}

	if err := s.artists.UpdateField(ctx, id, field, value); err != nil {
		return err
	}

	s.markDirtyBestEffort(ctx, id)

	// Record the change by comparing normalized representations. Re-fetch
	// after the mutation so both old and new use FieldValueFromArtist, avoiding
	// format mismatches for slice fields (e.g. "Rock, Alternative" vs "Rock,Alternative").
	if s.history != nil && oldKnown {
		newA, err := s.artists.GetByID(ctx, id)
		if err != nil {
			slog.Warn("history: could not fetch artist after UpdateField",
				"artist_id", id, "field", field, "error", err)
		} else {
			newValue := FieldValueFromArtist(newA, field)
			if oldValue != newValue {
				source := sourceFromContext(ctx)
				if err := s.history.Record(ctx, id, field, oldValue, newValue, source); err != nil {
					slog.Warn("history: failed to record UpdateField change",
						"artist_id", id, "field", field, "error", err)
				}
			}
		}
	}

	return nil
}

// ClearField sets a single metadata field to its zero value.
//
// When a HistoryService is attached, the old value is read before clearing
// and a MetadataChange is recorded if the field was non-empty. History
// recording is best-effort and will not cause the clear to fail.
func (s *Service) ClearField(ctx context.Context, id, field string) error {
	// Capture old value before clearing so we can record the diff.
	var oldValue string
	if s.history != nil {
		a, err := s.artists.GetByID(ctx, id)
		if err != nil {
			slog.Warn("history: could not fetch artist for ClearField diff",
				"artist_id", id, "field", field, "error", err)
		} else {
			oldValue = FieldValueFromArtist(a, field)
		}
	}

	if err := s.artists.ClearField(ctx, id, field); err != nil {
		return err
	}

	s.markDirtyBestEffort(ctx, id)

	// Record the change only if the field was non-empty before clearing.
	// Use FieldValueFromArtist on the post-clear state for consistent representation.
	if s.history != nil && oldValue != "" {
		newA, err := s.artists.GetByID(ctx, id)
		if err != nil {
			slog.Warn("history: could not fetch artist after ClearField",
				"artist_id", id, "field", field, "error", err)
		} else {
			newValue := FieldValueFromArtist(newA, field)
			if oldValue != newValue {
				source := sourceFromContext(ctx)
				if err := s.history.Record(ctx, id, field, oldValue, newValue, source); err != nil {
					slog.Warn("history: failed to record ClearField change",
						"artist_id", id, "field", field, "error", err)
				}
			}
		}
	}

	return nil
}

// UpdateProviderField sets a single provider ID field (musicbrainz_id,
// audiodb_id, discogs_id, wikidata_id, or deezer_id) on the artist.
// It re-fetches the artist, applies the field update, and calls Update so
// that all provider IDs in the normalized table are written consistently.
func (s *Service) UpdateProviderField(ctx context.Context, id, field, value string) error {
	providerName, ok := providerFieldMap[field]
	if !ok {
		return fmt.Errorf("unknown provider field: %s", field)
	}

	a, err := s.GetByID(ctx, id)
	if err != nil {
		return err
	}

	// Apply the field update to the in-memory struct.
	applyProviderFieldToArtist(a, providerName, value)

	return s.Update(ctx, a)
}

// ClearProviderField removes a single provider ID field by setting it to
// empty string. It re-fetches the artist and calls Update so that the
// normalized artist_provider_ids table is kept consistent.
func (s *Service) ClearProviderField(ctx context.Context, id, field string) error {
	return s.UpdateProviderField(ctx, id, field, "")
}

// applyProviderFieldToArtist applies a single provider ID value to the
// appropriate field of the Artist struct. The providerName argument must be
// the canonical provider key (e.g. "musicbrainz", "audiodb"), not the API
// field name.
func applyProviderFieldToArtist(a *Artist, providerName, value string) {
	switch providerName {
	case "musicbrainz":
		a.MusicBrainzID = value
	case "audiodb":
		a.AudioDBID = value
	case "discogs":
		a.DiscogsID = value
	case "wikidata":
		a.WikidataID = value
	case "deezer":
		a.DeezerID = value
	}
}

// ValidateFieldUpdate returns a non-nil error when the field value is
// invalid. Validation rules:
// - "name" must not be empty or whitespace-only.
// - "musicbrainz_id" must be a valid UUID (or empty, which clears the ID).
//
// All other fields are accepted as-is (free-form text).
func ValidateFieldUpdate(field, value string) error {
	switch field {
	case "name":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("name cannot be empty")
		}
	case "musicbrainz_id":
		if value != "" && !isValidMBID(value) {
			return fmt.Errorf("invalid MusicBrainz ID format (expected UUID)")
		}
	}
	return nil
}

// isValidMBID reports whether s is a syntactically valid UUID, as used by
// MusicBrainz for all entity identifiers.
func isValidMBID(s string) bool {
	// A MusicBrainz UUID has the form xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.
	// We validate the format by checking length and character set rather than
	// importing an external UUID package, keeping this pure-stdlib.
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// FieldValueFromArtist extracts a single field's value from an Artist struct.
// For string fields returns the value directly; for slice fields returns
// the comma-joined representation.
func FieldValueFromArtist(a *Artist, field string) string {
	switch field {
	case "biography":
		return a.Biography
	case "genres":
		return strings.Join(a.Genres, ", ")
	case "styles":
		return strings.Join(a.Styles, ", ")
	case "moods":
		return strings.Join(a.Moods, ", ")
	case "formed":
		return a.Formed
	case "born":
		return a.Born
	case "disbanded":
		return a.Disbanded
	case "died":
		return a.Died
	case "years_active":
		return a.YearsActive
	case "type":
		return a.Type
	case "gender":
		return a.Gender
	case "origin":
		return a.Origin
	case "name":
		return a.Name
	case "sort_name":
		return a.SortName
	case "disambiguation":
		return a.Disambiguation
	case "musicbrainz_id":
		return a.MusicBrainzID
	case "audiodb_id":
		return a.AudioDBID
	case "discogs_id":
		return a.DiscogsID
	case "wikidata_id":
		return a.WikidataID
	case "deezer_id":
		return a.DeezerID
	default:
		return ""
	}
}

// SliceFieldFromArtist extracts a slice field's values from an Artist struct.
func SliceFieldFromArtist(a *Artist, field string) []string {
	switch field {
	case "genres":
		return a.Genres
	case "styles":
		return a.Styles
	case "moods":
		return a.Moods
	default:
		return nil
	}
}

// IsSliceField reports whether the given field stores a JSON array.
func IsSliceField(field string) bool {
	return sliceFields[field]
}

// UpdateProviderFetchedAt records when a provider ID fetch was last attempted.
// The provider argument must be a known provider name (e.g. "fanarttv", "audiodb").
func (s *Service) UpdateProviderFetchedAt(ctx context.Context, artistID, prov string) error {
	return s.providers.UpdateProviderFetchedAt(ctx, artistID, prov)
}

// Delete removes an artist by ID. Cascade deletes related rows.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.artists.Delete(ctx, id)
}

// Search finds artists by name substring match, with provider IDs and image metadata hydrated.
func (s *Service) Search(ctx context.Context, query string) ([]Artist, error) {
	artists, err := s.artists.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateProviderIDsBatch(ctx, artists); err != nil {
		return nil, err
	}
	if err := s.hydrateImagesBatch(ctx, artists); err != nil {
		return nil, err
	}
	if err := s.hydratePrimaryLibrariesBatch(ctx, artists); err != nil {
		return nil, err
	}
	return artists, nil
}

// validLockSources enumerates the allowed values for lock_source.
var validLockSources = map[string]bool{
	"user":     true,
	"imported": true,
}

// Lock sets the metadata lock on an artist with the given source ("user" or "imported").
// When locked, automated operations (rule fixers, metadata fetchers, image operations)
// skip the artist. Manual edits remain allowed.
func (s *Service) Lock(ctx context.Context, id, source string) error {
	if !validLockSources[source] {
		return fmt.Errorf("invalid lock source %q: must be \"user\" or \"imported\"", source)
	}
	return s.artists.SetLock(ctx, id, true, source)
}

// Unlock removes the metadata lock from an artist.
func (s *Service) Unlock(ctx context.Context, id string) error {
	return s.artists.SetLock(ctx, id, false, "")
}

// SetLockedFields replaces the full set of locked field names for an artist.
// Empty slice clears all field locks.
func (s *Service) SetLockedFields(ctx context.Context, id string, fields []string) error {
	return s.artists.SetLockedFields(ctx, id, fields)
}

// AddLockedField appends a single field to the artist's locked-field set.
// Existing entries are preserved; duplicates are elided by the repository's
// normalization step.
func (s *Service) AddLockedField(ctx context.Context, id, field string) error {
	a, err := s.artists.GetByID(ctx, id)
	if err != nil {
		return err
	}
	fields := append([]string{}, a.LockedFields...)
	fields = append(fields, field)
	return s.artists.SetLockedFields(ctx, id, fields)
}

// RemoveLockedField removes a single field from the artist's locked-field set.
// Missing entries are silently ignored.
func (s *Service) RemoveLockedField(ctx context.Context, id, field string) error {
	a, err := s.artists.GetByID(ctx, id)
	if err != nil {
		return err
	}
	target := strings.TrimSpace(field)
	kept := make([]string, 0, len(a.LockedFields))
	for _, f := range a.LockedFields {
		if strings.EqualFold(f, target) {
			continue
		}
		kept = append(kept, f)
	}
	return s.artists.SetLockedFields(ctx, id, kept)
}

// UpsertImage writes or updates a single image row. Exposed primarily for
// tests and tooling; production code paths use UpsertAll via scanning flows.
func (s *Service) UpsertImage(ctx context.Context, img *ArtistImage) error {
	return s.images.Upsert(ctx, img)
}

// SetImageLock toggles the lock flag on a single image row for an artist.
// The imageID must belong to the given artist; callers should verify ownership
// before invoking this method.
func (s *Service) SetImageLock(ctx context.Context, imageID string, locked bool) error {
	return s.images.SetLock(ctx, imageID, locked)
}

// IsFieldLocked returns true when the artist has the given field marked as
// locked. Comparison is case-insensitive and ignores leading/trailing
// whitespace on the input.
func (s *Service) IsFieldLocked(a *Artist, field string) bool {
	if a == nil {
		return false
	}
	target := strings.TrimSpace(field)
	for _, f := range a.LockedFields {
		if strings.EqualFold(f, target) {
			return true
		}
	}
	return false
}

// ListMembersByArtistID returns all band members for an artist, ordered by sort_order.
func (s *Service) ListMembersByArtistID(ctx context.Context, artistID string) ([]BandMember, error) {
	return s.members.ListByArtistID(ctx, artistID)
}

// CreateMember inserts a new band member.
func (s *Service) CreateMember(ctx context.Context, m *BandMember) error {
	return s.members.Create(ctx, m)
}

// DeleteMember removes a band member by ID.
func (s *Service) DeleteMember(ctx context.Context, id string) error {
	return s.members.Delete(ctx, id)
}

// DeleteMembersByArtistID removes all band members for an artist.
func (s *Service) DeleteMembersByArtistID(ctx context.Context, artistID string) error {
	return s.members.DeleteByArtistID(ctx, artistID)
}

// UpsertMembers replaces all band members for an artist with the given list.
func (s *Service) UpsertMembers(ctx context.Context, artistID string, members []BandMember) error {
	return s.members.Upsert(ctx, artistID, members)
}

// AddAlias adds an alias for an artist.
func (s *Service) AddAlias(ctx context.Context, artistID, alias, source string) (*Alias, error) {
	if alias == "" {
		return nil, fmt.Errorf("alias is required")
	}

	// Check artist exists
	_, err := s.artists.GetByID(ctx, artistID)
	if err != nil {
		return nil, err
	}

	a := &Alias{
		ArtistID: artistID,
		Alias:    alias,
		Source:   source,
	}

	if err := s.aliases.Create(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// RemoveAlias removes an alias by ID.
func (s *Service) RemoveAlias(ctx context.Context, aliasID string) error {
	return s.aliases.Delete(ctx, aliasID)
}

// ListAliases returns all aliases for an artist.
func (s *Service) ListAliases(ctx context.Context, artistID string) ([]Alias, error) {
	return s.aliases.ListByArtistID(ctx, artistID)
}

// SearchWithAliases searches artists by name or alias text, with provider IDs
// and image metadata hydrated.
func (s *Service) SearchWithAliases(ctx context.Context, query string) ([]Artist, error) {
	artists, err := s.aliases.SearchWithAliases(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateProviderIDsBatch(ctx, artists); err != nil {
		return nil, err
	}
	if err := s.hydrateImagesBatch(ctx, artists); err != nil {
		return nil, err
	}
	if err := s.hydratePrimaryLibrariesBatch(ctx, artists); err != nil {
		return nil, err
	}
	return artists, nil
}

// FindDuplicates returns groups of artists that appear to be duplicates.
// Detection is based on shared aliases, matching MBIDs, or similar names.
func (s *Service) FindDuplicates(ctx context.Context) ([]DuplicateGroup, error) {
	var groups []DuplicateGroup

	mbidGroups, err := s.aliases.FindMBIDDuplicates(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding MBID duplicates: %w", err)
	}
	groups = append(groups, mbidGroups...)

	aliasGroups, err := s.aliases.FindAliasDuplicates(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding alias duplicates: %w", err)
	}
	groups = append(groups, aliasGroups...)

	if err := s.hydrateDuplicateGroups(ctx, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// hydrateDuplicateGroups loads provider IDs and image metadata for all artists
// across all duplicate groups in two bulk queries (one for providers, one for images),
// then distributes the results back to each group's artists.
func (s *Service) hydrateDuplicateGroups(ctx context.Context, groups []DuplicateGroup) error {
	// Collect all unique artist IDs across every group.
	seen := make(map[string]struct{})
	for gi := range groups {
		g := &groups[gi]
		for ai := range g.Artists {
			seen[g.Artists[ai].ID] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	allIDs := make([]string, 0, len(seen))
	for id := range seen {
		allIDs = append(allIDs, id)
	}

	provMap, err := s.providers.GetForArtists(ctx, allIDs)
	if err != nil {
		return fmt.Errorf("bulk hydrating provider IDs for duplicates: %w", err)
	}
	imgMap, err := s.images.GetForArtists(ctx, allIDs)
	if err != nil {
		return fmt.Errorf("bulk hydrating images for duplicates: %w", err)
	}

	for gi := range groups {
		for ai := range groups[gi].Artists {
			a := &groups[gi].Artists[ai]
			applyProviderIDs(a, provMap[a.ID])
			applyImageMetadata(a, imgMap[a.ID])
		}
	}
	return nil
}

// SetPlatformID stores or updates the platform artist ID for an artist on a connection.
func (s *Service) SetPlatformID(ctx context.Context, artistID, connectionID, platformArtistID string) error {
	if artistID == "" || connectionID == "" || platformArtistID == "" {
		return fmt.Errorf("artist_id, connection_id, and platform_artist_id are required")
	}
	return s.platformIDs.Set(ctx, artistID, connectionID, platformArtistID)
}

// GetPlatformID retrieves the platform artist ID for an artist on a specific connection.
// If no mapping exists, it returns an empty string and a nil error.
func (s *Service) GetPlatformID(ctx context.Context, artistID, connectionID string) (string, error) {
	return s.platformIDs.Get(ctx, artistID, connectionID)
}

// GetPlatformIDs returns all platform artist IDs for an artist across all connections.
func (s *Service) GetPlatformIDs(ctx context.Context, artistID string) ([]PlatformID, error) {
	return s.platformIDs.GetAll(ctx, artistID)
}

// DeletePlatformID removes the platform artist ID mapping for an artist on a connection.
func (s *Service) DeletePlatformID(ctx context.Context, artistID, connectionID string) error {
	return s.platformIDs.Delete(ctx, artistID, connectionID)
}

// DeletePlatformIDsByArtist removes all platform artist ID mappings for an artist.
func (s *Service) DeletePlatformIDsByArtist(ctx context.Context, artistID string) error {
	return s.platformIDs.DeleteByArtistID(ctx, artistID)
}

// GetPlatformPresenceForArtists returns platform presence (Emby, Jellyfin,
// Lidarr) for a batch of artists. Artists with no platform ID mappings are
// omitted from the result map.
func (s *Service) GetPlatformPresenceForArtists(ctx context.Context, artistIDs []string) (map[string]PlatformPresence, error) {
	return s.platformIDs.GetPresenceForArtists(ctx, artistIDs)
}

// hydrateProviderIDs loads provider IDs from the normalized table and applies
// them to the Artist struct fields.
func (s *Service) hydrateProviderIDs(ctx context.Context, a *Artist) error {
	ids, err := s.providers.GetForArtist(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("hydrating provider IDs: %w", err)
	}
	applyProviderIDs(a, ids)
	return nil
}

// dbProvider is the minimal interface hydratePrimaryLibrary needs from the
// repository: a handle to the underlying *sql.DB so it can issue the
// membership lookup. Decorated/wrapped repositories (NewServiceWithRepos)
// satisfy this contract by either embedding *sqliteArtistRepo or
// re-exposing DB(); fake repos used in unit tests omit it and the
// hydration becomes a silent no-op.
type dbProvider interface {
	DB() *sql.DB
}

// hydratePrimaryLibrary populates a.LibraryID from artist_libraries by
// picking the earliest membership row (oldest added_at). The legacy
// artists.library_id column was dropped in migration 004; readers that
// still rely on Artist.LibraryID (rule engine shared-fs detection,
// compliance CSV export, library-name display in artist detail pages)
// continue to see a value derived from the M:N table. A repository that
// does not expose a *sql.DB is a silent no-op so tests using fake repos
// without a real DB are unaffected.
//
// Per the OpenAPI contract on Artist.library_id, the field is empty when
// the artist has no library memberships. We therefore CLEAR LibraryID for
// orphaned artists so callers do not see stale values left over from a
// previous hydration or a bare struct literal.
func (s *Service) hydratePrimaryLibrary(ctx context.Context, a *Artist) error {
	if a == nil || a.ID == "" {
		return nil
	}
	// Use the underlying *sql.DB through the dbProvider interface so wrapped
	// repos (decorators that embed *sqliteArtistRepo) still hydrate correctly.
	provider, ok := s.artists.(dbProvider)
	if !ok {
		return nil
	}
	db := provider.DB()
	if db == nil {
		return nil
	}
	// added_at can hold mixed SQLite ("YYYY-MM-DD HH:MM:SS") and RFC3339
	// timestamps from different writers, so wrap with datetime() to compare
	// chronologically rather than lexicographically.
	var libID string
	err := db.QueryRowContext(ctx,
		`SELECT library_id FROM artist_libraries WHERE artist_id = ? ORDER BY datetime(added_at), library_id LIMIT 1`,
		a.ID).Scan(&libID)
	if errors.Is(err, sql.ErrNoRows) {
		// Zero memberships: clear LibraryID per the OpenAPI contract so
		// readers do not observe a stale caller-set value for an orphaned
		// artist.
		a.LibraryID = ""
		return nil
	}
	if err != nil {
		return fmt.Errorf("hydrating primary library for artist %s: %w", a.ID, err)
	}
	a.LibraryID = libID
	return nil
}

// hydratePrimaryLibrariesBatch populates LibraryID on a slice of artists
// in a single query so List does not fan out to N round-trips. The same
// "earliest added_at" rule applies per artist. Artists without any
// membership row have LibraryID CLEARED to "" per the OpenAPI contract;
// any caller-set value on an orphaned artist would otherwise leak into
// API responses.
func (s *Service) hydratePrimaryLibrariesBatch(ctx context.Context, artists []Artist) error {
	if len(artists) == 0 {
		return nil
	}
	provider, ok := s.artists.(dbProvider)
	if !ok {
		return nil
	}
	db := provider.DB()
	if db == nil {
		return nil
	}
	placeholders := make([]string, len(artists))
	args := make([]any, len(artists))
	for i := range artists {
		placeholders[i] = "?"
		args[i] = artists[i].ID
	}
	// Window-style "first per group" via NOT EXISTS so we get exactly one
	// row per artist_id (the one with the smallest added_at, ties broken
	// by library_id). Wrap added_at with datetime() to normalize the mixed
	// "YYYY-MM-DD HH:MM:SS" + RFC3339 formats present in production data;
	// raw TEXT comparison would order RFC3339 (T separator) after SQLite
	// (space separator) and pick the wrong "earliest" membership.
	//nolint:gosec // G202: placeholders is a literal "?,?,..." string built by joining "?" literals; no user input.
	query := `SELECT al.artist_id, al.library_id FROM artist_libraries al
		WHERE al.artist_id IN (` + strings.Join(placeholders, ",") + `)
		AND NOT EXISTS (
			SELECT 1 FROM artist_libraries al2
			WHERE al2.artist_id = al.artist_id
			AND (datetime(al2.added_at) < datetime(al.added_at)
				OR (datetime(al2.added_at) = datetime(al.added_at) AND al2.library_id < al.library_id))
		)`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("batch hydrating primary library: %w", err)
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	libByArtist := make(map[string]string, len(artists))
	for rows.Next() {
		var aid, lid string
		if err := rows.Scan(&aid, &lid); err != nil {
			return fmt.Errorf("scanning primary library row: %w", err)
		}
		libByArtist[aid] = lid
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating primary library rows: %w", err)
	}
	for i := range artists {
		if lid, ok := libByArtist[artists[i].ID]; ok {
			artists[i].LibraryID = lid
		} else {
			// Zero memberships: clear LibraryID per the OpenAPI contract.
			// Without this, a caller-set value on an orphaned artist would
			// survive batch hydration and leak into List responses.
			artists[i].LibraryID = ""
		}
	}
	return nil
}

// hydrateProviderIDsBatch loads provider IDs for multiple artists in a single query.
func (s *Service) hydrateProviderIDsBatch(ctx context.Context, artists []Artist) error {
	if len(artists) == 0 {
		return nil
	}
	ids := make([]string, len(artists))
	for i := range artists {
		ids[i] = artists[i].ID
	}
	idMap, err := s.providers.GetForArtists(ctx, ids)
	if err != nil {
		return fmt.Errorf("batch hydrating provider IDs: %w", err)
	}
	for i := range artists {
		applyProviderIDs(&artists[i], idMap[artists[i].ID])
	}
	return nil
}

// applyProviderIDs sets the Artist struct's provider ID fields from the
// normalized ProviderID slice.
func applyProviderIDs(a *Artist, ids []ProviderID) {
	for _, p := range ids {
		switch p.Provider {
		case "musicbrainz":
			a.MusicBrainzID = p.ProviderID
		case "audiodb":
			a.AudioDBID = p.ProviderID
			a.AudioDBIDFetchedAt = p.FetchedAt
		case "discogs":
			a.DiscogsID = p.ProviderID
			a.DiscogsIDFetchedAt = p.FetchedAt
		case "wikidata":
			a.WikidataID = p.ProviderID
			a.WikidataIDFetchedAt = p.FetchedAt
		case "deezer":
			a.DeezerID = p.ProviderID
		case "spotify":
			a.SpotifyID = p.ProviderID
		case "lastfm":
			a.LastFMFetchedAt = p.FetchedAt
		}
	}
}

// extractProviderIDs builds a ProviderID slice from the Artist struct's
// provider ID fields, ready for persistence.
func extractProviderIDs(a *Artist) []ProviderID {
	var ids []ProviderID

	if a.MusicBrainzID != "" {
		ids = append(ids, ProviderID{Provider: "musicbrainz", ProviderID: a.MusicBrainzID})
	}
	if a.AudioDBID != "" || a.AudioDBIDFetchedAt != nil {
		ids = append(ids, ProviderID{Provider: "audiodb", ProviderID: a.AudioDBID, FetchedAt: a.AudioDBIDFetchedAt})
	}
	if a.DiscogsID != "" || a.DiscogsIDFetchedAt != nil {
		ids = append(ids, ProviderID{Provider: "discogs", ProviderID: a.DiscogsID, FetchedAt: a.DiscogsIDFetchedAt})
	}
	if a.WikidataID != "" || a.WikidataIDFetchedAt != nil {
		ids = append(ids, ProviderID{Provider: "wikidata", ProviderID: a.WikidataID, FetchedAt: a.WikidataIDFetchedAt})
	}
	if a.DeezerID != "" {
		ids = append(ids, ProviderID{Provider: "deezer", ProviderID: a.DeezerID})
	}
	if a.SpotifyID != "" {
		ids = append(ids, ProviderID{Provider: "spotify", ProviderID: a.SpotifyID})
	}
	if a.LastFMFetchedAt != nil {
		ids = append(ids, ProviderID{Provider: "lastfm", ProviderID: "", FetchedAt: a.LastFMFetchedAt})
	}

	return ids
}

// GetImagesForArtist returns the raw ArtistImage rows for a given artist.
// This is useful when callers need direct access to image metadata fields
// (phash, source, last_written_at) rather than the summarized flags on Artist.
func (s *Service) GetImagesForArtist(ctx context.Context, artistID string) ([]ArtistImage, error) {
	if artistID == "" {
		return nil, fmt.Errorf("artist ID is required")
	}
	return s.images.GetForArtist(ctx, artistID)
}

// NewestWriteTimesByArtistForLibrary returns a map of artist_id to their most
// recent last_written_at timestamp string for all artists in the given library.
// Only artists with at least one recorded write are included.
func (s *Service) NewestWriteTimesByArtistForLibrary(ctx context.Context, libraryID string) (map[string]string, error) {
	return s.images.NewestWriteTimesByArtist(ctx, libraryID)
}

// ListPathsByLibrary returns a map of artist ID to filesystem path for all
// artists in the given library that have a non-empty path.
func (s *Service) ListPathsByLibrary(ctx context.Context, libraryID string) (map[string]string, error) {
	return s.artists.ListPathsByLibrary(ctx, libraryID)
}

// ListRefsByLibrary returns lightweight (id, name, path) records for every
// artist in the given library with a non-empty path. Single-query helper
// used by the scanner's per-library removal sweep (#1409) so detectRemoved
// does not have to paginate the full hydrated artist list.
func (s *Service) ListRefsByLibrary(ctx context.Context, libraryID string) ([]ArtistRef, error) {
	return s.artists.ListRefsByLibrary(ctx, libraryID)
}

// GetByIDsBatch returns the artists matching the supplied IDs as a map keyed
// by artist ID. Missing IDs are silently dropped (no error). The opts arg
// controls side-table hydration with the same semantics as GetByID; without
// opts every side-table is hydrated for back-compat. With HydrateOpts{} the
// call issues exactly one query (the IN-clause SELECT).
//
// Inputs are validated and de-duplicated at the boundary:
//   - Empty input yields an empty map with no DB round-trip.
//   - Duplicate IDs collapse to a single lookup; the map keeps one entry.
//   - The slice is capped at MaxListIDs as defense in depth; callers that
//     could exceed the cap should pre-chunk their input.
//
// Used by the bulk-action handler (#1410) so a 50-ID kickoff issues at most
// 4 queries (one core + three batched side tables when hydration is on)
// instead of 50 GetByID round-trips.
func (s *Service) GetByIDsBatch(ctx context.Context, ids []string, opts ...HydrateOpts) (map[string]*Artist, error) {
	if len(ids) == 0 {
		return map[string]*Artist{}, nil
	}
	// Dedupe + drop empties to keep the IN-clause tight and avoid double-
	// hydration cost. Defense in depth: callers (e.g. handleBulkAction)
	// already dedupe, but a future caller might not.
	seen := make(map[string]struct{}, len(ids))
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
	}
	if len(clean) == 0 {
		return map[string]*Artist{}, nil
	}
	// Hard cap to match the Repository contract; SQLite's bound-parameter
	// limit (default 32766) is well above MaxListIDs but the API-side cap
	// (api.MaxBulkActionIDs) is sourced from MaxListIDs so staying inside
	// it keeps the two surfaces in lockstep. Reject over-limit input
	// explicitly so callers see the cap instead of having selections
	// silently truncated past it.
	if len(clean) > MaxListIDs {
		return nil, fmt.Errorf("too many artist IDs: %d > %d", len(clean), MaxListIDs)
	}
	artists, err := s.artists.ListByIDs(ctx, clean)
	if err != nil {
		return nil, err
	}
	if err := s.applyHydrationBatch(ctx, artists, resolveHydrateOpts(opts)); err != nil {
		return nil, err
	}
	result := make(map[string]*Artist, len(artists))
	for i := range artists {
		// Point directly into the slice so each map entry has a distinct,
		// stable address. The previous `a := artists[i]; &a` form worked
		// because each iteration declared a fresh `a` that escaped to the
		// heap, but `&artists[i]` is idiomatic, avoids the per-row copy,
		// and matches the pointer-identity guard in the batch tests.
		result[artists[i].ID] = &artists[i]
	}
	return result, nil
}

// PreloadArtistsByLibrary returns every artist in the given library as a map
// keyed by filesystem path. Artists with an empty path are excluded so the
// map shape matches what the scanner uses for membership lookups. Hydration
// follows the same opts contract as GetByIDsBatch.
//
// Used by the scanner's processDirectory pre-load (#1411) so the per-
// directory hot path can resolve "do we already know this artist?" by
// reading from this map instead of issuing a GetByPath round-trip per
// directory. With HydrateOpts{} the call issues exactly one query for an
// arbitrarily large library.
func (s *Service) PreloadArtistsByLibrary(ctx context.Context, libraryID string, opts ...HydrateOpts) (map[string]*Artist, error) {
	if libraryID == "" {
		return map[string]*Artist{}, nil
	}
	artists, err := s.artists.ListByLibrary(ctx, libraryID)
	if err != nil {
		return nil, err
	}
	if err := s.applyHydrationBatch(ctx, artists, resolveHydrateOpts(opts)); err != nil {
		return nil, err
	}
	result := make(map[string]*Artist, len(artists))
	for i := range artists {
		if artists[i].Path == "" {
			continue
		}
		result[artists[i].Path] = &artists[i]
	}
	return result, nil
}

// hydrateImages loads image metadata from the normalized table and applies
// it to the Artist struct fields.
func (s *Service) hydrateImages(ctx context.Context, a *Artist) error {
	imgs, err := s.images.GetForArtist(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("hydrating images: %w", err)
	}
	applyImageMetadata(a, imgs)
	return nil
}

// hydrateImagesBatch loads image metadata for multiple artists in a single query.
func (s *Service) hydrateImagesBatch(ctx context.Context, artists []Artist) error {
	if len(artists) == 0 {
		return nil
	}
	ids := make([]string, len(artists))
	for i := range artists {
		ids[i] = artists[i].ID
	}
	imgMap, err := s.images.GetForArtists(ctx, ids)
	if err != nil {
		return fmt.Errorf("batch hydrating images: %w", err)
	}
	for i := range artists {
		applyImageMetadata(&artists[i], imgMap[artists[i].ID])
	}
	return nil
}

// applyImageMetadata sets the Artist struct's image fields from the
// normalized ArtistImage slice.
func applyImageMetadata(a *Artist, imgs []ArtistImage) {
	fanartCount := 0
	for i := range imgs {
		img := &imgs[i]
		switch img.ImageType {
		case "thumb":
			a.ThumbExists = img.Exists
			a.ThumbLowRes = img.LowRes
			a.ThumbPlaceholder = img.Placeholder
			a.ThumbWidth = img.Width
			a.ThumbHeight = img.Height
		case "fanart":
			if img.SlotIndex == 0 {
				a.FanartExists = img.Exists
				a.FanartLowRes = img.LowRes
				a.FanartPlaceholder = img.Placeholder
				a.FanartWidth = img.Width
				a.FanartHeight = img.Height
			}
			if img.Exists {
				fanartCount++
			}
		case "logo":
			a.LogoExists = img.Exists
			a.LogoLowRes = img.LowRes
			a.LogoPlaceholder = img.Placeholder
			a.LogoWidth = img.Width
			a.LogoHeight = img.Height
		case "banner":
			a.BannerExists = img.Exists
			a.BannerLowRes = img.LowRes
			a.BannerPlaceholder = img.Placeholder
			a.BannerWidth = img.Width
			a.BannerHeight = img.Height
		}
	}
	a.FanartCount = fanartCount
}

// extractImageMetadata builds an ArtistImage slice from the Artist struct's
// image fields, ready for persistence. Provenance fields (phash, source,
// file_format, last_written_at) are populated separately via
// UpdateImageProvenance after the image file is saved to disk.
func extractImageMetadata(a *Artist) []ArtistImage {
	var imgs []ArtistImage

	if a.ThumbExists || a.ThumbLowRes || a.ThumbPlaceholder != "" {
		imgs = append(imgs, ArtistImage{
			ArtistID:    a.ID,
			ImageType:   "thumb",
			SlotIndex:   0,
			Exists:      a.ThumbExists,
			LowRes:      a.ThumbLowRes,
			Placeholder: a.ThumbPlaceholder,
			Width:       a.ThumbWidth,
			Height:      a.ThumbHeight,
		})
	}
	if a.FanartExists || a.FanartLowRes || a.FanartPlaceholder != "" {
		imgs = append(imgs, ArtistImage{
			ArtistID:    a.ID,
			ImageType:   "fanart",
			SlotIndex:   0,
			Exists:      a.FanartExists,
			LowRes:      a.FanartLowRes,
			Placeholder: a.FanartPlaceholder,
			Width:       a.FanartWidth,
			Height:      a.FanartHeight,
		})
		// Persist additional fanart slots so FanartCount round-trips through the DB.
		// Slots 1..FanartCount-1 only track existence; per-slot metadata (dimensions,
		// placeholder) is not maintained for non-primary fanart files.
		if a.FanartExists {
			for i := 1; i < a.FanartCount; i++ {
				imgs = append(imgs, ArtistImage{
					ArtistID:  a.ID,
					ImageType: "fanart",
					SlotIndex: i,
					Exists:    true,
				})
			}
		}
	}
	if a.LogoExists || a.LogoLowRes || a.LogoPlaceholder != "" {
		imgs = append(imgs, ArtistImage{
			ArtistID:    a.ID,
			ImageType:   "logo",
			SlotIndex:   0,
			Exists:      a.LogoExists,
			LowRes:      a.LogoLowRes,
			Placeholder: a.LogoPlaceholder,
			Width:       a.LogoWidth,
			Height:      a.LogoHeight,
		})
	}
	if a.BannerExists || a.BannerLowRes || a.BannerPlaceholder != "" {
		imgs = append(imgs, ArtistImage{
			ArtistID:    a.ID,
			ImageType:   "banner",
			SlotIndex:   0,
			Exists:      a.BannerExists,
			LowRes:      a.BannerLowRes,
			Placeholder: a.BannerPlaceholder,
			Width:       a.BannerWidth,
			Height:      a.BannerHeight,
		})
	}

	return imgs
}
