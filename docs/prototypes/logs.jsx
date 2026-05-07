/* Logs — proposed v2 screen.
 *
 * Renders a structured log stream backed by the real LogEntry shape from
 * internal/logging/ringbuffer.go (Time, Level, Message, Component, Source, Attrs).
 *
 * Locked design decisions (see docs/milestone-55/05-logs.md):
 *  - Smart-follow live tail (auto-pause on scroll-up, "↓ N new" pill)
 *  - Errors-first tally chip above the stream (subtle, click-to-filter)
 *  - Hybrid attrs: pinned chips inline, rest behind chevron
 *  - Density toggle (data-density on root, same key as Artists)
 *  - Time-range picker shaped for rotated-log search; greyed pre-backend
 *  - Filter chips clickable on every row; click component → filter component
 *  - Open in bug report (prefills GitHub issue with redacted last 200 lines)
 */

const { useState: lState, useEffect: lEffect, useRef: lRef, useMemo: lMemo } = React;

const PINNED_ATTRS = ["artist_id", "request_id", "provider", "error", "rule"];

/* ---------- mock data — modeled exactly on internal/logging/ringbuffer.LogEntry ---------- */

const NOW = new Date("2026-05-06T14:22:18.412Z");
function ago(ms) { return new Date(NOW.getTime() - ms); }

const LOG_ENTRIES = [
  { time: ago(0),       level: "error", component: "rules",                 source: "rules.go:184",      message: 'Rule body too long for "Mastodon"',                                          attrs: { artist_id: 8821, rule: "bio.length", limit: 4096, actual: 5210 } },
  { time: ago(411),     level: "warn",  component: "provider.musicbrainz",  source: "musicbrainz.go:92", message: "429 backoff 30s",                                                              attrs: { provider: "musicbrainz", retry: 1, request_id: "r-7c4f1a", url: "/ws/2/artist/4d5447db" } },
  { time: ago(998),     level: "info",  component: "scanner",               source: "scanner.go:201",    message: 'Started scan for library "Main"',                                              attrs: { library: 1, artists: 2847 } },
  { time: ago(1402),    level: "debug", component: "watcher",               source: "probe.go:142",      message: "fsnotify: write /music/Pink Floyd/Wish You Were Here/02 Have a Cigar.flac",     attrs: { event: "write", path: "/music/Pink Floyd/Wish You Were Here/02 Have a Cigar.flac" } },
  { time: ago(2200),    level: "info",  component: "rules",                 source: "engine.go:64",      message: "Evaluated 38 rules across 1,284 artists",                                      attrs: { duration_ms: 412, violations: 64 } },
  { time: ago(3010),    level: "error", component: "provider.fanart",       source: "fanart.go:118",     message: "Image fetch failed: connection reset",                                          attrs: { artist_id: 4012, provider: "fanart.tv", error: "connection reset by peer", request_id: "r-7c4f12" } },
  { time: ago(4001),    level: "info",  component: "scanner",               source: "scanner.go:412",    message: "Indexed 47 new artists, 612 new releases",                                     attrs: { library: 1, new_artists: 47, new_releases: 612 } },
  { time: ago(4500),    level: "warn",  component: "scraper",               source: "scraper.go:88",     message: "NFO mismatch: artist.nfo says Mastodon, MBID maps to Mastodon (FR)",            attrs: { artist_id: 8821, mbid: "1c0e5e6f-3c70-49b5-a98f-9e9e93fa3a70" } },
  { time: ago(5300),    level: "info",  component: "http",                  source: "router.go:54",      message: "GET /api/v1/artists?library=1&page=2 200 124ms",                              attrs: { request_id: "r-7c4f0a", status: 200, duration_ms: 124, user_id: 1 } },
  { time: ago(6100),    level: "debug", component: "rules",                 source: "engine.go:128",     message: "Skipping locked field 'biography' for Pink Floyd",                              attrs: { artist_id: 901, rule: "bio.length", field: "biography" } },
  { time: ago(7400),    level: "warn",  component: "provider.musicbrainz",  source: "musicbrainz.go:92", message: "503 backoff 60s",                                                              attrs: { provider: "musicbrainz", retry: 2, request_id: "r-7c4f08" } },
  { time: ago(8800),    level: "error", component: "watcher",               source: "probe.go:201",     message: "Watch handle exhausted: ENOSPC, falling back to poll",                          attrs: { error: "ENOSPC", path: "/music", fallback: "poll" } },
  { time: ago(10200),   level: "info",  component: "scanner",               source: "scanner.go:412",    message: "Indexed 12 new artists, 84 new releases",                                      attrs: { library: 2, new_artists: 12, new_releases: 84 } },
  { time: ago(12100),   level: "info",  component: "imagebridge",           source: "bridge.go:74",      message: "Cached thumb 500x500 for Radiohead",                                            attrs: { artist_id: 2244, kind: "thumb", w: 500, h: 500 } },
  { time: ago(15040),   level: "warn",  component: "rules",                 source: "engine.go:204",     message: "Rule 'image.aspect' violated for Boards of Canada",                             attrs: { artist_id: 4012, rule: "image.aspect", expected: "16:9", actual: "4:3" } },
  { time: ago(18900),   level: "debug", component: "http",                  source: "router.go:54",      message: "POST /api/v1/notifications/v1/fix 200 41ms",                                  attrs: { request_id: "r-7c4ef9", status: 200, duration_ms: 41 } },
  { time: ago(22400),   level: "info",  component: "publish",               source: "publish.go:31",     message: "Wrote artist.nfo for Slowdive",                                                 attrs: { artist_id: 33, path: "/music/Slowdive/artist.nfo" } },
  { time: ago(28100),   level: "error", component: "provider.musicbrainz",  source: "musicbrainz.go:204", message: "Parse error: invalid XML at offset 4214",                                       attrs: { provider: "musicbrainz", artist_id: 6611, error: "EOF", request_id: "r-7c4ef0" } },
  { time: ago(34500),   level: "info",  component: "scanner",               source: "scanner.go:611",    message: "Scan complete in 31.4s",                                                       attrs: { library: 1, duration_ms: 31412, total_artists: 2847 } },
];

/* ---------- helpers ---------- */

const LEVEL_ORDER = ["debug", "info", "warn", "error"];
const LEVEL_LABEL = { debug: "DEBUG", info: "INFO", warn: "WARN", error: "ERROR" };

function fmtTime(d) {
  const hh = String(d.getUTCHours()).padStart(2, "0");
  const mm = String(d.getUTCMinutes()).padStart(2, "0");
  const ss = String(d.getUTCSeconds()).padStart(2, "0");
  const ms = String(d.getUTCMilliseconds()).padStart(3, "0");
  return `${hh}:${mm}:${ss}.${ms}`;
}

function levelTone(lvl) {
  switch (lvl) {
    case "error": return "err";
    case "warn":  return "warn";
    case "info":  return "info";
    case "debug": return "debug";
    default: return "info";
  }
}

function fmtAttrValue(v) {
  if (typeof v === "string") return v;
  if (typeof v === "number") return v.toLocaleString();
  if (v == null) return "—";
  return JSON.stringify(v);
}

function partitionAttrs(attrs) {
  const pinned = [];
  const rest = [];
  if (!attrs) return { pinned, rest };
  for (const key of PINNED_ATTRS) if (key in attrs) pinned.push([key, attrs[key]]);
  for (const [k, v] of Object.entries(attrs)) if (!PINNED_ATTRS.includes(k)) rest.push([k, v]);
  return { pinned, rest };
}

/* ---------- subcomponents ---------- */

function LevelPill({ level, onClick }) {
  return (
    <button
      type="button"
      className={`sw-log-level sev-${levelTone(level)}`}
      onClick={(e) => { e.stopPropagation(); onClick && onClick(level); }}
      title={`Filter to ${LEVEL_LABEL[level]}`}
    >
      {LEVEL_LABEL[level]}
    </button>
  );
}

function ComponentChip({ name, onClick }) {
  return (
    <button
      type="button"
      className="sw-log-component"
      onClick={(e) => { e.stopPropagation(); onClick && onClick(name); }}
      title={`Filter to component:${name}`}
    >
      {name}
    </button>
  );
}

function AttrChip({ k, v, deepLink, onClick, muted }) {
  const display = fmtAttrValue(v);
  return (
    <button
      type="button"
      className={`sw-log-attr ${muted ? "muted" : ""} ${deepLink ? "linkish" : ""}`}
      onClick={(e) => { e.stopPropagation(); onClick && onClick(k, v); }}
      title={deepLink ? `Filter by ${k}:${display} · ⌘-click to open artist` : `Filter by ${k}:${display}`}
    >
      <span className="k">{k}</span><span className="sep">:</span><span className="v">{display}</span>
    </button>
  );
}

function LogRow({ entry, onComponent, onLevel, onAttr, density }) {
  const [open, setOpen] = lState(false);
  const { pinned, rest } = lMemo(() => partitionAttrs(entry.attrs), [entry.attrs]);
  const hasMore = rest.length > 0 || !!entry.source;

  function copyLine(e) {
    e.stopPropagation();
    const parts = [
      fmtTime(entry.time),
      LEVEL_LABEL[entry.level],
      entry.component,
      entry.message,
    ];
    if (entry.attrs) {
      for (const [k, v] of Object.entries(entry.attrs)) parts.push(`${k}=${fmtAttrValue(v)}`);
    }
    navigator.clipboard?.writeText(parts.join("\t"));
  }

  return (
    <div className={`sw-log-row sev-${levelTone(entry.level)} ${open ? "open" : ""}`}>
      <button
        type="button"
        className="sw-log-chev"
        aria-label={hasMore ? (open ? "Collapse details" : "Expand details") : "No additional details"}
        disabled={!hasMore}
        onClick={() => hasMore && setOpen(o => !o)}
      >
        <Icon name="chevron" size={11} />
      </button>
      <span className="sw-log-ts">{fmtTime(entry.time)}</span>
      <LevelPill level={entry.level} onClick={onLevel} />
      <ComponentChip name={entry.component} onClick={onComponent} />
      <span className="sw-log-msg" title={entry.message}>{entry.message}</span>
      <span className="sw-log-attrs">
        {pinned.map(([k, v]) => (
          <AttrChip key={k} k={k} v={v} deepLink={k === "artist_id"} onClick={onAttr} />
        ))}
      </span>
      <button type="button" className="sw-log-copy" onClick={copyLine} title="Copy this line">
        Copy
      </button>

      {open && (
        <div className="sw-log-detail">
          {rest.length > 0 && (
            <div className="row" style={{ flexWrap: "wrap", gap: 6 }}>
              {rest.map(([k, v]) => <AttrChip key={k} k={k} v={v} muted onClick={onAttr} />)}
            </div>
          )}
          {entry.source && (
            <div className="sw-log-source">
              <span className="muted">source</span>
              <code>{entry.source}</code>
              <button type="button" className="link" onClick={() => navigator.clipboard?.writeText(entry.source)}>copy</button>
            </div>
          )}
          <details className="sw-log-json">
            <summary>Show JSON</summary>
            <pre>{JSON.stringify({
              time: entry.time.toISOString(),
              level: entry.level,
              component: entry.component,
              source: entry.source,
              message: entry.message,
              attrs: entry.attrs,
            }, null, 2)}</pre>
          </details>
        </div>
      )}
    </div>
  );
}

/* ---------- main screen ---------- */

function ComponentsPopover({ all, selected, onToggle, onClear }) {
  const [open, setOpen] = lState(false);
  const ref = lRef(null);
  lEffect(() => {
    function onDoc(e) { if (open && ref.current && !ref.current.contains(e.target)) setOpen(false); }
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);
  const count = selected.size;
  return (
    <div className="sw-log-components-pop" ref={ref}>
      <button
        type="button"
        className={`sw-log-components-trigger ${count > 0 ? "active" : ""}`}
        onClick={() => setOpen(o => !o)}
      >
        <Icon name="filter" size={11}/>
        <span>Components</span>
        {count > 0 && <span className="count">{count}</span>}
        <Icon name="chevron-down" size={10}/>
      </button>
      {open && (
        <div className="sw-log-components-panel">
          <div className="sw-log-components-head">
            <span>Filter to components</span>
            {count > 0 && <button type="button" className="link" onClick={onClear}>clear</button>}
          </div>
          <div className="sw-log-components-list">
            {all.map(name => (
              <button
                type="button"
                key={name}
                className={`sw-log-component-toggle ${selected.has(name) ? "on" : ""}`}
                onClick={() => onToggle(name)}
              >
                {selected.has(name) && <Icon name="check" size={10}/>}
                {name}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function LogsProposal({ density = "comfy", showAnnotations = false }) {
  const [d, setDensity] = lState(density);
  const [follow, setFollow] = lState(true);             // smart-follow state
  const [streaming, setStreaming] = lState(true);       // would be true while SSE connected
  const [range, setRange] = lState("1h");
  const [levels, setLevels] = lState(new Set(LEVEL_ORDER));   // multi-select
  const [components, setComponents] = lState(new Set());      // include set
  // Seed the search from `?artist_id=NNNN` (or generic `?q=...`) so deep-links
  // from artist-detail and other surfaces land on a filtered stream. Matches
  // the inbound deep-link contract documented in docs/milestone-55/05-logs.md.
  const [search, setSearch] = lState(() => {
    if (typeof window === "undefined") return "";
    const params = new URLSearchParams(window.location.search);
    const artistId = params.get("artist_id");
    if (artistId) return `artist_id:${artistId}`;
    return params.get("q") || "";
  });
  const [pendingNew, setPendingNew] = lState(0);              // for the "↓ N new" pill mock
  const streamRef = lRef(null);

  const ALL_COMPONENTS = lMemo(() => {
    const s = new Set();
    for (const e of LOG_ENTRIES) s.add(e.component);
    return [...s].sort();
  }, []);

  const filtered = lMemo(() => {
    const q = search.trim().toLowerCase();
    return LOG_ENTRIES.filter(e => {
      if (!levels.has(e.level)) return false;
      if (components.size > 0 && !components.has(e.component)) return false;
      if (q) {
        // Match against message + component AND the attrs (rendered as
        // `key:value` chips). Without this, clicking an attr chip
        // populates the search box with `artist_id:8821` and the predicate
        // immediately empties the stream.
        const attrText = e.attrs
          ? Object.entries(e.attrs).map(([k, v]) => `${k}:${fmtAttrValue(v)}`).join(" ").toLowerCase()
          : "";
        if (!(
          e.message.toLowerCase().includes(q) ||
          e.component.toLowerCase().includes(q) ||
          attrText.includes(q)
        )) return false;
      }
      return true;
    });
  }, [levels, components, search]);

  // Tally chip for last hour, errors+warns. Subtle, click-to-filter.
  const tally = lMemo(() => {
    const oneHour = 60 * 60 * 1000;
    let err = 0, warn = 0;
    for (const e of LOG_ENTRIES) {
      const age = NOW - e.time;
      if (age > oneHour) continue;
      if (e.level === "error") err++;
      else if (e.level === "warn") warn++;
    }
    return { err, warn };
  }, []);

  function toggleLevel(lvl) {
    setLevels(prev => {
      const next = new Set(prev);
      if (next.size === LEVEL_ORDER.length) {
        // first interaction: switch to single-select-from-here mental model
        return new Set([lvl]);
      }
      if (next.has(lvl)) next.delete(lvl); else next.add(lvl);
      if (next.size === 0) return new Set(LEVEL_ORDER); // never end up with no levels
      return next;
    });
  }

  function setOnlyLevel(lvl) { setLevels(new Set([lvl])); }

  function toggleComponent(name) {
    setComponents(prev => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name); else next.add(name);
      return next;
    });
  }

  function clearFilters() {
    setLevels(new Set(LEVEL_ORDER));
    setComponents(new Set());
    setSearch("");
  }

  function onAttrFilter(k, v) {
    // Mock affordance: in real app, this would push to filter URL state.
    // Here we just append to search to make the click feel responsive.
    setSearch(`${k}:${fmtAttrValue(v)}`);
  }

  function onScroll() {
    const el = streamRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
    if (!atBottom && follow) { setFollow(false); setPendingNew(3); }
    if (atBottom && !follow) { setFollow(true); setPendingNew(0); }
  }

  function jumpToBottom() {
    const el = streamRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
    setFollow(true);
    setPendingNew(0);
  }

  const filtersActive = levels.size !== LEVEL_ORDER.length || components.size > 0 || search.trim().length > 0;

  return (
    <div data-density={d} className="sw-anno" style={{ position: "relative" }}>
      <Sidebar active="logs" actionsCount={64} />
      <div className="sw-main sw-logs-main">
        <PageHead
          title="Logs"
          sub="Live tail · last 500 lines retained in memory"
          right={<>
            <button
              type="button"
              className={`btn ghost sw-log-live ${streaming ? "on" : "off"}`}
              onClick={() => setStreaming(s => !s)}
              title={streaming ? "Disconnect live tail" : "Connect live tail"}
            >
              <span className={`sw-log-live-dot ${streaming ? (follow ? "follow" : "pause") : "off"}`} />
              {streaming ? (follow ? "Streaming" : "Paused") : "Off"}
            </button>
            <button type="button" className="btn ghost" title="Copy current view as text"><Icon name="download" size={13}/> Copy</button>
            <button type="button" className="btn ghost" title="Open in bug report"><Icon name="bolt" size={13}/> Bug report</button>
            <div className="sw-log-density">
              <button type="button" className={d === "comfy" ? "on" : ""} onClick={() => setDensity("comfy")} title="Comfortable density">
                <Icon name="logs" size={12}/>
              </button>
              <button type="button" className={d === "compact" ? "on" : ""} onClick={() => setDensity("compact")} title="Dense density">
                <Icon name="terminal" size={12}/>
              </button>
            </div>
          </>}
        />

        {/* Filter strip */}
        <div className="sw-log-filters">
          <div className="sw-log-search">
            <Icon name="search" size={13}/>
            <input
              type="text"
              placeholder="Search messages, components, attrs (e.g. artist_id:4012)…"
              value={search}
              onChange={e => setSearch(e.target.value)}
            />
            {search && <button type="button" className="x" onClick={() => setSearch("")} aria-label="Clear search"><Icon name="x" size={11}/></button>}
          </div>

          <div className="sw-log-levels">
            {LEVEL_ORDER.map(lvl => (
              <button
                type="button"
                key={lvl}
                className={`sw-log-level-toggle sev-${levelTone(lvl)} ${levels.has(lvl) ? "on" : ""}`}
                onClick={() => toggleLevel(lvl)}
              >
                {LEVEL_LABEL[lvl]}
              </button>
            ))}
          </div>

          <div className="sw-log-range">
            {[
              { v: "1h", label: "1h" },
              { v: "24h", label: "24h", rotated: true },
              { v: "7d", label: "7d", rotated: true },
              { v: "custom", label: "Custom…", rotated: true },
            ].map(r => (
              <button
                type="button"
                key={r.v}
                className={`${range === r.v ? "on" : ""} ${r.rotated ? "rotated" : ""}`}
                onClick={() => setRange(r.v)}
                title={r.rotated ? "Rotated-log search not yet available — backend follow-up." : `Last ${r.label}`}
                disabled={r.rotated}
              >
                {r.label}
              </button>
            ))}
          </div>
        </div>

        {/* Tally + active components row */}
        <div className="sw-log-tally-row">
          <div className="sw-log-tally">
            <button
              type="button"
              className="tally err"
              onClick={() => setOnlyLevel("error")}
              disabled={tally.err === 0}
              title="Filter to errors only"
            >
              <span className="dot" /> {tally.err} {tally.err === 1 ? "error" : "errors"}
            </button>
            <button
              type="button"
              className="tally warn"
              onClick={() => setOnlyLevel("warn")}
              disabled={tally.warn === 0}
              title="Filter to warnings only"
            >
              <span className="dot" /> {tally.warn} {tally.warn === 1 ? "warning" : "warnings"}
            </button>
            <span className="muted" style={{ fontSize: 12 }}>in last hour</span>
          </div>

          <ComponentsPopover
            all={ALL_COMPONENTS}
            selected={components}
            onToggle={toggleComponent}
            onClear={() => setComponents(new Set())}
          />

          {filtersActive && (
            <button type="button" className="link" onClick={clearFilters} style={{ marginLeft: "auto" }}>
              Clear filters
            </button>
          )}
        </div>

        {/* Stream */}
        <div className="sw-log-stream-wrap">
          <div
            className="sw-log-stream"
            ref={streamRef}
            onScroll={onScroll}
          >
            {filtered.length === 0 ? (
              <div className="sw-log-empty">
                <div>No log lines match these filters.</div>
                <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
                  Live tail still active — new matching lines will appear here.
                </div>
                <button type="button" className="btn ghost sm" style={{ marginTop: 10 }} onClick={clearFilters}>
                  Clear filters
                </button>
              </div>
            ) : (
              filtered.map((entry, i) => (
                <LogRow
                  key={i}
                  entry={entry}
                  density={d}
                  onComponent={toggleComponent}
                  onLevel={setOnlyLevel}
                  onAttr={onAttrFilter}
                />
              ))
            )}
          </div>

          {!follow && pendingNew > 0 && (
            <button type="button" className="sw-log-jump" onClick={jumpToBottom}>
              <Icon name="chevron-down" size={12}/> {pendingNew} new line{pendingNew === 1 ? "" : "s"} — jump to bottom
            </button>
          )}
        </div>

        {/* Footer hints */}
        <div className="muted" style={{ fontSize: 11.5, marginTop: 14, display: "flex", gap: 14, justifyContent: "center", alignItems: "center", flexWrap: "wrap" }}>
          <span><span className="kbd">/</span> search</span>
          <span><span className="kbd">f</span> follow</span>
          <span><span className="kbd">c</span> copy line</span>
          <span><span className="kbd">g</span><span className="kbd">l</span> logs</span>
          <span><span className="kbd">?</span> shortcuts</span>
        </div>
      </div>

      {showAnnotations && (
        <>
          <Callout n={1} x={300} y={130} pos="left" w={260}>Smart-follow: pill turns yellow if you scroll up; ↓ N new pops up.</Callout>
          <Callout n={2} x={300} y={236} pos="left" tone="cool" w={260}>Time range is shaped for rotated-log search; greyed-out until backend lands.</Callout>
          <Callout n={3} x={300} y={310} pos="left" w={260}>Errors-first tally is subtle: a chip you click to focus, not a default filter.</Callout>
          <Callout n={4} x={300} y={460} pos="left" tone="cool" w={260}>Pinned attrs inline; chevron reveals the rest + source + raw JSON.</Callout>
        </>
      )}
    </div>
  );
}

Object.assign(window, { LogsProposal, LogRow, LevelPill, ComponentChip, AttrChip });
