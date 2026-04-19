package audiodb

// TheAudioDB API response types.

// ArtistResponse is the top-level response from TheAudioDB artist endpoints.
// The v1 API uses "artists", v2 uses "search" or "lookup" depending on endpoint.
type ArtistResponse struct {
	Artists []AudioDBArtist `json:"artists"` // v1
	Search  []AudioDBArtist `json:"search"`  // v2 search endpoint
	Lookup  []AudioDBArtist `json:"lookup"`  // v2 lookup endpoint
}

// results returns whichever populated slice was returned by the API.
func (r ArtistResponse) results() []AudioDBArtist {
	if len(r.Lookup) > 0 {
		return r.Lookup
	}
	if len(r.Search) > 0 {
		return r.Search
	}
	return r.Artists
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
	// Biography is the API's default/localized field (strBiography). The per-language
	// fields (strBiographyEN, strBiographyDE, etc.) allow preference-ordered selection.
	Biography       string `json:"strBiography"`
	BiographyEN     string `json:"strBiographyEN"`
	BiographyDE     string `json:"strBiographyDE"`
	BiographyFR     string `json:"strBiographyFR"`
	BiographyJA     string `json:"strBiographyJA"`
	BiographyCN     string `json:"strBiographyCN"`
	BiographyIT     string `json:"strBiographyIT"`
	BiographyPT     string `json:"strBiographyPT"`
	BiographyRU     string `json:"strBiographyRU"`
	BiographyHU     string `json:"strBiographyHU"`
	BiographyIL     string `json:"strBiographyIL"` // IL = AudioDB's name for Hebrew (he)
	BiographyNO     string `json:"strBiographyNO"`
	BiographySE     string `json:"strBiographySE"`
	BiographyPL     string `json:"strBiographyPL"`
	BiographyNL     string `json:"strBiographyNL"`
	BiographyES     string `json:"strBiographyES"`
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
