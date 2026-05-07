/* Settings — searchable left rail + ⌘K palette + flat sub-sections.
 * Replaces 11 horizontal tabs with: search-first nav, single scroll pane,
 * deep-linkable section anchors.
 */

/* Each item carries `keywords`: a flat list of strings that exist on the
 * actual content pane (section headings, control labels, env vars, file
 * paths, provider names, etc). Search matches the item label OR any
 * keyword — so typing "biography" surfaces Metadata providers (where
 * Biography field-priority lives) and Rules (where the bio_exists rule
 * lives), not just items with "Biography" in the title.
 */
const settingsGroups = [
  { id: "essentials", label: "Essentials", items: [
    { id: "general",     label: "General",         icon: "settings", count: 4,
      keywords: ["theme", "dark mode", "light mode", "density", "compact", "comfy", "ambient backdrop", "fanart", "lite mode", "blur", "transitions", "appearance", "behaviour", "behavior", "auto-fetch", "import", "base path", "SW_BASE_PATH", "suggest fixes"] },
    { id: "libraries",   label: "Music libraries", icon: "folder",   count: 2,
      keywords: ["/music", "classical", "regular library", "album-artist", "directory", "path", "file system", "scan", "Emby library", "add library", "remove library"] },
    { id: "platform",    label: "Platform profile", icon: "shield",  count: 1,
      keywords: ["NAS", "Synology", "Unraid", "Docker", "container", "permissions", "uid", "gid", "chmod", "filesystem"] },
  ]},
  { id: "data", label: "Data", items: [
    { id: "providers",   label: "Metadata providers", icon: "key",     count: 9,
      keywords: ["MusicBrainz", "Fanart.tv", "TheAudioDB", "Discogs", "Last.fm", "Spotify", "Wikipedia", "API key", "OAuth", "rate limit", "biography", "thumb image", "members", "field priority", "tags", "similar artists", "genres"] },
    { id: "languages",   label: "Languages",          icon: "tag",     count: 2,
      keywords: ["locale", "i18n", "translation", "fallback language", "preferred language", "en-US", "biography language"] },
    { id: "rules",       label: "Rules & severity",   icon: "shield",  count: 22,
      keywords: ["thumb minimum resolution", "thumb square", "fanart 1920x1080", "fanart aspect", "logo width", "NFO present", "NFO has MusicBrainz ID", "biography present", "biography minimum length", "extraneous files", "severity", "warn", "error", "info", "image rules", "metadata rules", "filesystem rules", "validation"] },
    { id: "schedule",    label: "Schedule",           icon: "clock",   count: 1,
      keywords: ["cron", "scan schedule", "refresh interval", "nightly", "weekly", "manual", "auto-refresh"] },
  ]},
  { id: "integrations", label: "Integrations", items: [
    { id: "connections", label: "Servers (Emby, Jellyfin, Lidarr)", icon: "server",  count: 3,
      keywords: ["Emby", "Jellyfin", "Lidarr", "NFO write-back", "quality profile", "test connection", "library push", "round-trip"] },
    { id: "webhooks",    label: "Webhooks & notifications",         icon: "bell",    count: 2,
      keywords: ["webhook", "Discord", "Slack", "ntfy", "Apprise", "notification", "scan complete", "error alert", "POST URL"] },
    { id: "tokens",      label: "API tokens",                       icon: "key",     count: 1,
      keywords: ["personal access token", "PAT", "scope", "read-only", "read-write", "expiry", "revoke", "REST API"] },
  ]},
  { id: "system", label: "System", items: [
    { id: "users",       label: "Users",         icon: "users",    count: 3,
      keywords: ["account", "role", "admin", "viewer", "invite", "password", "remove user"] },
    { id: "auth",        label: "Auth providers", icon: "shield",  count: 2,
      keywords: ["OIDC", "OpenID Connect", "SAML", "LDAP", "local auth", "SSO", "Authelia", "Authentik", "session timeout", "2FA"] },
    { id: "config-file", label: "Configuration file", icon: "logs", count: 1,
      keywords: ["TOML", "config.toml", "stillwater.toml", "version control", "git", "Ansible", "infrastructure as code", "IaC", "import config", "export config", "diff", "hand-edit", "watcher"] },
    { id: "maintenance", label: "Maintenance",   icon: "wrench",   count: 5,
      keywords: ["rebuild index", "vacuum database", "clear cache", "reset thumbnails", "backup", "restore", "export", "DB"] },
    { id: "logs",        label: "Logs",          icon: "logs",     count: 0,
      keywords: ["log level", "debug", "info", "warn", "error", "stdout", "log file", "tail", "filter logs"] },
    { id: "updates",     label: "Updates",       icon: "download", count: 1,
      keywords: ["release channel", "stable", "beta", "version", "changelog", "auto-update", "self-host"] },
  ]},
];

const allItems = settingsGroups.flatMap(g => g.items);

/* Wrap the matching substring in <mark> so users see why a result surfaced.
 * Returns either the original string (no match) or a fragment with one <mark>.
 */
function highlight(text, q) {
  if (!q) return text;
  const i = text.toLowerCase().indexOf(q.toLowerCase());
  if (i < 0) return text;
  return (
    <>
      {text.slice(0, i)}
      <mark style={{ background: "rgba(245, 158, 11, 0.22)", color: "var(--sw-warm)", padding: "0 1px", borderRadius: 2 }}>
        {text.slice(i, i + q.length)}
      </mark>
      {text.slice(i + q.length)}
    </>
  );
}

function SettingsProposal({ density = "comfy", layout = "rail", showAnnotations = false }) {
  const [active, setActive] = ufState("providers");
  const [query, setQuery] = ufState("");
  const [paletteOpen, setPaletteOpen] = ufState(false);

  ufEffect(() => {
    function onKey(e) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen(v => !v);
      }
      if (e.key === "Escape") setPaletteOpen(false);
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  /* Match against label OR any keyword. Returns the matched keyword (if any)
   * so we can render it under the item label in the rail — gives the user
   * a visible reason why the item showed up.
   */
  function matchItem(it, q) {
    if (!q) return { ok: true, hit: null };
    const ql = q.toLowerCase();
    if (it.label.toLowerCase().includes(ql)) return { ok: true, hit: null };
    const kw = (it.keywords || []).find(k => k.toLowerCase().includes(ql));
    if (kw) return { ok: true, hit: kw };
    return { ok: false, hit: null };
  }

  return (
    <div data-density={density} data-settings-layout={layout} className="sw-anno" style={{ position: "relative" }}>
      <Sidebar active="settings" actionsCount={64} />
      <div className="sw-main">
        <PageHead
          title="Settings"
          sub="Everything is searchable. Press ⌘K to jump anywhere."
          right={<>
            <button className="btn ghost" onClick={() => setPaletteOpen(true)}>
              <Icon name="command" size={12} /> Jump to setting <span className="kbd">⌘K</span>
            </button>
          </>}
        />

        <div className="sw-card sw-settings" style={{ minHeight: 560, padding: 0, gridTemplateColumns: layout === "palette" ? "1fr" : "260px 1fr" }}>
          {layout !== "palette" && (
            <nav className="rail">
              <div className="search" style={{ position: "relative" }}>
                <input
                  className="input with-icon"
                  placeholder="Filter settings…"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  style={{ height: 30 }}
                />
                <Icon name="search" size={13} style={{ position: "absolute", left: 9, top: "50%", transform: "translateY(-50%)", color: "var(--sw-ink-3)" }} />
              </div>
              {settingsGroups.map(g => {
                const items = query
                  ? g.items
                      .map(it => ({ it, m: matchItem(it, query) }))
                      .filter(x => x.m.ok)
                      .map(x => ({ ...x.it, _hit: x.m.hit }))
                  : g.items;
                if (items.length === 0) return null;
                return (
                  <div key={g.id}>
                    <div className="group">{g.label}</div>
                    {items.map(it => (
                      <div
                        key={it.id}
                        className={`item ${active === it.id ? "active" : ""}`}
                        onClick={() => setActive(it.id)}
                        style={it._hit ? { alignItems: "flex-start", paddingTop: 8, paddingBottom: 8 } : undefined}
                      >
                        <Icon name={it.icon} size={13} style={it._hit ? { marginTop: 2 } : undefined} />
                        <div className="flex-1" style={{ minWidth: 0 }}>
                          <div className="truncate">{highlight(it.label, query)}</div>
                          {it._hit && (
                            <div className="muted truncate" style={{ fontSize: 11, marginTop: 1 }}>
                              ↳ {highlight(it._hit, query)}
                            </div>
                          )}
                        </div>
                        {it.count > 0 && <span className="count">{it.count}</span>}
                      </div>
                    ))}
                  </div>
                );
              })}
            </nav>
          )}

          <div className="pane">
            <SettingsPaneContent active={active} />
          </div>
        </div>

        <div className="muted" style={{ fontSize: 11.5, marginTop: 14, textAlign: "center", display: "flex", gap: 14, justifyContent: "center", alignItems: "center" }}>
          <span><span className="kbd">⌘K</span> palette</span>
          <span><span className="kbd">/</span> filter rail</span>
          <span><span className="kbd">⏎</span> jump to section</span>
          <span>· Anchors are deep-linkable: <span className="mono">/settings#providers.musicbrainz</span></span>
        </div>
      </div>

      {paletteOpen && <CommandPalette items={allItems} onSelect={(id) => { setActive(id); setPaletteOpen(false); }} onClose={() => setPaletteOpen(false)} />}

      {showAnnotations && (
        <>
          <Callout n={1} x={244} y={170} pos="left" w={220}>11 tabs → 4 groups, 14 sections. Same surface, way less hunting.</Callout>
          <Callout n={2} x={244} y={250} pos="left" tone="cool">Filter rail by name OR press ⌘K from anywhere.</Callout>
          <Callout n={3} x={520} y={250} pos="bottom" w={240}>Counts show what's configured / how many entries — orientation at a glance.</Callout>
          <Callout n={4} x={520} y={500} pos="bottom" tone="cool" w={240}>Sections are deep-linkable so you can /settings#providers.musicbrainz from a doc or a chat link.</Callout>
        </>
      )}
    </div>
  );
}

function CommandPalette({ items, onSelect, onClose }) {
  const [q, setQ] = ufState("");
  const [idx, setIdx] = ufState(0);
  const filtered = items
    .map(it => {
      if (!q) return { ...it, _hit: null };
      const ql = q.toLowerCase();
      if (it.label.toLowerCase().includes(ql)) return { ...it, _hit: null };
      const kw = (it.keywords || []).find(k => k.toLowerCase().includes(ql));
      return kw ? { ...it, _hit: kw } : null;
    })
    .filter(Boolean)
    .slice(0, 8);
  const fakeActions = q.length === 0 ? [
    { id: "act-add-library",   label: "Add a music library…",   icon: "plus" },
    { id: "act-add-token",     label: "Generate API token…",    icon: "key" },
    { id: "act-test-conn",     label: "Test all connections",   icon: "server" },
    { id: "act-rules-now",     label: "Run rules now",          icon: "shield" },
  ] : [];

  // Single activation path used by Enter, click, and (later) keyboard
  // activation on the action rows. Resolves the current row index across
  // the two row groups (Actions appear above Settings when q is empty).
  function activateAt(rowIdx) {
    if (rowIdx < fakeActions.length) {
      const a = fakeActions[rowIdx];
      // Faux-execute — in a real build this would dispatch to a handler.
      // For the prototype we close and let the host show a toast.
      onSelect(a.id);
    } else {
      const it = filtered[rowIdx - fakeActions.length];
      if (it) onSelect(it.id);
    }
  }

  ufEffect(() => {
    function onKey(e) {
      const total = filtered.length + fakeActions.length;
      if (e.key === "ArrowDown") { e.preventDefault(); setIdx(i => Math.min(i + 1, total - 1)); }
      if (e.key === "ArrowUp")   { e.preventDefault(); setIdx(i => Math.max(i - 1, 0)); }
      if (e.key === "Enter")     { e.preventDefault(); activateAt(idx); }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [filtered.length, fakeActions.length, idx]);

  return (
    <div
      style={{
        position: "fixed", inset: 0, background: "rgba(7,12,24,0.6)",
        backdropFilter: "blur(6px)", display: "grid", placeItems: "start center",
        paddingTop: 120, zIndex: 100,
      }}
      onClick={onClose}
    >
      <div
        className="sw-card elev"
        style={{ width: 520, padding: 0 }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="row" style={{ padding: "12px 16px", borderBottom: "1px solid var(--sw-line)", gap: 10 }}>
          <Icon name="command" size={14} />
          <input
            autoFocus
            className="input"
            placeholder="Jump to a setting or run an action…"
            value={q}
            onChange={(e) => { setQ(e.target.value); setIdx(0); }}
            style={{ height: 30, border: 0, background: "transparent", padding: 0 }}
          />
          <span className="kbd">esc</span>
        </div>
        <div style={{ maxHeight: 360, overflowY: "auto", padding: 6 }}>
          {fakeActions.length > 0 && (
            <>
              <div style={{ fontSize: 10.5, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--sw-ink-4)", padding: "8px 10px 4px" }}>Actions</div>
              {fakeActions.map((a, i) => (
                <div
                  key={a.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => activateAt(i)}
                  onMouseEnter={() => setIdx(i)}
                  className="row"
                  style={{
                    padding: "8px 10px", gap: 10, borderRadius: 6,
                    background: idx === i ? "var(--sw-blue-soft)" : "transparent",
                    color: idx === i ? "var(--sw-blue-ink)" : "var(--sw-ink-2)",
                    fontSize: 13, cursor: "pointer",
                  }}
                >
                  <Icon name={a.icon} size={13} />
                  <span className="flex-1">{a.label}</span>
                  {idx === i && <span className="kbd">⏎</span>}
                </div>
              ))}
            </>
          )}
          <div style={{ fontSize: 10.5, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--sw-ink-4)", padding: "8px 10px 4px" }}>Settings</div>
          {filtered.map((it, i) => {
            const off = fakeActions.length + i;
            const groupLabel = (settingsGroups.find(g => g.items.some(x => x.id === it.id)) || {}).label || "";
            return (
              <div
                key={it.id}
                role="button"
                tabIndex={0}
                onClick={() => onSelect(it.id)}
                onMouseEnter={() => setIdx(off)}
                className="row"
                style={{
                  padding: "8px 10px", gap: 10, borderRadius: 6, fontSize: 13,
                  background: idx === off ? "var(--sw-blue-soft)" : "transparent",
                  color: idx === off ? "var(--sw-blue-ink)" : "var(--sw-ink-2)",
                  cursor: "pointer", alignItems: "flex-start",
                }}
              >
                <Icon name={it.icon} size={13} style={{ marginTop: 2 }} />
                <div className="flex-1" style={{ minWidth: 0 }}>
                  <div className="row" style={{ gap: 6, fontSize: 13 }}>
                    <span>{highlight(it.label, q)}</span>
                    <span className="muted" style={{ fontSize: 11 }}>· {groupLabel}</span>
                  </div>
                  {it._hit && (
                    <div style={{ fontSize: 11.5, marginTop: 2, color: idx === off ? "var(--sw-blue-ink)" : "var(--sw-ink-3)" }}>
                      ↳ {highlight(it._hit, q)}
                    </div>
                  )}
                </div>
                {idx === off && <span className="kbd">⏎</span>}
              </div>
            );
          })}
        </div>
        <div className="row" style={{ padding: "8px 12px", borderTop: "1px solid var(--sw-line)", fontSize: 11, color: "var(--sw-ink-3)", gap: 12 }}>
          <span><span className="kbd">↑</span><span className="kbd">↓</span> navigate</span>
          <span><span className="kbd">⏎</span> open</span>
          <span style={{ marginLeft: "auto" }}>Same palette works on every page</span>
        </div>
      </div>
    </div>
  );
}

function SettingsPaneContent({ active }) {
  // Concrete panes — every section that has bespoke prototype copy.
  if (active === "providers")   return <ProvidersPane />;
  if (active === "general")     return <GeneralPane />;
  if (active === "libraries")   return <LibrariesPane />;
  if (active === "connections") return <ConnectionsPane />;
  if (active === "rules")       return <RulesPane />;
  if (active === "config-file") return <ConfigFilePane />;
  // Sections from the rail that don't have a bespoke prototype yet.
  // We resolve to a stub rather than silently re-rendering GeneralPane so
  // reviewers can tell which surfaces still need a real layout pass.
  const knownStubs = ["platform", "languages", "schedule", "webhooks", "tokens",
                      "users", "auth", "maintenance", "logs", "updates"];
  if (knownStubs.includes(active)) return <StubPane sectionId={active} />;
  // Truly unknown id — surface it instead of falling through.
  return <StubPane sectionId={active} unknown />;
}

function StubPane({ sectionId, unknown }) {
  const label = (settingsGroups.flatMap(g => g.items).find(it => it.id === sectionId) || {}).label || sectionId;
  return (
    <div className="sw-card" style={{ padding: 24, marginTop: 8 }}>
      <h2 style={{ margin: 0, fontSize: 16 }}>{label}</h2>
      <p className="muted" style={{ marginTop: 8, fontSize: 13 }}>
        {unknown
          ? <>Unknown section <span className="mono">{sectionId}</span>. The rail item points at a section the prototype doesn't render yet.</>
          : <>This section's prototype layout hasn't been authored yet — the rail is here so reviewers can see the navigation surface even before each pane lands.</>}
      </p>
    </div>
  );
}

function SettingRow({ label, desc, children }) {
  return (
    <div className="sw-setting">
      <div>
        <div className="lbl">{label}</div>
        {desc && <div className="desc">{desc}</div>}
      </div>
      <div className="ctl">{children}</div>
    </div>
  );
}
function Toggle({ on, onChange = () => {} }) {
  return <button className={`tog ${on ? "on" : ""}`} onClick={onChange} aria-pressed={on}></button>;
}

function ProvidersPane() {
  const providers = [
    { name: "MusicBrainz", status: "ok",       desc: "Open library — no key required",            note: "Source of truth for IDs" },
    { name: "Fanart.tv",   status: "ok",       desc: "Personal API key — 5 requests / second",     note: "Best for fanart, banners" },
    { name: "TheAudioDB",  status: "ok",       desc: "Personal API key",                           note: "Biographies, genres" },
    { name: "Discogs",     status: "ok",       desc: "Personal token + user-agent",                note: "Members & roles" },
    { name: "Last.fm",     status: "warn",     desc: "Rate-limited yesterday — auto-recovered",    note: "Tags, similar artists" },
    { name: "Spotify",     status: "off",      desc: "OAuth — not configured",                     note: "Optional, premium artwork" },
    { name: "Wikipedia",   status: "ok",       desc: "No key required",                            note: "Biographies (long-form)" },
  ];
  return (
    <>
      <h2>Metadata providers</h2>
      <div className="sub">
        Stillwater pulls artist data from these sources. Drag rows below to set field-level priority — <span className="mono">biography</span> may prefer Wikipedia, while <span className="mono">images</span> may prefer Fanart.tv.
      </div>

      <div className="sw-card" style={{ marginBottom: 16 }}>
        <div className="head">
          <h2>Configured</h2>
          <span className="meta">7 of 9 providers</span>
        </div>
        <div className="body" style={{ padding: 0 }}>
          {providers.map(p => (
            <div key={p.name} className="row" style={{ padding: "10px 18px", borderTop: "1px solid var(--sw-line)", gap: 12 }}>
              <div className="row" style={{ width: 22 }}>
                {p.status === "ok"   && <span style={{ width: 8, height: 8, borderRadius: 999, background: "var(--sw-ok)" }}></span>}
                {p.status === "warn" && <span style={{ width: 8, height: 8, borderRadius: 999, background: "var(--sw-warn)" }}></span>}
                {p.status === "off"  && <span style={{ width: 8, height: 8, borderRadius: 999, background: "var(--sw-ink-4)" }}></span>}
              </div>
              <div className="flex-1">
                <div style={{ fontSize: 13, fontWeight: 500 }}>{p.name}</div>
                <div className="muted" style={{ fontSize: 11.5 }}>{p.desc} · {p.note}</div>
              </div>
              <button className="btn ghost sm">Configure</button>
              <Toggle on={p.status !== "off"} />
            </div>
          ))}
        </div>
      </div>

      <div className="sw-card">
        <div className="head"><h2>Field priority</h2><span className="meta">Drag to reorder · per field</span></div>
        <div className="body">
          <SettingRow label="Biography" desc="First provider with a non-empty value wins.">
            <div className="row" style={{ gap: 4 }}>
              <span className="chip active">1. Wikipedia</span>
              <span className="chip">2. AudioDB</span>
              <span className="chip">3. Last.fm</span>
            </div>
          </SettingRow>
          <SettingRow label="Thumb image" desc="Square portrait, 500×500 minimum.">
            <div className="row" style={{ gap: 4 }}>
              <span className="chip active">1. Fanart.tv</span>
              <span className="chip">2. AudioDB</span>
              <span className="chip">3. Wikipedia</span>
            </div>
          </SettingRow>
          <SettingRow label="Members" desc="Group/band lineup parsed from MusicBrainz relationships.">
            <div className="row" style={{ gap: 4 }}>
              <span className="chip active">1. MusicBrainz</span>
              <span className="chip">2. Discogs</span>
            </div>
          </SettingRow>
        </div>
      </div>
    </>
  );
}

function GeneralPane() {
  return (
    <>
      <h2>General</h2>
      <div className="sub">Workspace-wide preferences. Per-user prefs live in your profile menu.</div>

      <div className="sw-card" style={{ marginBottom: 16 }}>
        <div className="head"><h2>Appearance</h2></div>
        <div className="body">
          <SettingRow label="Theme" desc="Dark, light, or follow system.">
            <select className="select" defaultValue="dark" style={{ width: 140 }}>
              <option>dark</option><option>light</option><option>system</option>
            </select>
          </SettingRow>
          <SettingRow label="Density" desc="How tightly cards and rows are packed.">
            <select className="select" defaultValue="comfy" style={{ width: 140 }}>
              <option>compact</option><option>comfy</option><option>cozy</option>
            </select>
          </SettingRow>
          <SettingRow label="Ambient backdrop" desc="Random fanart, blurred behind the UI.">
            <Toggle on={true} />
          </SettingRow>
          <SettingRow label="Lite mode" desc="Disables blur, glass, and transitions for low-power devices.">
            <Toggle on={false} />
          </SettingRow>
        </div>
      </div>

      <div className="sw-card">
        <div className="head"><h2>Behaviour</h2></div>
        <div className="body">
          <SettingRow label="Suggest fixes for compliant artists" desc="Run rules in dry-run mode against artists already at 100%.">
            <Toggle on={false} />
          </SettingRow>
          <SettingRow label="Auto-fetch on import" desc="Pull metadata immediately when a new artist is detected.">
            <Toggle on={true} />
          </SettingRow>
          <SettingRow label={<>Base path <span className="mono">SW_BASE_PATH</span></>} desc="Read-only — set via env var on container.">
            <code className="mono" style={{ fontSize: 12, color: "var(--sw-ink-3)" }}>/stillwater</code>
          </SettingRow>
        </div>
      </div>
    </>
  );
}

function LibrariesPane() {
  return (
    <>
      <h2>Music libraries</h2>
      <div className="sub">One album-artist per top-level directory, e.g. <span className="mono">/music/Pink Floyd/</span>.</div>
      <div className="sw-card">
        <div className="head"><h2>Configured</h2><button className="btn primary sm" style={{ marginLeft: "auto" }}><Icon name="plus" size={12}/> Add library</button></div>
        <div className="body" style={{ padding: 0 }}>
          {[
            { name: "Main library",      path: "/music",            type: "regular",   conn: "Emby"     },
            { name: "Classical archive", path: "/music/classical",  type: "classical", conn: null       },
          ].map(l => (
            <div key={l.name} className="row" style={{ padding: "12px 18px", borderTop: "1px solid var(--sw-line)", gap: 12 }}>
              <Icon name="folder" />
              <div className="flex-1">
                <div style={{ fontSize: 13, fontWeight: 500 }}>{l.name}</div>
                <div className="muted mono" style={{ fontSize: 11.5 }}>{l.path}</div>
              </div>
              <span className="chip">{l.type}</span>
              {l.conn && <span className="chip">{l.conn}</span>}
              <button className="btn ghost sm">Edit</button>
              <button className="btn ghost sm" style={{ color: "var(--sw-err)" }}>Remove</button>
            </div>
          ))}
        </div>
      </div>
    </>
  );
}

function ConnectionsPane() {
  return (
    <>
      <h2>Servers</h2>
      <div className="sub">Push metadata to Emby/Jellyfin and read profiles from Lidarr.</div>
      <div className="sw-card">
        <div className="head"><h2>Connections</h2><button className="btn primary sm" style={{ marginLeft: "auto" }}><Icon name="plus" size={12}/> Add</button></div>
        <div className="body" style={{ padding: 0 }}>
          {[
            { type: "Emby",     url: "http://192.168.1.100:8096", status: "ok",   notes: "5 libraries · NFO write-back disabled" },
            { type: "Jellyfin", url: "http://192.168.1.100:8097", status: "warn", notes: "NFO write-back enabled — round-trip risk" },
            { type: "Lidarr",   url: "http://192.168.1.100:8686", status: "ok",   notes: "Read-only · 3 quality profiles" },
          ].map(c => (
            <div key={c.type} className="row" style={{ padding: "12px 18px", borderTop: "1px solid var(--sw-line)", gap: 12, alignItems: "flex-start" }}>
              <Icon name="server" />
              <div className="flex-1">
                <div style={{ fontSize: 13, fontWeight: 500 }}>{c.type}</div>
                <div className="muted mono" style={{ fontSize: 11.5 }}>{c.url}</div>
                <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>{c.notes}</div>
              </div>
              {c.status === "ok"   && <span className="sev info"><span className="dot"></span>Connected</span>}
              {c.status === "warn" && <span className="sev warn"><span className="dot"></span>Conflict</span>}
              <button className="btn ghost sm">Test</button>
              <Toggle on={true} />
            </div>
          ))}
        </div>
      </div>
    </>
  );
}

function RulesPane() {
  const rules = [
    { id: "thumb_min_res",      label: "Thumb meets minimum resolution", sev: "warn", on: true,  cat: "image" },
    { id: "thumb_square",       label: "Thumb is square (1:1)",          sev: "warn", on: true,  cat: "image" },
    { id: "fanart_min_res",     label: "Fanart 1920×1080 minimum",       sev: "warn", on: true,  cat: "image" },
    { id: "fanart_aspect",      label: "Fanart aspect 16:9",             sev: "info", on: true,  cat: "image" },
    { id: "logo_min_res",       label: "Logo at least 400 px wide",      sev: "info", on: true,  cat: "image" },
    { id: "nfo_exists",         label: "NFO file present",               sev: "err",  on: true,  cat: "nfo" },
    { id: "nfo_has_mbid",       label: "NFO has MusicBrainz ID",         sev: "err",  on: true,  cat: "nfo" },
    { id: "extraneous_files",   label: "No extraneous artist files",     sev: "info", on: false, cat: "nfo" },
    { id: "bio_exists",         label: "Biography is present",           sev: "warn", on: true,  cat: "metadata" },
    { id: "artist_has_members", label: "Group artists list members",     sev: "info", on: true,  cat: "metadata" },
  ];
  return (
    <>
      <h2>Rules & severity</h2>
      <div className="sub">22 rules. Disable any you don't care about — they stop contributing to the health score immediately.</div>
      <div className="sw-card">
        <div className="head">
          <h2>Active rules</h2>
          <span className="meta">8 of 22 enabled in this view</span>
          <input className="input" placeholder="Filter rules…" style={{ width: 180, marginLeft: "auto" }} />
        </div>
        <div className="body" style={{ padding: 0 }}>
          {rules.map(r => (
            <div key={r.id} className="row" style={{ padding: "10px 18px", borderTop: "1px solid var(--sw-line)", gap: 12 }}>
              <Toggle on={r.on} />
              <div className="flex-1">
                <div style={{ fontSize: 13, fontWeight: 500 }}>{r.label}</div>
                <div className="muted mono" style={{ fontSize: 11.5 }}>{r.id}</div>
              </div>
              <span className={`sev ${r.sev === "err" ? "err" : r.sev === "warn" ? "warn" : "info"}`}>
                <span className="dot"/>{r.sev === "err" ? "Error" : r.sev === "warn" ? "Warning" : "Info"}
              </span>
              <span className="chip">{r.cat}</span>
              <button className="btn ghost sm">Edit</button>
            </div>
          ))}
        </div>
      </div>
    </>
  );
}

/* "Current" recreation — 11 horizontal tabs */
function SettingsCurrent() {
  const tabs = ["General", "Providers", "Connections", "Libraries", "Automation", "Rules", "Users", "Auth", "Maintenance", "Logs", "Updates"];
  return (
    <div className="sw-anno" data-density="comfy">
      <Sidebar active="settings" actionsCount={64}/>
      <div className="sw-main">
        <div className="sw-card" style={{ padding: "16px 18px", marginBottom: 14 }}>
          <h1 style={{ fontSize: 20, margin: 0 }}>Settings</h1>
          <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>Configure your Stillwater workspace.</div>
        </div>
        <div className="row" style={{ overflowX: "auto", borderBottom: "1px solid var(--sw-line)", marginBottom: 16, gap: 0 }}>
          {tabs.map((t, i) => (
            <button key={t} style={{
              padding: "8px 14px", fontSize: 13,
              color: i === 1 ? "var(--sw-blue-ink)" : "var(--sw-ink-3)",
              borderBottom: i === 1 ? "2px solid var(--sw-blue)" : "2px solid transparent",
              whiteSpace: "nowrap",
            }}>{t}</button>
          ))}
        </div>
        <div className="muted" style={{ fontSize: 12, marginBottom: 12 }}>Providers tab content — long scroll, no in-tab navigation, no search.</div>
        <div className="sw-card" style={{ marginBottom: 12 }}>
          <div className="head"><h2>Provider API keys</h2></div>
          <div className="body">
            <div className="muted" style={{ fontSize: 12.5 }}>9 providers stacked vertically — find one by scrolling.</div>
          </div>
        </div>
        <div className="sw-card" style={{ marginBottom: 12 }}>
          <div className="head"><h2>Web image search</h2></div>
          <div className="body"><div className="muted" style={{ fontSize: 12.5 }}>3 toggles…</div></div>
        </div>
        <div className="sw-card" style={{ marginBottom: 12 }}>
          <div className="head"><h2>Provider priorities</h2></div>
          <div className="body"><div className="muted" style={{ fontSize: 12.5 }}>5 sortable lists…</div></div>
        </div>
        <div className="sw-card" style={{ marginBottom: 12 }}>
          <div className="head"><h2>Metadata languages</h2></div>
          <div className="body"><div className="muted" style={{ fontSize: 12.5 }}>Pills + autocomplete.</div></div>
        </div>
        <div className="sw-card">
          <div className="head"><h2>Advanced</h2></div>
          <div className="body"><div className="muted" style={{ fontSize: 12.5 }}>Name similarity threshold + others.</div></div>
        </div>
      </div>
    </div>
  );
}

Object.assign(window, { SettingsProposal, SettingsCurrent, CommandPalette });
// Expose for the global cmdk palette to index.
window.cmdkSettingsGroups = settingsGroups;
