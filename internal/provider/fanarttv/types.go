package fanarttv

// Fanart.tv API response types.

// Response is the top-level response for an artist lookup.
type Response struct {
	Name            string        `json:"name"`
	MBID            string        `json:"mbid_id"`
	ArtistThumb     []FanartImage `json:"artistthumb"`
	ArtistBackground []FanartImage `json:"artistbackground"`
	HDMusicLogo     []FanartImage `json:"hdmusiclogo"`
	MusicLogo       []FanartImage `json:"musiclogo"`
	MusicBanner     []FanartImage `json:"musicbanner"`
}

// FanartImage represents a single image from Fanart.tv.
type FanartImage struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Likes string `json:"likes"`
	Lang string `json:"lang"`
}
