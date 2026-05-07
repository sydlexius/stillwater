/* Dashboard — proposal vs current */

const dashFacets = [
  { key: "severity", label: "Severity", options: [
    { value: "error",   label: "Errors",   count: 12 },
    { value: "warning", label: "Warnings", count: 38 },
    { value: "info",    label: "Info",     count: 14 },
  ]},
  { key: "category", label: "Category", options: [
    { value: "image",    label: "Images",   count: 31 },
    { value: "metadata", label: "Metadata", count: 22 },
    { value: "nfo",      label: "NFO files", count: 11 },
  ]},
  { key: "library", label: "Library", options: [
    { value: "main",      label: "Main library",       count: 56 },
    { value: "classical", label: "Classical archive",  count: 8 },
  ]},
  { key: "fixable", label: "Auto-fixable", options: [
    { value: "yes", label: "Can fix in one click", count: 41 },
    { value: "no",  label: "Needs review",         count: 23 },
  ]},
];

const dashViolations = [
  { id: "v1",  cat: "image", sev: "warn", who: "Pink Floyd",         msg: "Thumb is 320×320, below the 500×500 minimum",   action: "Fetch image",    initials: "PF" },
  { id: "v2",  cat: "nfo",   sev: "err",  who: "Radiohead",          msg: "artist.nfo missing MusicBrainz ID",              action: "Search MBID",   initials: "RH" },
  { id: "v3",  cat: "meta",  sev: "warn", who: "King Crimson",       msg: "Biography is empty",                              action: "Fetch metadata", initials: "KC" },
  { id: "v4",  cat: "image", sev: "err",  who: "Boards of Canada",   msg: "fanart.jpg has wrong aspect (4:3, expected 16:9)", action: "Fetch image",  initials: "BC" },
  { id: "v5",  cat: "nfo",   sev: "info", who: "Aphex Twin",         msg: "Three extraneous .nfo files in artist directory",  action: "Review",      initials: "AT" },
  { id: "v6",  cat: "image", sev: "warn", who: "Yo La Tengo",        msg: "logo.png missing transparent padding (10 px)",     action: "Fetch image",  initials: "YT" },
  { id: "v7",  cat: "meta",  sev: "info", who: "Stereolab",          msg: "Members list out of date with MusicBrainz",        action: "Fetch metadata", initials: "SL" },
];

const recent = [
  { who: "Talk Talk",       field: "Biography", kind: "set",     when: "2m",  src: "fanart.tv" },
  { who: "Slowdive",        field: "Thumb",     kind: "changed", when: "12m", src: "manual" },
  { who: "Cocteau Twins",   field: "Members",   kind: "set",     when: "1h",  src: "musicbrainz" },
  { who: "My Bloody V.",    field: "Banner",    kind: "cleared", when: "3h",  src: "manual" },
  { who: "Mogwai",          field: "Logo",      kind: "set",     when: "5h",  src: "fanart.tv" },
  { who: "Sigur Rós",       field: "fanart",    kind: "changed", when: "1d",  src: "manual" },
];

function HealthRing({ score, size = 64, stroke = 6 }) {
  const r = (size - stroke) / 2;
  const c = 2 * Math.PI * r;
  const cls = score >= 80 ? "ok" : score >= 50 ? "warn" : "err";
  return (
    <svg className="health-ring" width={size} height={size}>
      <circle className="track" cx={size / 2} cy={size / 2} r={r} fill="none" strokeWidth={stroke} />
      <circle
        className={`arc ${cls}`}
        cx={size / 2} cy={size / 2} r={r} fill="none" strokeWidth={stroke}
        strokeDasharray={c} strokeDashoffset={c - (score / 100) * c}
        strokeLinecap="round" transform={`rotate(-90 ${size / 2} ${size / 2})`}
      />
      <text x="50%" y="52%" textAnchor="middle" dominantBaseline="middle" fill="var(--sw-ink)" fontSize="14" fontWeight="600" fontFamily="var(--sw-mono)">
        {Math.round(score)}
      </text>
    </svg>
  );
}

function StatCard({ label, value, trend, accent }) {
  return (
    <div className="sw-stat" style={{ borderColor: accent ? `${accent}40` : undefined }}>
      <div className="label">{label}</div>
      <div className="val">{value}</div>
      {trend && <div className="trend">{trend}</div>}
    </div>
  );
}

function ActionRow({ v, dense = false }) {
  const sevMap = { err: "err", warn: "warn", info: "info" };
  return (
    <div className={`sw-action cat-${v.cat === "meta" ? "meta" : v.cat}`} style={{ paddingTop: dense ? 6 : 8, paddingBottom: dense ? 6 : 8 }}>
      <input type="checkbox" />
      <div className="av">{v.initials}</div>
      <div className="flex-1">
        <div className="row" style={{ gap: 6 }}>
          <span className="who">{v.who}</span>
          <span className={`sev ${sevMap[v.sev]}`}><span className="dot"></span>{v.sev === "err" ? "Error" : v.sev === "warn" ? "Warning" : "Info"}</span>
          <span className="muted mono" style={{ fontSize: 11 }}>{v.cat === "image" ? "image" : v.cat === "nfo" ? "nfo" : "metadata"}</span>
        </div>
        <div className="what truncate">{v.msg}</div>
      </div>
      <div className="row">
        <button className="btn ghost sm">Dismiss</button>
        <button className="btn primary sm">{v.action}</button>
      </div>
    </div>
  );
}

function DashboardProposal({ density = "comfy", layout = "rail", showAnnotations = false }) {
  const [activeFacets, setActiveFacets] = ufState({ severity: "warning" });
  const [scope, setScope] = ufState("Needs action");

  return (
    <div data-density={density} data-dash-layout={layout} className="sw-anno" style={{ position: "relative" }}>
      <Sidebar active="dashboard" actionsCount={64} />
      <div className="sw-main">
        <PageHead
          title="Dashboard"
          sub="64 actions across 1,284 artists · last evaluated 4m ago"
          right={<>
            <button className="btn ghost"><Icon name="bell" size={14}/> 3</button>
            <button className="btn ghost"><Icon name="download" size={14}/> Export</button>
            <button className="btn primary"><Icon name="check" size={14}/> Run rules now</button>
          </>}
        />

        <div className="sw-stat-row" style={{ marginBottom: 16 }}>
          <div className="sw-stat" style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: 14, alignItems: "center" }}>
            <HealthRing score={87} />
            <div>
              <div className="label">Library health</div>
              <div className="val" style={{ fontSize: 22 }}>87%</div>
              <div className="trend">+2.1 since last week</div>
            </div>
          </div>
          <StatCard label="Artists" value="1,284" trend="12 added this week" />
          <StatCard label="Auto-fixable" value="41" trend="≈ 6m to resolve" />
          <StatCard label="Needs you" value="23" trend="manual review" />
        </div>

        <UniversalFilterBar
          placeholder="Search artists, files, rules, MBIDs…"
          scope={["All", "Needs action", "Compliant", "Unidentified"]}
          activeScope={scope}
          setActiveScope={setScope}
          facets={dashFacets}
          active={activeFacets}
          setActive={setActiveFacets}
          savedViews={[{ id: "weekly", label: "Weekly review" }, { id: "high", label: "High severity" }]}
          resultCount={64}
        />

        <div className="sw-dash" style={{ marginTop: 16, gap: 16 }}>
          <div className="sw-card">
            <div className="head">
              <h2>Action queue</h2>
              <span className="muted" style={{ fontSize: 12 }}>Showing 7 of 64 · sorted by severity</span>
              <div className="meta row">
                <button className="btn ghost sm"><Icon name="check" size={12}/> Select all</button>
                <button className="btn warm sm">Fix 41 fixable</button>
              </div>
            </div>
            <div className="body tight">
              <div className="col" style={{ gap: 6 }}>
                {dashViolations.map(v => <ActionRow key={v.id} v={v} dense={density === "compact"} />)}
              </div>
            </div>
            <div className="head" style={{ borderTop: "1px solid var(--sw-line)", borderBottom: 0, justifyContent: "center" }}>
              <button className="btn ghost sm">Load 20 more · 57 remaining</button>
            </div>
          </div>

          <aside className="sw-card" style={{ position: "sticky", top: 16 }}>
            <div className="head">
              <h2>Recent activity</h2>
              <span className="meta">SSE live</span>
            </div>
            <div className="body tight">
              {recent.map((r, i) => (
                <div key={i} className="sw-activity">
                  <div className={`ico ${r.kind}`}>
                    {r.kind === "set" && <Icon name="plus" size={11}/>}
                    {r.kind === "changed" && <Icon name="chevron" size={11}/>}
                    {r.kind === "cleared" && <Icon name="x" size={11}/>}
                  </div>
                  <div className="flex-1">
                    <div style={{ fontSize: 13 }}>{r.who}</div>
                    <div className="muted" style={{ fontSize: 11.5 }}>
                      {r.field} {r.kind} <span className="mono">· {r.src}</span>
                    </div>
                  </div>
                  <span className="ts">{r.when}</span>
                </div>
              ))}
              <div className="divider"/>
              <a className="row muted" style={{ fontSize: 12, padding: "0 10px", color: "var(--sw-blue-ink)" }}>
                View all activity <Icon name="chevron" size={12}/>
              </a>
            </div>
          </aside>
        </div>

        <div className="muted" style={{ fontSize: 11.5, marginTop: 18, textAlign: "center", display: "flex", gap: 14, justifyContent: "center", alignItems: "center" }}>
          <span><span className="kbd">/</span> search</span>
          <span><span className="kbd">f</span> filters</span>
          <span><span className="kbd">g</span><span className="kbd">d</span> dashboard</span>
          <span><span className="kbd">g</span><span className="kbd">a</span> artists</span>
          <span><span className="kbd">?</span> shortcuts</span>
        </div>
      </div>

      {showAnnotations && (
        <>
          <Callout n={1} x={244} y={120} pos="left" w={240}>Same filter bar appears on Artists & Reports — single source of truth for chips, search, debounce.</Callout>
          <Callout n={2} x={244} y={188} pos="left" tone="cool">Health ring + 4 numbers, not 4 numbers fighting for attention.</Callout>
          <Callout n={3} x={840} y={420} pos="right" w={220}>Severity now uses dot + label, not just color (a11y).</Callout>
          <Callout n={4} x={244} y={620} pos="left" tone="cool" w={220}>"Fix 41 fixable" is the warm CTA — only one warm element on the page.</Callout>
        </>
      )}
    </div>
  );
}

/* "Current" recreation — close-enough mock of the live UI for side-by-side */
function DashboardCurrent() {
  return (
    <div className="sw-anno" data-density="comfy" style={{ background: "#0f172a" }}>
      <Sidebar active="dashboard" actionsCount={64} />
      <div className="sw-main">
        <div className="sw-card" style={{ padding: "14px 18px", marginBottom: 16 }}>
          <div className="row" style={{ justifyContent: "space-between" }}>
            <div className="row">
              <h1 style={{ fontSize: 20, margin: 0 }}>Dashboard</h1>
              <span className="chip active" style={{ background: "rgba(248,113,113,0.16)", color: "#fca5a5", borderColor: "rgba(248,113,113,0.4)" }}>64 actions needed</span>
            </div>
            <div className="row muted" style={{ fontSize: 12 }}>
              <span>1,284 artists</span><span>|</span>
              <span style={{ color: "#facc15" }}>87.0% compliant</span><span>|</span>
              <span style={{ fontSize: 11 }}>last evaluated 4m ago</span>
              <button className="btn ghost sm"><Icon name="filter" size={12}/> Filters <span className="chip active sm" style={{ height: 16, padding: "0 5px", fontSize: 10 }}>1</span></button>
              <button className="btn ghost sm"><Icon name="clock" size={12}/> Recent activity</button>
            </div>
          </div>
          <div style={{ position: "relative", marginTop: 10 }}>
            <input className="input with-icon" placeholder="Search artists, messages, rules…" defaultValue="" style={{ height: 32 }} />
            <Icon name="search" size={14}/>
          </div>
          <div className="row" style={{ marginTop: 8, gap: 6 }}>
            <span className="muted" style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.05em" }}>Active</span>
            <span className="chip active">Warning <button className="x"><Icon name="x" size={10}/></button></span>
            <button className="muted" style={{ fontSize: 12, color: "var(--sw-blue-ink)" }}>Clear all</button>
          </div>
        </div>
        <div className="row muted" style={{ marginBottom: 6, paddingLeft: 4, gap: 8 }}>
          <input type="checkbox" /><span style={{ fontSize: 11.5 }}>Select all</span>
        </div>
        <div className="col" style={{ gap: 6 }}>
          {dashViolations.slice(0, 5).map(v => <ActionRow key={v.id} v={v} />)}
        </div>
      </div>
    </div>
  );
}

Object.assign(window, { DashboardProposal, DashboardCurrent, HealthRing, StatCard, ActionRow });
