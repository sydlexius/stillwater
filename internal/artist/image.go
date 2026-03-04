package artist

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
	FileFormat  string `json:"file_format,omitempty"`
	Source      string `json:"source,omitempty"`
}
