package artist

// FieldName is a typed identifier for an artist metadata field. The
// constants below cover every field that Stillwater allows a user to lock
// against provider-driven overwrite. Using a named type (rather than bare
// strings) at IsFieldLocked / SetLockedFields call sites encourages a
// constants-first style that grep, refactor tools, and (where the caller
// holds a typed string variable) the compiler can all reason about. Note
// that Go still permits an untyped string literal to be passed where a
// FieldName is expected -- the type catches typed-string mismatches and
// renames, not raw-literal typos -- so callers should prefer the
// artist.FieldX constants below over inline string literals.
//
// The underlying type is string and the constant values match the
// lowercase keys Stillwater has historically stored in Artist.LockedFields.
// No DB migration is required; legacy stored values normalize via the
// existing case-insensitive lookup in IsFieldLocked.
type FieldName string

// FieldArtistName through FieldYearsActive enumerate Stillwater's full
// lockable-field vocabulary. The string values match the lowercase keys
// historically stored in the database. When adding a field, keep this
// constant set and any validation/canonicalization paths (notably the
// platform-side canonicalizer maps in internal/connection/emby) in sync.
const (
	FieldArtistName     FieldName = "name"
	FieldSortName       FieldName = "sort_name"
	FieldBiography      FieldName = "biography"
	FieldGenres         FieldName = "genres"
	FieldStyles         FieldName = "styles"
	FieldMoods          FieldName = "moods"
	FieldMembers        FieldName = "members"
	FieldType           FieldName = "type"
	FieldGender         FieldName = "gender"
	FieldOrigin         FieldName = "origin"
	FieldDisambiguation FieldName = "disambiguation"
	FieldFormed         FieldName = "formed"
	FieldBorn           FieldName = "born"
	FieldDied           FieldName = "died"
	FieldDisbanded      FieldName = "disbanded"
	FieldYearsActive    FieldName = "years_active"

	// FieldDiscogsID is the lock key for the Discogs provider ID. Unlike the
	// metadata fields above, this is a provider-ID field: it is not part of the
	// platform-side metadata canonicalizer vocabulary, but it participates in
	// the same per-field lock mechanism (Artist.LockedFields) so the Discogs
	// "match by name" identify flow can refuse to overwrite a user-pinned ID.
	// Its string value matches the discogs_id key used throughout the UI and
	// the FieldDisplay switch.
	FieldDiscogsID FieldName = "discogs_id"

	// FieldAudioDBID is the lock key for the TheAudioDB provider ID. Like
	// FieldDiscogsID it is a provider-ID field (not part of the platform-side
	// canonicalizer vocabulary) that participates in the per-field lock
	// mechanism so the AudioDB "match by name" identify flow can refuse to
	// overwrite a user-pinned ID. Its string value matches the audiodb_id key
	// used throughout the UI and the FieldDisplay switch.
	FieldAudioDBID FieldName = "audiodb_id"
)
