package jellyfin

// SystemInfo represents the response from GET /System/Info.
type SystemInfo struct {
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
	ID         string `json:"Id"`
}

// LibraryOptions contains per-library configuration from Jellyfin.
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

// ArtistItem represents an artist from the AlbumArtists endpoint.
type ArtistItem struct {
	Name         string            `json:"Name"`
	SortName     string            `json:"SortName"`
	ID           string            `json:"Id"`
	Path         string            `json:"Path"`
	Overview     string            `json:"Overview"`
	Genres       []string          `json:"Genres"`
	Tags         []string          `json:"Tags"`
	PremiereDate string            `json:"PremiereDate"`
	EndDate      string            `json:"EndDate"`
	ProviderIDs  ProviderIDs       `json:"ProviderIds"`
	ImageTags    map[string]string `json:"ImageTags"`
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

// UserItem represents a user entry from GET /Users.
type UserItem struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// ArtistDetailItem represents detailed metadata for an artist item from Jellyfin.
type ArtistDetailItem struct {
	Name              string            `json:"Name"`
	SortName          string            `json:"SortName"`
	Overview          string            `json:"Overview"`
	Genres            []string          `json:"Genres"`
	Tags              []string          `json:"Tags"`
	PremiereDate      string            `json:"PremiereDate"`
	EndDate           string            `json:"EndDate"`
	ProviderIDs       ProviderIDs       `json:"ProviderIds"`
	ImageTags         map[string]string `json:"ImageTags"`
	BackdropImageTags []string          `json:"BackdropImageTags"`
	IsLocked          bool              `json:"IsLocked"`
	LockedFields      []string          `json:"LockedFields"`
}
