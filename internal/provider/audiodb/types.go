package audiodb

// TheAudioDB API response types.

// ArtistResponse is the top-level response from TheAudioDB artist endpoints.
type ArtistResponse struct {
	Artists []AudioDBArtist `json:"artists"`
}

// AudioDBArtist represents a TheAudioDB artist entity.
type AudioDBArtist struct {
	IDArtist        string `json:"idArtist"`
	Artist          string `json:"strArtist"`
	ArtistAlternate string `json:"strArtistAlternate"`
	Label           string `json:"strLabel"`
	Genre           string `json:"strGenre"`
	Style           string `json:"strStyle"`
	Mood            string `json:"strMood"`
	BiographyEN     string `json:"strBiographyEN"`
	Gender          string `json:"strGender"`
	Country         string `json:"strCountry"`
	FormedYear      string `json:"intFormedYear"`
	BornYear        string `json:"intBornYear"`
	DiedYear        string `json:"intDiedYear"`
	Disbanded       string `json:"strDisbanded"`
	MusicBrainzID   string `json:"strMusicBrainzID"`
	ArtistThumb     string `json:"strArtistThumb"`
	ArtistLogo      string `json:"strArtistLogo"`
	ArtistWideThumb string `json:"strArtistWideThumb"`
	ArtistFanart    string `json:"strArtistFanart"`
	ArtistFanart2   string `json:"strArtistFanart2"`
	ArtistFanart3   string `json:"strArtistFanart3"`
	ArtistFanart4   string `json:"strArtistFanart4"`
	ArtistBanner    string `json:"strArtistBanner"`
	ArtistCutout    string `json:"strArtistCutout"`
	ArtistClearart  string `json:"strArtistClearart"`
}
