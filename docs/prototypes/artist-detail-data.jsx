/* Artist-detail mock data.
 *
 * Field-level shape: every value is wrapped in { value, source, conflicts?, finding?, manual?, locked? }
 *   - source     "musicbrainz" | "discogs" | "wikidata" | "fanart" | "spotify" | "nfo" | "manual"
 *   - conflicts  optional array of {provider, value} that disagreed with the resolved value
 *   - finding    optional {rule, severity:"err"|"warn"|"info", message, suggestedFix}
 *   - manual     true if a human overrode the value (source becomes "manual")
 *   - locked     true if rules should never auto-overwrite this field
 *
 * This shape lets the UI render a finding chip, a conflict popover, and a manual-override
 * badge inline next to any field, without the field renderer having to know which kind
 * of annotation to look up — they all live on the field itself.
 */

const F = (value, source = "musicbrainz", extras = {}) => ({ value, source, ...extras });

const ARTIST_DETAIL = {
  // Stillwater's internal artist identifier — used as the canonical key in
  // log lines (`artist_id:NNNN`), in deep-link URL params, and in the rules
  // engine. The mock value here matches the example in
  // `docs/milestone-55/05-logs.md` so the prototype's Logs deep-link demos
  // the actual contract, not a placeholder.
  id:          8821,
  // Header
  name:        F("Mastodon", "musicbrainz", {
    conflicts: [{ provider: "nfo", value: "Mastodon (FR)" }],
    finding: { rule: "name.canonical", title: "Name doesn't match MusicBrainz", severity: "warn",
               message: "artist.nfo says 'Mastodon (FR)' but MBID resolves to 'Mastodon' (Atlanta).",
               suggestedFix: "Rewrite artist.nfo with canonical name." }
  }),
  type:        F("group"),
  aliases:     F(["Mastadon", "MASTODON"], "musicbrainz"),
  image:       null,                                    // placeholder slot
  library:     F("Main", "local"),
  disambig:    F("American heavy metal band from Atlanta, Georgia", "musicbrainz"),

  // Identifiers
  identifiers: [
    { kind: "MBID",      value: "bc5e2ad6-0a4a-4d90-b911-e9ed7a8eb2a9", source: "musicbrainz", verified: true  },
    { kind: "ISNI",      value: "0000 0001 2096 5076",                 source: "musicbrainz", verified: true  },
    { kind: "Discogs",   value: "13608",                               source: "discogs",     verified: true  },
    { kind: "Spotify",   value: "1Dvfqq39HxvCJ3GvfeIFuT",              source: "spotify",     verified: true  },
    { kind: "Wikidata",  value: "Q220909",                             source: "wikidata",    verified: true  },
    { kind: "AllMusic",  value: null,                                  source: null,          verified: false, finding: { rule: "id.allmusic", title: "AllMusic ID not linked", severity: "info", message: "AllMusic ID not linked.", suggestedFix: "Run 'Match identifiers'." } },
  ],

  // Core metadata
  formed:     F("2000", "musicbrainz"),
  origin:     F("Atlanta, Georgia, US", "musicbrainz", {
    conflicts: [{ provider: "discogs", value: "Atlanta, US" }],
  }),
  ended:      F(null),
  members:    F(["Troy Sanders", "Brent Hinds", "Bill Kelliher", "Brann Dailor"], "musicbrainz"),
  genres:     F(["heavy metal", "progressive metal", "sludge metal"], "musicbrainz", {
    conflicts: [{ provider: "discogs", value: ["Heavy Metal", "Sludge", "Progressive Metal"] }],
  }),
  biography:  F(
    "Mastodon is an American heavy metal band from Atlanta, Georgia, formed in 2000. The group is composed of bassist Troy Sanders, guitarists Brent Hinds and Bill Kelliher, and drummer Brann Dailor, all of whom perform vocals.",
    "musicbrainz",
    { locked: true,
      finding: { rule: "bio.length", title: "Biography too short", severity: "warn",
                 message: "Biography is 247 chars; rule expects ≥ 400.",
                 suggestedFix: "Re-fetch from Wikidata or write manually." } }
  ),

  // Artwork — Kodi/Emby/Jellyfin/Plex artwork kinds Stillwater fetches per artist.
  // Names use Emby/Jellyfin terminology; per-platform aliases shown in upload modal.
  // Each kind groups one or more `items` and marks one as the primary (canonical for that kind).
  //   status: "ok" | "missing" | "flagged" | "error"
  //   - ok       at least one item fetched, primary passes rules
  //   - missing  no candidate found at any provider
  //   - flagged  primary fetched but a rule complains (see finding)
  //   - error    provider call failed; nothing to show
  // Each item:
  //   { id, source: "fanart"|"manual"|..., url, width, height, primary: bool }
  artwork: [
    { kind: "primary",  label: "Primary",
      role: "Square portrait used in lists & hero",
      aliases: { plex: "Poster", kodi: "thumb", embyjf: "Primary" },
      status: "ok",
      items: [
        { id: "p1", source: "fanart", url: "https://assets.fanart.tv/fanart/music/bc5e2ad6/artistthumb/mastodon-thumb.jpg", width: 1000, height: 1000, primary: true },
        { id: "p2", source: "fanart", url: "https://assets.fanart.tv/fanart/music/bc5e2ad6/artistthumb/mastodon-thumb-2.jpg", width: 1000, height: 1000, primary: false },
        { id: "p3", source: "fanart", url: "https://assets.fanart.tv/fanart/music/bc5e2ad6/artistthumb/mastodon-thumb-3.jpg", width: 1000, height: 1000, primary: false },
        { id: "p4", source: "manual", url: "https://assets.fanart.tv/fanart/music/bc5e2ad6/artistthumb/mastodon-thumb-4.jpg", width: 1200, height: 1200, primary: false },
      ],
      finding: null },
    { kind: "backdrop", label: "Backdrop",
      role: "Wide background image for detail views (cycled when multiple)",
      aliases: { plex: "Art", kodi: "fanart", embyjf: "Backdrop" },
      status: "error",
      items: [],
      finding: { rule: "image.required", title: "No artwork available", severity: "err",
                 message: "fanart.tv connection reset — couldn't fetch any backdrops for 3 hours.",
                 suggestedFix: "Wait for backoff to clear, or upload manually." } },
    { kind: "logo",     label: "Logo",
      role: "Transparent wordmark for overlays",
      aliases: { plex: "—", kodi: "clearlogo", embyjf: "Logo" },
      status: "flagged",
      items: [
        { id: "l1", source: "fanart", url: "https://assets.fanart.tv/fanart/music/bc5e2ad6/hdmusiclogo/mastodon-logo.png", width: 800, height: 600, primary: true },
        { id: "l2", source: "fanart", url: "https://assets.fanart.tv/fanart/music/bc5e2ad6/musiclogo/mastodon-logo-2.png",  width: 1024, height: 410, primary: false },
      ],
      finding: { rule: "image.aspect", title: "Logo aspect ratio off", severity: "warn",
                 message: "Primary logo is 800×600 but rule expects ≥ 1000×400 with 2.5:1 aspect.",
                 suggestedFix: "Promote the wider candidate, or upload a new one." } },
    { kind: "banner",   label: "Banner",
      role: "Wide thin header (1000×185)",
      aliases: { plex: "—", kodi: "banner", embyjf: "Banner" },
      status: "missing",
      items: [],
      finding: null },
  ],

  // Provider links — what we know from each source, status of the connection
  providers: [
    { id: "musicbrainz", name: "MusicBrainz",  status: "linked",   lastFetched: "2 hours ago",  url: "https://musicbrainz.org/artist/bc5e2ad6-0a4a-4d90-b911-e9ed7a8eb2a9" },
    { id: "discogs",     name: "Discogs",      status: "linked",   lastFetched: "2 hours ago",  url: "https://www.discogs.com/artist/13608" },
    { id: "wikidata",    name: "Wikidata",     status: "linked",   lastFetched: "2 hours ago",  url: "https://www.wikidata.org/wiki/Q220909" },
    { id: "spotify",     name: "Spotify",      status: "linked",   lastFetched: "1 hour ago",   url: "https://open.spotify.com/artist/1Dvfqq39HxvCJ3GvfeIFuT" },
    { id: "fanart",      name: "fanart.tv",    status: "error",    lastFetched: "3 hours ago",  error: "Image fetch failed: connection reset (3 retries exhausted)" },
    { id: "lastfm",      name: "Last.fm",      status: "unlinked", lastFetched: null },
    { id: "allmusic",    name: "AllMusic",     status: "unlinked", lastFetched: null },
  ],

  // Open findings on this artist (the embedded section uses the row shape from the Findings page)
  findings: [
    { id: "f1", rule: "name.canonical", title: "Name doesn't match MusicBrainz", severity: "warn", field: "name",
      message: "artist.nfo says 'Mastodon (FR)' but MBID resolves to 'Mastodon' (Atlanta).",
      suggestedFix: "Rewrite artist.nfo with canonical name.", evidence: "/music/Mastodon/artist.nfo:3" },
    { id: "f2", rule: "bio.length", title: "Biography too short", severity: "warn", field: "biography",
      message: "Biography is 247 chars; rule expects ≥ 400.",
      suggestedFix: "Re-fetch from Wikidata, or write manually.", evidence: null },
    { id: "f3", rule: "id.allmusic", title: "AllMusic ID not linked", severity: "info", field: "identifiers",
      message: "AllMusic ID not linked.",
      suggestedFix: "Run 'Match identifiers' to attempt automatic linking.", evidence: null },
    { id: "f4", rule: "image.aspect", title: "Logo aspect ratio off", severity: "warn", field: "image",
      message: "fanart logo is 800×600 but rule expects ≥ 1000×400 with 2.5:1 aspect.",
      suggestedFix: "Pick a wider logo from fanart.tv, or upload one manually.", evidence: "fanart.tv:logo:#22411" },
    { id: "f5", rule: "image.required", title: "No artwork available", severity: "err", field: "image",
      message: "fanart.tv connection reset — couldn't fetch any imagery for 3 hours.",
      suggestedFix: "Wait for backoff to clear, or upload images manually.", evidence: "logs:provider.fanart:err" },
  ],

  // Recent activity — last 5 events, deep-link to Logs for full history
  activity: [
    { time: "12 min ago",  kind: "scan",     who: "system",       message: "Re-scanned · 0 changes" },
    { time: "2 hours ago", kind: "fetch",    who: "system",       message: "Re-fetched MusicBrainz, Discogs, Wikidata, Spotify" },
    { time: "2 hours ago", kind: "error",    who: "system",       message: "fanart.tv image fetch failed (connection reset)" },
    { time: "yesterday",   kind: "manual",   who: "alex@dox.az",  message: "Set genres manually (was: 'metal'; now: 'heavy metal, progressive metal, sludge metal')" },
    { time: "3 days ago",  kind: "resolve",  who: "alex@dox.az",  message: "Resolved finding 'id.discogs not linked' by running Match identifiers" },
  ],

  // Compliance / coverage summary (mirrors the artists list row)
  compliance: {
    score:    62,
    fields:   { biography: true, primary: true, backdrop: false, logo: false, banner: false, nfo: true },
    idsHave:  5, idsTotal: 6,
    findings: { err: 1, warn: 3, info: 1 },
    sources:  { local: true, emby: true, lidarr: false },
  },

  // Audit metadata
  lastScan:    "12 min ago",
  lastChange:  "yesterday",
  createdAt:   "joined library 2 years ago",
};

Object.assign(window, { ARTIST_DETAIL });
