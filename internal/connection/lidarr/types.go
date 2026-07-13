package lidarr

// SystemStatus represents the response from GET /api/v1/system/status.
type SystemStatus struct {
	Version string `json:"version"`
	AppName string `json:"appName"`
}

// ArtistImage represents an image associated with a Lidarr artist.
type ArtistImage struct {
	CoverType string `json:"coverType"`
	URL       string `json:"url"`
}

// Artist represents an artist from GET /api/v1/artist.
type Artist struct {
	ID                int           `json:"id"`
	ArtistName        string        `json:"artistName"`
	ForeignArtistID   string        `json:"foreignArtistId"`
	Path              string        `json:"path"`
	Monitored         bool          `json:"monitored"`
	MetadataProfileID int           `json:"metadataProfileId"`
	Images            []ArtistImage `json:"images"`
}

// MetadataProfile represents a metadata profile from GET /api/v1/metadataprofile.
type MetadataProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// MetadataProviderConfig represents metadata provider config from GET /api/v1/config/metadataprovider.
type MetadataProviderConfig struct {
	ID           int    `json:"id"`
	MetadataType string `json:"metadataType"`
	ConsumerID   int    `json:"consumerId"`
	ConsumerName string `json:"consumerName"`
	Enable       bool   `json:"enable"`
}

// CommandBody is the request body for POST /api/v1/command.
type CommandBody struct {
	Name     string `json:"name"`
	ArtistID int    `json:"artistId,omitempty"`
}

// CommandResponse is the response from POST /api/v1/command.
type CommandResponse struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// RootFolder represents one entry from GET /api/v1/rootfolder. Path is the
// directory as LIDARR addresses it (its own container's filesystem namespace),
// which is precisely what the pre-flight root guard needs: a path Stillwater is
// about to push must resolve inside one of these, or Lidarr will store a
// location that means nothing on its side. Lidarr accepts an out-of-root path
// verbatim over the API -- it does NOT enforce canonical naming -- so this list
// is the only way to catch the mistake before it lands (#2380).
type RootFolder struct {
	ID   int    `json:"id"`
	Path string `json:"path"`
}
