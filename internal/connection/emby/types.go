package emby

// SystemInfo represents the response from GET /System/Info.
type SystemInfo struct {
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
	ID         string `json:"Id"`
}

// LibraryOptions contains per-library configuration from Emby.
type LibraryOptions struct {
	SaveLocalMetadata bool     `json:"SaveLocalMetadata"`
	MetadataSavers    []string `json:"MetadataSavers"`
}

// VirtualFolder represents a library folder from GET /Library/VirtualFolders.
type VirtualFolder struct {
	Name           string         `json:"Name"`
	CollectionType string         `json:"CollectionType"`
	ItemID         string         `json:"ItemId"`
	LibraryOptions LibraryOptions `json:"LibraryOptions"`
}

// ArtistItem represents an artist from the Items endpoint.
type ArtistItem struct {
	Name        string      `json:"Name"`
	ID          string      `json:"Id"`
	Path        string      `json:"Path"`
	ProviderIDs ProviderIDs `json:"ProviderIds"`
}

// ProviderIDs contains external provider identifiers.
type ProviderIDs struct {
	MusicBrainzArtist string `json:"MusicBrainzArtist"`
}

// ItemsResponse wraps paginated item results.
type ItemsResponse struct {
	Items            []ArtistItem `json:"Items"`
	TotalRecordCount int          `json:"TotalRecordCount"`
}
