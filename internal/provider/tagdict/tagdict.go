// Package tagdict provides canonical spelling normalization for genre/style/mood tags.
// Tags from different providers use inconsistent spellings; this package maps common
// variants to their canonical forms and deduplicates across providers.
//
// Locale-aware deduplication: Wikidata returns localized genre labels while
// MusicBrainz returns English-only strings. Without locale awareness,
// MergeAndDeduplicate stores both "Rock" and the Japanese form "ロック" as
// distinct tags even though they represent the same concept. The locale maps in
// this file let callers request that the user's preferred-language form survive
// and the English duplicate be suppressed when the two are recognized synonyms.
package tagdict

import "strings"

// canonical maps normalized tag variants to their preferred spelling.
// Keys must be lowercase with whitespace collapsed.
// Seeded from AllMusic's taxonomy (most curated source), supplemented
// by MusicBrainz genre list and Last.fm top tags.
var canonical = map[string]string{
	// Synth/Electronic variants
	"synthpop":                "Synth-Pop",
	"synth pop":               "Synth-Pop",
	"synth-pop":               "Synth-Pop",
	"electropop":              "Electropop",
	"electronica":             "Electronica",
	"electronic":              "Electronic",
	"idm":                     "IDM",
	"intelligent dance music": "IDM",

	// Lo-Fi variants
	"lofi":          "Lo-Fi",
	"lo fi":         "Lo-Fi",
	"lo-fi":         "Lo-Fi",
	"lo-fi hip hop": "Lo-Fi Hip-Hop",
	"lofi hip hop":  "Lo-Fi Hip-Hop",

	// Hip-Hop/Rap
	"hip hop":          "Hip-Hop",
	"hip-hop":          "Hip-Hop",
	"hiphop":           "Hip-Hop",
	"rap":              "Rap",
	"trap":             "Trap",
	"r&b":              "R&B",
	"r & b":            "R&B",
	"rnb":              "R&B",
	"rhythm and blues": "R&B",

	// Rock
	"rock": "Rock",

	// Rock subgenres
	"alt rock":         "Alternative Rock",
	"alt-rock":         "Alternative Rock",
	"alternative rock": "Alternative Rock",
	"indie rock":       "Indie Rock",
	"indie pop":        "Indie Pop",
	"post-rock":        "Post-Rock",
	"post rock":        "Post-Rock",
	"postrock":         "Post-Rock",
	"math rock":        "Math Rock",
	"math-rock":        "Math Rock",
	"prog rock":        "Progressive Rock",
	"progressive rock": "Progressive Rock",
	"psychedelic rock": "Psychedelic Rock",
	"psych rock":       "Psychedelic Rock",
	"shoegaze":         "Shoegaze",
	"dream pop":        "Dream Pop",
	"dream-pop":        "Dream Pop",
	"noise rock":       "Noise Rock",
	"noise-rock":       "Noise Rock",
	"new wave":         "New Wave",
	"post-punk":        "Post-Punk",
	"post punk":        "Post-Punk",
	"punk rock":        "Punk Rock",
	"garage rock":      "Garage Rock",
	"garage-rock":      "Garage Rock",

	// Metal
	"heavy metal":     "Heavy Metal",
	"death metal":     "Death Metal",
	"black metal":     "Black Metal",
	"doom metal":      "Doom Metal",
	"thrash metal":    "Thrash Metal",
	"power metal":     "Power Metal",
	"folk metal":      "Folk Metal",
	"symphonic metal": "Symphonic Metal",

	// Country/Folk/Americana
	"country":           "Country",
	"alt country":       "Alt-Country",
	"alt-country":       "Alt-Country",
	"americana":         "Americana",
	"folk":              "Folk",
	"bluegrass":         "Bluegrass",
	"roots rock":        "Roots Rock",
	"singer-songwriter": "Singer-Songwriter",
	"singer songwriter": "Singer-Songwriter",

	// Jazz/Blues/Soul
	"jazz":     "Jazz",
	"blues":    "Blues",
	"soul":     "Soul",
	"funk":     "Funk",
	"gospel":   "Gospel",
	"neo soul": "Neo-Soul",
	"neo-soul": "Neo-Soul",

	// Classical/Orchestral
	"classical":              "Classical",
	"orchestral":             "Orchestral",
	"chamber music":          "Chamber Music",
	"contemporary classical": "Contemporary Classical",

	// World
	"world music": "World Music",
	"latin":       "Latin",
	"bossa nova":  "Bossa Nova",
	"reggae":      "Reggae",
	"reggaeton":   "Reggaeton",
	"afrobeat":    "Afrobeat",

	// Mood-related canonical forms
	"melancholic":   "Melancholic",
	"melancholy":    "Melancholic",
	"bittersweet":   "Bittersweet",
	"chill":         "Chill",
	"chillout":      "Chill",
	"mellow":        "Mellow",
	"dark":          "Dark",
	"atmospheric":   "Atmospheric",
	"dreamy":        "Dreamy",
	"energetic":     "Energetic",
	"uplifting":     "Uplifting",
	"aggressive":    "Aggressive",
	"intense":       "Intense",
	"romantic":      "Romantic",
	"nostalgic":     "Nostalgic",
	"introspective": "Introspective",
	"ethereal":      "Ethereal",

	// K-pop / J-pop (preserve stylization)
	"k-pop":    "K-Pop",
	"kpop":     "K-Pop",
	"j-pop":    "J-Pop",
	"jpop":     "J-Pop",
	"j-rock":   "J-Rock",
	"jrock":    "J-Rock",
	"city pop": "City Pop",
}

// normalizeKey returns a lowercase, whitespace-collapsed form of tag for map lookup.
// Underscores are treated as word separators so "synth_pop" matches "synth pop".
func normalizeKey(tag string) string {
	tag = strings.ReplaceAll(strings.ToLower(tag), "_", " ")
	return strings.Join(strings.Fields(tag), " ")
}

// Canonical returns the preferred spelling for a tag. If no canonical form is
// known, the original tag is returned unchanged.
func Canonical(tag string) string {
	if c, ok := canonical[normalizeKey(tag)]; ok {
		return c
	}
	return tag
}

// MergeAndDeduplicate appends incoming tags to existing, normalizing to canonical
// spelling and deduplicating. First-seen ordering is preserved. Lookup is
// case-insensitive via normalizeKey.
func MergeAndDeduplicate(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	result := make([]string, 0, len(existing)+len(incoming))

	add := func(tag string) {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return
		}
		c := Canonical(tag)
		key := normalizeKey(c)
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			result = append(result, c)
		}
	}

	for _, t := range existing {
		add(t)
	}
	for _, t := range incoming {
		add(t)
	}
	return result
}

// translations maps BCP 47 primary-language subtags to a canonical-EN-form ->
// localized-form lookup. Keys in the inner map match the canonical values
// produced by Canonical(). Only the primary subtag (e.g. "ja" from "ja-JP")
// is used so that region subtags are handled transparently.
//
// Coverage targets the genres, styles, and moods most commonly returned by
// MusicBrainz tags and Wikidata labels for the three seed locales. English
// entries are deliberately absent because the canonical forms are already
// English; any unknown locale falls back to the EN canonical form unchanged.
var translations = map[string]map[string]string{
	"ja": {
		// Core genres
		"Rock":                   "ロック",
		"Alternative Rock":       "オルタナティブ・ロック",
		"Americana":              "アメリカーナ",
		"Atmospheric":            "アトモスフェリック",
		"Bluegrass":              "ブルーグラス",
		"Blues":                  "ブルース",
		"Bossa Nova":             "ボサ・ノバ",
		"Chamber Music":          "室内楽",
		"Chill":                  "チル",
		"City Pop":               "シティ・ポップ",
		"Classical":              "クラシック",
		"Contemporary Classical": "現代クラシック",
		"Country":                "カントリー",
		"Dark":                   "ダーク",
		"Death Metal":            "デスメタル",
		"Dream Pop":              "ドリームポップ",
		"Dreamy":                 "ドリーミー",
		"Electronic":             "エレクトロニック",
		"Electronica":            "エレクトロニカ",
		"Electropop":             "エレクトロポップ",
		"Energetic":              "エネルギッシュ",
		"Ethereal":               "エーテリアル",
		"Folk":                   "フォーク",
		"Funk":                   "ファンク",
		"Garage Rock":            "ガレージロック",
		"Gospel":                 "ゴスペル",
		"Heavy Metal":            "ヘビーメタル",
		"Hip-Hop":                "ヒップホップ",
		"IDM":                    "IDM",
		"Indie Pop":              "インディーポップ",
		"Indie Rock":             "インディーロック",
		"Intense":                "インテンス",
		"Introspective":          "内省的",
		"J-Pop":                  "J-POP",
		"J-Rock":                 "J-ROCK",
		"Jazz":                   "ジャズ",
		"K-Pop":                  "K-POP",
		"Latin":                  "ラテン",
		"Lo-Fi":                  "ローファイ",
		"Lo-Fi Hip-Hop":          "ローファイ・ヒップホップ",
		"Math Rock":              "マスロック",
		"Melancholic":            "メランコリック",
		"Mellow":                 "メロウ",
		"Neo-Soul":               "ネオソウル",
		"New Wave":               "ニューウェーブ",
		"Noise Rock":             "ノイズロック",
		"Nostalgic":              "ノスタルジック",
		"Orchestral":             "オーケストラ",
		"Post-Punk":              "ポストパンク",
		"Post-Rock":              "ポストロック",
		"Progressive Rock":       "プログレッシブ・ロック",
		"Psychedelic Rock":       "サイケデリック・ロック",
		"Punk Rock":              "パンクロック",
		"R&B":                    "R&B",
		"Rap":                    "ラップ",
		"Reggae":                 "レゲエ",
		"Reggaeton":              "レゲトン",
		"Romantic":               "ロマンティック",
		"Roots Rock":             "ルーツロック",
		"Shoegaze":               "シューゲイザー",
		"Singer-Songwriter":      "シンガーソングライター",
		"Soul":                   "ソウル",
		"Synth-Pop":              "シンセポップ",
		"Trap":                   "トラップ",
		"Uplifting":              "アップリフティング",
		"World Music":            "ワールドミュージック",
		// Moods (used by tagclass mood classification)
		"Aggressive":  "アグレッシブ",
		"Bittersweet": "ほろ苦い",
	},
	"fr": {
		// Core genres
		"Rock":                   "Rock",
		"Alternative Rock":       "Rock alternatif",
		"Americana":              "Americana",
		"Atmospheric":            "Atmosphérique",
		"Bluegrass":              "Bluegrass",
		"Blues":                  "Blues",
		"Bossa Nova":             "Bossa Nova",
		"Chamber Music":          "Musique de chambre",
		"Chill":                  "Chill",
		"City Pop":               "City Pop",
		"Classical":              "Classique",
		"Contemporary Classical": "Classique contemporain",
		"Country":                "Country",
		"Dark":                   "Sombre",
		"Death Metal":            "Death metal",
		"Dream Pop":              "Dream pop",
		"Dreamy":                 "Onirique",
		"Electronic":             "Électronique",
		"Electronica":            "Electronica",
		"Electropop":             "Electropop",
		"Energetic":              "Énergique",
		"Ethereal":               "Éthérée",
		"Folk":                   "Folk",
		"Funk":                   "Funk",
		"Garage Rock":            "Rock garage",
		"Gospel":                 "Gospel",
		"Heavy Metal":            "Heavy metal",
		"Hip-Hop":                "Hip-hop",
		"IDM":                    "IDM",
		"Indie Pop":              "Indie pop",
		"Indie Rock":             "Indie rock",
		"Intense":                "Intense",
		"Introspective":          "Introspectif",
		"J-Pop":                  "J-Pop",
		"J-Rock":                 "J-Rock",
		"Jazz":                   "Jazz",
		"K-Pop":                  "K-Pop",
		"Latin":                  "Latin",
		"Lo-Fi":                  "Lo-fi",
		"Lo-Fi Hip-Hop":          "Hip-hop lo-fi",
		"Math Rock":              "Math rock",
		"Melancholic":            "Mélancolique",
		"Mellow":                 "Doux",
		"Neo-Soul":               "Néo-soul",
		"New Wave":               "New wave",
		"Noise Rock":             "Noise rock",
		"Nostalgic":              "Nostalgique",
		"Orchestral":             "Orchestral",
		"Post-Punk":              "Post-punk",
		"Post-Rock":              "Post-rock",
		"Progressive Rock":       "Rock progressif",
		"Psychedelic Rock":       "Rock psychédélique",
		"Punk Rock":              "Punk rock",
		"R&B":                    "R&B",
		"Rap":                    "Rap",
		"Reggae":                 "Reggae",
		"Reggaeton":              "Reggaeton",
		"Romantic":               "Romantique",
		"Roots Rock":             "Rock roots",
		"Shoegaze":               "Shoegaze",
		"Singer-Songwriter":      "Auteur-compositeur-interprète",
		"Soul":                   "Soul",
		"Synth-Pop":              "Synth-pop",
		"Trap":                   "Trap",
		"Uplifting":              "Exaltant",
		"World Music":            "Musique du monde",
		// Moods
		"Aggressive":  "Agressif",
		"Bittersweet": "Doux-amer",
	},
}

// LocalizeTag returns the user-preferred locale form of a tag. The input tag
// is first canonicalized to its EN form; if the locale has a translation for
// that canonical form, the translated label is returned. Otherwise the EN
// canonical form is returned unchanged. An empty or "en" locale always returns
// the EN canonical form.
//
// This is the per-tag counterpart to MergeAndDeduplicateLocale. Callers that
// need to translate a single already-canonical tag can call LocalizeTag
// directly without going through the merge pipeline.
func LocalizeTag(locale, tag string) string {
	canon := Canonical(tag)
	if locale == "" || locale == "en" {
		return canon
	}
	// Strip any region subtag so "ja-JP" and "ja" both hit the same table.
	primary := locale
	if idx := strings.IndexByte(locale, '-'); idx > 0 {
		primary = locale[:idx]
	}
	if table, ok := translations[primary]; ok {
		if localized, ok := table[canon]; ok {
			return localized
		}
	}
	return canon
}

// localeConceptKey returns a stable deduplication key for a tag in the context
// of a given locale. For English and unknown locales the key is the normalized
// canonical EN form. For known locales both the EN canonical form and the
// localized form map to the same key, enabling cross-locale deduplication so
// that "Rock" (EN from MusicBrainz) and "ロック" (ja from Wikidata) are treated
// as the same concept and only the preferred form survives.
//
// Mapping an already-localized form back to its EN concept uses a linear scan
// of the locale table with an exact normalized-string match, which assumes no
// two EN concepts share a localized form within a locale. The
// no-localized-form-collision test enforces that invariant.
func localeConceptKey(locale, tag string) string {
	canon := Canonical(tag)
	enKey := normalizeKey(canon)
	if locale == "" || locale == "en" {
		return enKey
	}
	primary := locale
	if idx := strings.IndexByte(locale, '-'); idx > 0 {
		primary = locale[:idx]
	}
	table, ok := translations[primary]
	if !ok {
		return enKey
	}
	// Check whether this tag, when canonicalized, has a known localized form.
	// If so, use the EN key as the concept anchor so both forms deduplicate.
	if _, hasTranslation := table[canon]; hasTranslation {
		return enKey
	}
	// The tag might already be a localized form. Walk the translation table to
	// find the EN canonical this tag corresponds to, if any.
	tagNorm := normalizeKey(tag)
	for enForm, localizedForm := range table {
		if normalizeKey(localizedForm) == tagNorm {
			return normalizeKey(enForm)
		}
	}
	return enKey
}

// MergeAndDeduplicateLocale is the locale-aware counterpart to
// MergeAndDeduplicate. When locale is non-empty and not "en", tags that
// represent the same concept in different languages (e.g. "Rock" from
// MusicBrainz and "ロック" from Wikidata) are deduplicated to a single entry.
//
// Duplicates are collapsed by a language-independent concept key. The surviving
// entry is always the localized form when a translation exists (via
// LocalizeTag), regardless of which variant arrived first; arrival order only
// determines the position of that single surviving entry in the output slice.
//
// When locale is empty or "en", behavior is identical to MergeAndDeduplicate.
func MergeAndDeduplicateLocale(existing, incoming []string, locale string) []string {
	if locale == "" || locale == "en" {
		return MergeAndDeduplicate(existing, incoming)
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	result := make([]string, 0, len(existing)+len(incoming))

	add := func(tag string) {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return
		}
		c := Canonical(tag)
		conceptKey := localeConceptKey(locale, c)
		if _, dup := seen[conceptKey]; !dup {
			seen[conceptKey] = struct{}{}
			// Store the localized form when a translation is available so the
			// preferred-language form is what ends up in the metadata.
			result = append(result, LocalizeTag(locale, c))
		}
	}

	for _, t := range existing {
		add(t)
	}
	for _, t := range incoming {
		add(t)
	}
	return result
}
