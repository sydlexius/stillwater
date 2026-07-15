package artist

// ImageSourceUser is the provenance sentinel stamped on an image slot's
// `source` (in EXIF at write time and in the artist_images row) when the
// operator set that image by hand. The #2533 data-loss carve-out keys off it
// to protect operator-set artwork from auto image fixers, so every write site
// that marks user provenance and every read site that checks for it must use
// this single constant -- a divergent literal on either side would silently
// disable the protection, and tests hardcoding the same string would not catch
// the drift.
const ImageSourceUser = "user"

// ArtistImage represents image metadata for a specific image type and slot.
// Image metadata is stored in a normalized table (artist_images) rather than
// as columns on the artists table.
type ArtistImage struct {
	ID          string `json:"id"`
	ArtistID    string `json:"artist_id"`
	ImageType   string `json:"image_type"`
	SlotIndex   int    `json:"slot_index"`
	Exists      bool   `json:"exists"`
	LowRes      bool   `json:"low_res"`
	Placeholder string `json:"placeholder,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	PHash       string `json:"phash,omitempty"`
	// ContentHash is the sha256 (hex) of the file's on-disk bytes, used by
	// the exact-duplicate rule. Empty means "not yet hashed", not "no
	// duplicate"; it is backfilled lazily by duplicate detection.
	ContentHash   string `json:"content_hash,omitempty"`
	FileFormat    string `json:"file_format,omitempty"`
	Source        string `json:"source,omitempty"`
	LastWrittenAt string `json:"last_written_at,omitempty"`
	Locked        bool   `json:"locked"`
}
