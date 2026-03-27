package emby

// SystemInfo represents the response from GET /System/Info.
type SystemInfo struct {
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
	ID         string `json:"Id"`
}

// LibraryOptions contains per-library configuration from Emby.
type LibraryOptions struct {
	SaveLocalMetadata bool         `json:"SaveLocalMetadata"`
	MetadataSavers    []string     `json:"MetadataSavers"`
	TypeOptions       []TypeOption `json:"TypeOptions"`
}

// TypeOption contains per-content-type settings (e.g., MusicArtist).
type TypeOption struct {
	Type             string   `json:"Type"`
	ImageFetchers    []string `json:"ImageFetchers"`
	MetadataFetchers []string `json:"MetadataFetchers"`
}

// VirtualFolder represents a library folder from GET /Library/VirtualFolders.
type VirtualFolder struct {
	Name           string         `json:"Name"`
	Locations      []string       `json:"Locations"` // Filesystem paths for this library
	CollectionType string         `json:"CollectionType"`
	ItemID         string         `json:"ItemId"`
	LibraryOptions LibraryOptions `json:"LibraryOptions"`
}

// ArtistItem represents an artist from the AlbumArtists endpoint.
type ArtistItem struct {
	Name              string            `json:"Name"`
	SortName          string            `json:"SortName"`
	ID                string            `json:"Id"`
	Path              string            `json:"Path"`
	Overview          string            `json:"Overview"`
	Genres            []string          `json:"Genres"`
	Tags              []string          `json:"Tags"`
	PremiereDate      string            `json:"PremiereDate"`
	EndDate           string            `json:"EndDate"`
	ProviderIDs       ProviderIDs       `json:"ProviderIds"`
	ImageTags         map[string]string `json:"ImageTags"`
	BackdropImageTags []string          `json:"BackdropImageTags"`
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

// ArtistDetailItem represents detailed metadata for an artist item from Emby.
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
	LockData          bool              `json:"LockData"`
	LockedFields      []string          `json:"LockedFields"`
}

// AuthResult holds the response from POST /Users/AuthenticateByName.
type AuthResult struct {
	AccessToken string   `json:"AccessToken"`
	User        AuthUser `json:"User"`
}

// AuthUser contains user identity from the AuthenticateByName response.
type AuthUser struct {
	ID     string     `json:"Id"`
	Name   string     `json:"Name"`
	Policy UserPolicy `json:"Policy"`
}

// UserPolicy contains user permission flags from the media server.
type UserPolicy struct {
	IsAdministrator bool `json:"IsAdministrator"`
}
