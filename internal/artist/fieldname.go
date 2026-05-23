package artist

// FieldName is a typed identifier for an artist metadata field. The
// constants below cover every field that Stillwater allows a user to lock
// against provider-driven overwrite. Using a named type (rather than bare
// strings) at IsFieldLocked / SetLockedFields call sites turns typos at
// those callers into compile-time errors, closing the silent-unlock class
// of bug captured in #1087.
//
// The underlying type is string and the constant values match the
// lowercase keys Stillwater has historically stored in Artist.LockedFields.
// No DB migration is required; legacy stored values normalize via the
// existing case-insensitive lookup in IsFieldLocked.
type FieldName string

// FieldArtistName through FieldYearsActive enumerate Stillwater's full
// lockable-field vocabulary. The string values match the lowercase keys
// historically stored in the database; new fields must be added here AND
// surfaced in AllLockableFields() so the validator stays in sync.
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
)
