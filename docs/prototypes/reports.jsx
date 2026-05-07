/* Reports — landing list + Compliance overview matrix.
 *
 * Reports are read-only views of current artist state, sliced by:
 *   - row filters (universal filter bar — library, type, etc.)
 *   - column filters (per-column tri-state in the matrix header)
 *
 * No rule evaluation happens in Reports. Cells show factual presence/absence,
 * not "this fails rule X."
 */

const { useState: ufState, useEffect: ufEffect, useRef: ufRef } = React;

const cannedReports = [
  { id: "compliance",     label: "Compliance overview",   icon: "shield",     desc: "Field & ID coverage across your library", count: 1284 },
  { id: "unidentified",   label: "Unidentified artists",  icon: "warn",       desc: "No MusicBrainz ID set",                   count: 47 },
  { id: "image-coverage", label: "Image coverage",        icon: "image",      desc: "Thumb, fanart, logo, banner, backdrop",   count: 1284 },
  { id: "connections",    label: "Connection sync",       icon: "server",     desc: "Where each artist is registered",         count: 1284 },
  { id: "id-coverage",    label: "ID coverage",           icon: "key",        desc: "Per-provider linking status",             count: 1284 },
  { id: "metadata",       label: "Metadata coverage",     icon: "tag",        desc: "Bio, members, country, dates",            count: 1284 },
  { id: "stale",          label: "Stale records",        icon: "clock",      desc: "No metadata refresh in 90+ days",         count: 213 },
];

const savedReports = [
  { id: "weekly-review",  label: "Weekly review queue",   icon: "tag",        desc: "Saved Sat — Pink Floyd albums missing fanart", count: 12 },
  { id: "lidarr-only",    label: "In Lidarr but not Emby", icon: "tag",       desc: "For sync investigation",                  count: 8 },
];

/* ---------- Mock matrix data ---------- */

const matrixArtists = [
  { name: "Pink Floyd",         lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 1, nfo: 1 },
  { name: "Radiohead",          lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 0, audiodb: 1, discogs: 1, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 1, nfo: 0 },
  { name: "Aphex Twin",         lib: "Main",       type: "person", inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 0 }, ids: { mb: 1, audiodb: 1, discogs: 0, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 0, nfo: 1 },
  { name: "Boards of Canada",   lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 0, spotify: 1 }, bio: 1, thumb: 1, fanart: 1, nfo: 1 },
  { name: "King Crimson",       lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 1, spotify: 0 }, bio: 0, thumb: 1, fanart: 1, nfo: 1 },
  { name: "Yo La Tengo",        lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 0 }, ids: { mb: 1, audiodb: 0, discogs: 0, lastfm: 1, spotify: 1 }, bio: 1, thumb: 0, fanart: 1, nfo: 1 },
  { name: "Stereolab",          lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 1, nfo: 1 },
  { name: "Mogwai",             lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 1, nfo: 0 },
  { name: "Sigur Rós",          lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 1, audiodb: 0, discogs: 0, lastfm: 0, spotify: 1 }, bio: 0, thumb: 1, fanart: 0, nfo: 1 },
  { name: "Talk Talk",          lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 0 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 1, spotify: 0 }, bio: 1, thumb: 1, fanart: 1, nfo: 1 },
  { name: "Slowdive",           lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 1, spotify: 1 }, bio: 1, thumb: 0, fanart: 1, nfo: 1 },
  { name: "Cocteau Twins",      lib: "Main",       type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 1 }, ids: { mb: 1, audiodb: 1, discogs: 0, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 1, nfo: 1 },
  { name: "My Bloody Valentine", lib: "Main",      type: "group",  inSrc: { local: 1, emby: 1, jellyfin: 0, lidarr: 0 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 1, nfo: 1 },
  { name: "J. S. Bach",         lib: "Classical",  type: "person", inSrc: { local: 1, emby: 0, jellyfin: 1, lidarr: 0 }, ids: { mb: 1, audiodb: 1, discogs: 1, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 1, nfo: 1 },
  { name: "Glenn Gould",        lib: "Classical",  type: "person", inSrc: { local: 1, emby: 0, jellyfin: 1, lidarr: 0 }, ids: { mb: 1, audiodb: 0, discogs: 1, lastfm: 1, spotify: 1 }, bio: 1, thumb: 1, fanart: 0, nfo: 1 },
  { name: "Arvo Pärt",          lib: "Classical",  type: "person", inSrc: { local: 1, emby: 0, jellyfin: 1, lidarr: 0 }, ids: { mb: 0, audiodb: 0, discogs: 0, lastfm: 0, spotify: 0 }, bio: 0, thumb: 0, fanart: 0, nfo: 0 },
];

const idProviders = [
  { key: "mb",      label: "MusicBrainz" },
  { key: "audiodb", label: "TheAudioDB" },
  { key: "discogs", label: "Discogs" },
  { key: "lastfm",  label: "Last.fm" },
  { key: "spotify", label: "Spotify" },
];

const sourceList = [
  { key: "local",    label: "Local files", letter: "L", local: true },
  { key: "emby",     label: "Emby",        letter: "E" },
  { key: "jellyfin", label: "Jellyfin",    letter: "J" },
  { key: "lidarr",   label: "Lidarr",      letter: "Ld" },
];

/* ---------- Components ---------- */

function FieldState({ on, label }) {
  // tri-state? Reports are present/missing only — see canvas writeup.
  const cls = on ? "has" : "miss";
  return (
    <span className={`fs ${cls}`} role="img" aria-label={`${label} ${on ? "present" : "missing"}`}>
      <span className="ico">
        {on ? <Icon name="check" size={11}/> : <span style={{ width: 9, height: 1, background: "currentColor", display: "block" }}/>}
      </span>
    </span>
  );
}

function CoverageCell({ have, total, providerLabels }) {
  const pct = total === 0 ? 0 : have / total;
  const cls = total === 0 ? "none" : pct === 1 ? "full" : pct >= 0.6 ? "most" : pct > 0 ? "thin" : "none";
  const ariaLabel = total === 0
    ? "No providers configured"
    : `${have} of ${total} provider IDs linked${providerLabels ? ` — ${providerLabels.have.join(", ")}; missing ${providerLabels.miss.join(", ") || "none"}` : ""}`;
  return (
    <span className={`cov ${cls}`} title={ariaLabel} aria-label={ariaLabel}>
      <span className="bar"><i style={{ width: `${Math.max(8, pct * 100)}%` }}/></span>
      <span>{have}/{total}</span>
    </span>
  );
}

function SourceStrip({ inSrc, configured }) {
  // configured = which sources are configured workspace-wide.
  // inSrc[key] = 1 means the artist is registered in that source.
  const labelParts = sourceList
    .filter(s => configured.has(s.key))
    .map(s => `${inSrc[s.key] ? "in" : "not in"} ${s.label}`);
  return (
    <span className="src-strip" role="img" aria-label={`Sources: ${labelParts.join(", ")}`}>
      {sourceList.map(s => {
        if (!configured.has(s.key)) return null;
        const on = !!inSrc[s.key];
        return (
          <span
            key={s.key}
            className={`src ${s.local ? "local" : ""} ${on ? "on" : "off"}`}
            title={`${on ? "In" : "Not in"} ${s.label}`}
          >
            {s.letter}
          </span>
        );
      })}
    </span>
  );
}

function ColumnTri({ state, onClick, label }) {
  // state: null | "has" | "miss"
  const lbl =
    state === "has" ? `Filtering ${label} = present (click for missing, again to clear)` :
    state === "miss" ? `Filtering ${label} = missing (click to clear)` :
    `Filter by ${label} (click for present)`;
  return (
    <button
      className={`tri ${state || ""}`}
      onClick={(e) => { e.stopPropagation(); onClick(); }}
      title={lbl}
      aria-label={lbl}
    >
      {state === "has" && <><Icon name="check" size={9}/><span>has</span></>}
      {state === "miss" && <><Icon name="x" size={9}/><span>missing</span></>}
      {!state && <span style={{ opacity: 0.6 }}>filter</span>}
    </button>
  );
}

/* ---------- Reports landing ---------- */

function ReportsProposal({ density = "comfy", showAnnotations = false }) {
  const [active, setActive] = ufState("compliance");

  return (
    <div data-density={density} className="sw-anno" style={{ position: "relative" }}>
      <Sidebar active="reports" actionsCount={64}/>
      <div className="sw-main">
        <PageHead
          title="Reports"
          sub="The current state of your library — slice it by row, by column, save what you need."
          right={<>
            <button className="btn ghost"><Icon name="download" size={14}/> Export CSV</button>
            <button className="btn primary"><Icon name="plus" size={14}/> New report</button>
          </>}
        />

        <div className="sw-card" style={{ display: "grid", gridTemplateColumns: "260px minmax(0, 1fr)", minHeight: 620, padding: 0 }}>
          <nav style={{ background: "var(--sw-bg-raised)", borderRight: "1px solid var(--sw-line)", padding: 6, borderRadius: "14px 0 0 14px" }}>
            <div style={{ position: "relative", padding: 8 }}>
              <Icon name="search" size={13} style={{ position: "absolute", left: 18, top: "50%", transform: "translateY(-50%)", color: "var(--sw-ink-3)" }}/>
              <input className="input with-icon" placeholder="Filter reports…" style={{ height: 30 }}/>
            </div>
            <div className="sw-rep-list">
              <div className="group-label">Built-in</div>
              {cannedReports.map(r => (
                <div key={r.id} className={`item ${active === r.id ? "active" : ""}`} onClick={() => setActive(r.id)}>
                  <span className="ico"><Icon name={r.icon} size={14}/></span>
                  <div style={{ minWidth: 0 }}>
                    <div className="truncate" style={{ fontSize: 13, fontWeight: 500 }}>{r.label}</div>
                    <div className="truncate muted" style={{ fontSize: 11.5 }}>{r.desc}</div>
                  </div>
                  <span className="meta">{r.count.toLocaleString()}</span>
                </div>
              ))}
              <div className="group-label">Saved</div>
              {savedReports.map(r => (
                <div key={r.id} className="item" onClick={() => setActive(r.id)}>
                  <span className="ico"><Icon name={r.icon} size={14}/></span>
                  <div style={{ minWidth: 0 }}>
                    <div className="truncate" style={{ fontSize: 13, fontWeight: 500 }}>{r.label}</div>
                    <div className="truncate muted" style={{ fontSize: 11.5 }}>{r.desc}</div>
                  </div>
                  <span className="meta">{r.count.toLocaleString()}</span>
                </div>
              ))}
              <div style={{ padding: "12px 10px 0" }}>
                <button className="btn ghost sm" style={{ width: "100%", justifyContent: "center" }}><Icon name="plus" size={11}/> Save current as report</button>
              </div>
            </div>
          </nav>

          <div style={{ padding: "16px 18px", minWidth: 0 }}>
            {active === "compliance" ? <ComplianceMatrix /> : <ReportPlaceholder id={active}/>}
          </div>
        </div>

        <div className="muted" style={{ fontSize: 11.5, marginTop: 14, textAlign: "center", display: "flex", gap: 14, justifyContent: "center", alignItems: "center" }}>
          <span><span className="kbd">/</span> filter rows</span>
          <span><span className="kbd">f</span> column filters</span>
          <span><span className="kbd">⌘S</span> save view</span>
          <span><span className="kbd">⌘E</span> export CSV</span>
        </div>
      </div>

      {showAnnotations && (
        <>
          <Callout n={1} x={244} y={170} pos="left" w={220}>Compliance is now a canned report — first in the list.</Callout>
          <Callout n={2} x={520} y={170} pos="bottom" w={240}>Saved reports = full CRUD. Workspace or private (multi-user installs).</Callout>
          <Callout n={3} x={820} y={300} pos="right" tone="cool" w={220}>Per-column tri-state filter — click "has", click "missing", click clear.</Callout>
          <Callout n={4} x={520} y={420} pos="bottom" w={240}>"Sources" strip = configured connections + local. Greyed = not in that source.</Callout>
          <Callout n={5} x={680} y={420} pos="bottom" tone="cool" w={220}>"IDs" cell = n/m provider IDs linked, color graded by coverage.</Callout>
        </>
      )}
    </div>
  );
}

function ReportPlaceholder({ id }) {
  const r = [...cannedReports, ...savedReports].find(x => x.id === id);
  return (
    <div className="empty">
      <div className="ico"><Icon name={r?.icon || "reports"} /></div>
      <div style={{ fontSize: 14, fontWeight: 500 }}>{r?.label}</div>
      <div className="muted" style={{ fontSize: 12.5, marginTop: 4, maxWidth: 360 }}>
        {r?.desc} — same matrix shape as Compliance, different default columns.
      </div>
    </div>
  );
}

/* ---------- The matrix ---------- */

const ChartStripView = (props) => {
  const C = window.ChartStrip;
  return C ? <C {...props}/> : null;
};

function ComplianceMatrix() {
  const configuredSources = new Set(["local", "emby", "lidarr"]);
  const configuredProviders = idProviders; // all 5 in the demo
  const totalIDs = configuredProviders.length;

  const [colFilters, setColFilters] = ufState({}); // colKey -> "has"|"miss"|null
  const [sortKey, setSortKey] = ufState("name");
  const [sortDir, setSortDir] = ufState("asc");
  const [rowFilters, setRowFilters] = ufState({});

  function cycleColFilter(key) {
    setColFilters(prev => {
      const cur = prev[key];
      const nxt = cur == null ? "has" : cur === "has" ? "miss" : null;
      return { ...prev, [key]: nxt };
    });
  }

  // Apply column filters to rows (for the visible-row count) and apply the
  // Artist column sort so the chevron-toggle in the header actually moves
  // rows. `name` is the only sortable column today.
  const filtered = matrixArtists.filter(a => {
    for (const [k, v] of Object.entries(colFilters)) {
      if (!v) continue;
      const has = !!a[k];
      if (v === "has" && !has) return false;
      if (v === "miss" && has) return false;
    }
    return true;
  }).sort((a, b) => {
    const cmp = (a.name || "").localeCompare(b.name || "");
    return sortDir === "asc" ? cmp : -cmp;
  });

  return (
    <>
      <div className="row" style={{ marginBottom: 10, gap: 10, alignItems: "flex-start", flexWrap: "wrap" }}>
        <div className="flex-1" style={{ minWidth: 240 }}>
          <h2 style={{ margin: 0, fontSize: 17, fontWeight: 600, letterSpacing: "-0.01em" }}>Compliance overview</h2>
          <div className="muted" style={{ fontSize: 12.5, marginTop: 2 }}>
            Field & ID coverage across your library · 1,284 artists · {filtered.length} match filter · last refreshed 2m ago
            <button className="btn ghost sm" style={{ marginLeft: 8, height: 22, padding: "0 8px" }}><Icon name="clock" size={11}/> Refresh</button>
          </div>
        </div>
        <button className="btn ghost sm">Reset to default</button>
        <button className="btn ghost sm"><Icon name="plus" size={11}/> Add column</button>
        <button className="btn primary sm"><Icon name="check" size={11}/> Save as report</button>
      </div>

      <ChartStripView artists={filtered} totalIDs={totalIDs} />

      <UniversalFilterBar
        placeholder="Filter rows by name, MBID, file path…"
        scope={["All", "Group", "Person"]}
        activeScope="All"
        setActiveScope={() => {}}
        facets={[
          { key: "library", label: "Library", options: [
            { value: "main", label: "Main", count: 13 },
            { value: "classical", label: "Classical archive", count: 3 },
          ]},
          { key: "source", label: "Has source", options: [
            { value: "lidarr", label: "Lidarr", count: 9 },
            { value: "emby",   label: "Emby",   count: 13 },
          ]},
        ]}
        active={rowFilters}
        setActive={setRowFilters}
        resultCount={filtered.length}
      />

      <div style={{ marginTop: 14, overflowX: "auto", border: "1px solid var(--sw-line)", borderRadius: 10, background: "var(--sw-bg-raised)" }}>
        <table className="sw-matrix" style={{ minWidth: 1080 }}>
          <thead>
            <tr>
              <th className="sortable" onClick={() => setSortDir(d => d === "asc" ? "desc" : "asc")}>
                <span className="row" style={{ gap: 4 }}>Artist <Icon name="chevron" size={10} style={{ transform: sortDir === "asc" ? "rotate(90deg)" : "rotate(-90deg)" }}/></span>
              </th>
              <th>Library</th>
              <th>Sources</th>
              <th>IDs</th>
              <th>
                <span className="col-filter">Bio <ColumnTri state={colFilters.bio} onClick={() => cycleColFilter("bio")} label="Bio"/></span>
              </th>
              <th>
                <span className="col-filter">Thumb <ColumnTri state={colFilters.thumb} onClick={() => cycleColFilter("thumb")} label="Thumb"/></span>
              </th>
              <th>
                <span className="col-filter">Fanart <ColumnTri state={colFilters.fanart} onClick={() => cycleColFilter("fanart")} label="Fanart"/></span>
              </th>
              <th>
                <span className="col-filter">NFO <ColumnTri state={colFilters.nfo} onClick={() => cycleColFilter("nfo")} label="NFO"/></span>
              </th>
            </tr>
          </thead>
          <tbody>
            {filtered.map(a => {
              const haveIds = configuredProviders.filter(p => a.ids[p.key]).length;
              const missLabels = configuredProviders.filter(p => !a.ids[p.key]).map(p => p.label);
              const haveLabels = configuredProviders.filter(p => a.ids[p.key]).map(p => p.label);
              return (
                <tr key={a.name}>
                  <td className="sticky-l">
                    <div className="row" style={{ gap: 8 }}>
                      <div style={{ width: 24, height: 24, borderRadius: "50%", background: "linear-gradient(135deg,#475569,#1e293b)", display: "grid", placeItems: "center", fontSize: 10.5, fontWeight: 600, color: "var(--sw-ink-2)" }}>
                        {a.name.split(" ").map(s => s[0]).slice(0, 2).join("").toUpperCase()}
                      </div>
                      <span style={{ fontSize: 13, fontWeight: 500 }}>{a.name}</span>
                      <span className="muted" style={{ fontSize: 11 }}>· {a.type}</span>
                    </div>
                  </td>
                  <td className="muted" style={{ fontSize: 12 }}>{a.lib}</td>
                  <td><SourceStrip inSrc={a.inSrc} configured={configuredSources}/></td>
                  <td><CoverageCell have={haveIds} total={totalIDs} providerLabels={{ have: haveLabels, miss: missLabels }}/></td>
                  <td><FieldState on={a.bio}    label="Biography"/></td>
                  <td><FieldState on={a.thumb}  label="Thumb"/></td>
                  <td><FieldState on={a.fanart} label="Fanart"/></td>
                  <td><FieldState on={a.nfo}    label="NFO"/></td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      <div className="row" style={{ marginTop: 10, gap: 12, fontSize: 11.5, color: "var(--sw-ink-3)" }}>
        <span className="row" style={{ gap: 6 }}>
          <span className="cov full" style={{ padding: "1px 8px", fontSize: 10 }}><span className="bar"><i style={{ width: "100%" }}/></span>5/5</span>
          all linked
        </span>
        <span className="row" style={{ gap: 6 }}>
          <span className="cov most" style={{ padding: "1px 8px", fontSize: 10 }}><span className="bar"><i style={{ width: "70%" }}/></span>3/5</span>
          mostly
        </span>
        <span className="row" style={{ gap: 6 }}>
          <span className="cov thin" style={{ padding: "1px 8px", fontSize: 10 }}><span className="bar"><i style={{ width: "35%" }}/></span>2/5</span>
          thin
        </span>
        <span className="row" style={{ gap: 6 }}>
          <span className="cov none" style={{ padding: "1px 8px", fontSize: 10 }}><span className="bar"><i style={{ width: "8%" }}/></span>0/5</span>
          none
        </span>
        <span style={{ marginLeft: "auto" }}>
          IDs grades update when you toggle providers in <span className="mono">Settings → Metadata providers</span>.
        </span>
      </div>
    </>
  );
}

Object.assign(window, { ReportsProposal, ComplianceMatrix, FieldState, CoverageCell, SourceStrip });
