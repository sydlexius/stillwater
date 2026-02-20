package image

// DefaultFileNames returns the default naming patterns for each image type.
// These are the known filename patterns used by media servers.
var DefaultFileNames = map[string][]string{
	"thumb":  {"folder.jpg", "folder.png", "artist.jpg", "artist.png", "poster.jpg", "poster.png"},
	"fanart": {"fanart.jpg", "fanart.png", "backdrop.jpg", "backdrop.png"},
	"logo":   {"logo.png", "logo-white.png"},
	"banner": {"banner.jpg", "banner.png"},
}

// FileNamesForType returns the configured filenames for an image type.
// Falls back to an empty slice if the type is unknown.
func FileNamesForType(naming map[string][]string, imageType string) []string {
	if names, ok := naming[imageType]; ok {
		return names
	}
	return nil
}

// PrimaryFileName returns the first filename for the given image type,
// or empty string if none are configured.
func PrimaryFileName(naming map[string][]string, imageType string) string {
	names := FileNamesForType(naming, imageType)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}
