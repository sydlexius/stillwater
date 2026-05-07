/* Global command palette — VS Code–style.
 *
 * Hidden until ⌘K (or Ctrl+K) is pressed; renders nothing in the DOM
 * until then. Mount once per page from screen-init; the host page
 * doesn't need to know it exists.
 *
 * Indexes three sources:
 *   - Screens         → top-level navigation (Dashboard, Artists, …)
 *   - Settings        → every settings section, deep-linked
 *   - Actions         → things you can DO right now (run rules, pause
 *                       writes, generate token, jump to last conflict)
 *
 * Variations exposed via Tweaks:
 *   - scope:    "all" | "screens" | "settings"   (what's indexed)
 *   - actions:  "always" | "empty-only"          (where actions appear)
 *   - layout:   "spotlight" | "vscode" | "raycast"
 *
 * The palette never persists state across opens — each invocation is
 * fresh, query empty, first item highlighted. That matches the muscle
 * memory power users have from VS Code / Linear / Raycast.
 */

const { useState: cmUseState, useEffect: cmUseEffect, useRef: cmUseRef, useMemo: cmUseMemo } = React;

/* The action set is intentionally short. Every item should be something
 * a user might reasonably want to do RIGHT NOW from any screen — not a
 * mirror of every button in the app. If it's only useful while you're
 * already on the page where it lives, it doesn't belong here. */
const cmdkActions = [
  { id: "act-run-rules",    label: "Run rules now",            icon: "shield",   kw: ["validate", "lint", "check"], hint: "scans library, no writes" },
  { id: "act-pause-writes", label: "Pause metadata writes",    icon: "shield",   kw: ["freeze", "halt", "stop"],     hint: "across all sources" },
  { id: "act-test-conn",    label: "Test all connections",     icon: "server",   kw: ["ping", "health", "status"] },
  { id: "act-add-token",    label: "Generate API token…",      icon: "key",      kw: ["pat", "personal access"] },
  { id: "act-add-library",  label: "Add a music library…",     icon: "folder",   kw: ["path", "directory", "scan"] },
  { id: "act-jump-conflict",label: "Open last conflict",       icon: "warn",     kw: ["error", "issue", "problem"] },
  { id: "act-toggle-theme", label: "Toggle theme",             icon: "settings", kw: ["dark", "light"] },
  { id: "act-restart",      label: "Restart Stillwater",       icon: "settings", kw: ["reboot", "restart service"], danger: true },
];

/* Screens are static — they never change at runtime. */
/* Per-screen go-to shortcuts. Linear-style: press `g` then a letter.
 * Avoids the ⌘1–⌘9 collision with browser tab switching. The leader
 * key is captured globally; second key has a 1.5s window. */
const cmdkScreens = [
  { id: "scr-dashboard",   label: "Dashboard",             icon: "home",     href: "dashboard.html",  shortcut: ["g", "d"] },
  { id: "scr-artists",     label: "Artists",               icon: "users",    href: "artists.html",    shortcut: ["g", "a"] },
  { id: "scr-reports",     label: "Reports",               icon: "chart",    href: "reports.html",    shortcut: ["g", "r"] },
  { id: "scr-findings",    label: "Findings",              icon: "warn",     href: "findings.html",   shortcut: ["g", "f"] },
  { id: "scr-settings",    label: "Settings",              icon: "settings", href: "settings.html",   shortcut: ["g", "s"] },
  { id: "scr-onboarding",  label: "Onboarding (proposal)", icon: "settings", href: "onboarding.html", shortcut: ["g", "o"] },
];

/* Settings index — pulled from the source of truth in settings.jsx if
 * available, else falls back to a small inline list. We don't try to
 * deep-link inside settings sections from this palette; clicking a
 * settings entry just lands you on the right top-level item.
 */
function cmdkSettingsIndex() {
  const groups = (typeof window !== "undefined" && window.cmdkSettingsGroups) || [];
  const out = [];
  for (const g of groups) {
    for (const it of g.items) {
      out.push({
        id: "set-" + it.id,
        label: it.label,
        icon: it.icon,
        href: "settings.html#" + it.id,
        group: g.label,
        kw: it.keywords || [],
      });
    }
  }
  if (out.length === 0) {
    return [
      { id: "set-general",     label: "General",            icon: "settings", href: "settings.html", group: "Essentials" },
      { id: "set-providers",   label: "Metadata providers", icon: "key",      href: "settings.html", group: "Data" },
      { id: "set-rules",       label: "Rules & severity",   icon: "shield",   href: "settings.html", group: "Data" },
      { id: "set-connections", label: "Servers",            icon: "server",   href: "settings.html", group: "Integrations" },
    ];
  }
  return out;
}

/* Substring match across label + keywords. Keeps it deterministic and
 * predictable — fuzzy ranking (fzf-style) is overkill for ~40 entries
 * and confuses users when "settings" matches "filesystem". */
function cmdkMatch(item, q) {
  if (!q) return { ok: true, hit: null };
  const ql = q.toLowerCase();
  if (item.label.toLowerCase().includes(ql)) return { ok: true, hit: null };
  const kw = (item.kw || []).find(k => k.toLowerCase().includes(ql));
  return kw ? { ok: true, hit: kw } : { ok: false, hit: null };
}

function cmdkHighlight(text, q) {
  if (!q) return text;
  const i = text.toLowerCase().indexOf(q.toLowerCase());
  if (i < 0) return text;
  return (
    <>
      {text.slice(0, i)}
      <mark className="cmdk-mark">{text.slice(i, i + q.length)}</mark>
      {text.slice(i + q.length)}
    </>
  );
}

/* Default tweak settings. The host can override with a meta tag on the
 * page; lets us A/B variations without touching this file.
 *    <meta name="sw-cmdk-layout"  content="vscode">
 *    <meta name="sw-cmdk-scope"   content="all">
 *    <meta name="sw-cmdk-actions" content="always">
 */
function cmdkReadMeta(name, fallback) {
  if (typeof document === "undefined") return fallback;
  // localStorage override (set by the in-palette layout switcher) wins
  // over the page meta-tag default.
  const stored = (() => { try { return localStorage.getItem(name); } catch (e) { return null; } })();
  if (stored) return stored;
  const m = document.querySelector(`meta[name="${name}"]`);
  return (m && m.content) || fallback;
}

function cmdkSetLayout(L) {
  try { localStorage.setItem("sw-cmdk-layout", L); } catch (e) {}
  // Force a re-render by re-toggling the palette. Cleanest path is a
  // page-level event the host listens for.
  window.dispatchEvent(new CustomEvent("cmdk-layout-change", { detail: L }));
}

function CommandPaletteHost() {
  const [open, setOpen] = cmUseState(false);

  // VS Code pattern: hidden until activated; not even mounted in the
  // DOM. The keybinding listener is global and lives on the host.
  cmUseEffect(() => {
    let leaderTimeout = null;
    let leaderActive = false;

    function clearLeader() {
      leaderActive = false;
      if (leaderTimeout) { clearTimeout(leaderTimeout); leaderTimeout = null; }
    }

    function onKey(e) {
      const meta = e.metaKey || e.ctrlKey;
      if (meta && (e.key === "k" || e.key === "K")) {
        e.preventDefault();
        setOpen(o => !o);
        clearLeader();
        return;
      }
      if (e.key === "Escape" && open) {
        setOpen(false);
        clearLeader();
        return;
      }

      // Ignore go-to shortcuts while typing in form fields or while
      // the palette is open (the palette has its own input focus).
      const t = e.target;
      const inField = t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable);
      if (inField || open || meta || e.altKey) return;

      // Leader: press `g`, then within 1.5s press a screen letter.
      if (!leaderActive && (e.key === "g" || e.key === "G")) {
        leaderActive = true;
        leaderTimeout = setTimeout(clearLeader, 1500);
        return;
      }
      if (leaderActive) {
        const k = e.key.toLowerCase();
        const target = cmdkScreens.find(s => s.shortcut && s.shortcut[1] === k);
        clearLeader();
        if (target) {
          e.preventDefault();
          // Resolve href the same way palette item activation does.
          const here = location.pathname;
          const inScreens = /\/screens\//.test(here);
          const url = inScreens ? target.href : ("screens/" + target.href);
          location.href = url;
        }
      }
    }
    window.addEventListener("keydown", onKey);
    window.__swCmdkOpen = () => setOpen(true);
    window.__swCmdkClose = () => setOpen(false);
    window.__swCmdkToggle = () => setOpen(o => !o);
    return () => {
      window.removeEventListener("keydown", onKey);
      clearLeader();
    };
  }, [open]);

  if (!open) return null;
  return <CommandPaletteSurface onClose={() => setOpen(false)}/>;
}

function CommandPaletteSurface({ onClose }) {
  const [, force] = cmUseState(0);
  cmUseEffect(() => {
    function onChange() { force(n => n + 1); }
    window.addEventListener("cmdk-layout-change", onChange);
    return () => window.removeEventListener("cmdk-layout-change", onChange);
  }, []);

  const layout  = cmdkReadMeta("sw-cmdk-layout",  "vscode");      // "vscode" | "spotlight" | "raycast"
  const scope   = cmdkReadMeta("sw-cmdk-scope",   "all");          // "all" | "screens" | "settings"
  const showAct = cmdkReadMeta("sw-cmdk-actions", "always");       // "always" | "empty-only"

  const [q, setQ] = cmUseState("");
  const [idx, setIdx] = cmUseState(0);
  const inputRef = cmUseRef(null);

  cmUseEffect(() => { inputRef.current && inputRef.current.focus(); }, []);

  const allItems = cmUseMemo(() => {
    const out = [];
    if (scope === "all" || scope === "screens")  out.push(...cmdkScreens.map(s => ({ ...s, kind: "screen" })));
    if (scope === "all" || scope === "settings") out.push(...cmdkSettingsIndex().map(s => ({ ...s, kind: "setting" })));
    return out;
  }, [scope]);

  const includeActions = showAct === "always" || (showAct === "empty-only" && q.length === 0);

  const matchedItems = allItems
    .map(it => ({ it, m: cmdkMatch(it, q) }))
    .filter(x => x.m.ok)
    .map(x => ({ ...x.it, _hit: x.m.hit }));

  const matchedActions = includeActions
    ? cmdkActions
        .map(it => ({ it, m: cmdkMatch(it, q) }))
        .filter(x => x.m.ok)
        .map(x => ({ ...x.it, _hit: x.m.hit, kind: "action" }))
    : [];

  // Group ordering depends on layout: VS Code leads with what you'd
  // type a verb for (actions); Spotlight/Raycast lead with what you
  // search for by name (results).
  const sections = layout === "vscode"
    ? [
        { key: "actions",  label: "Actions",  items: matchedActions },
        { key: "screens",  label: "Screens",  items: matchedItems.filter(i => i.kind === "screen") },
        { key: "settings", label: "Settings", items: matchedItems.filter(i => i.kind === "setting") },
      ]
    : [
        { key: "screens",  label: "Screens",  items: matchedItems.filter(i => i.kind === "screen") },
        { key: "settings", label: "Settings", items: matchedItems.filter(i => i.kind === "setting") },
        { key: "actions",  label: "Actions",  items: matchedActions },
      ];

  const flatItems = sections.flatMap(s => s.items);
  const safeIdx = Math.min(idx, Math.max(0, flatItems.length - 1));

  cmUseEffect(() => { setIdx(0); }, [q]);

  // Mirror the resolution that the `g` leader shortcut performs: when the
  // palette is opened from outside `/screens/` (e.g. the index or the design
  // canvas review shell), prefix `screens/` to plain relative hrefs so the
  // browser doesn't 404. Items already starting with `screens/`, `/`, or a
  // protocol are passed through untouched.
  function resolvePrototypeHref(href) {
    const inScreens = /\/screens\//.test(location.pathname);
    if (inScreens) return href;
    if (/^(screens\/|\/|[a-z]+:)/i.test(href)) return href;
    const [path, hash] = href.split("#");
    const resolved = `screens/${path}`;
    return hash ? `${resolved}#${hash}` : resolved;
  }

  function activate(item) {
    if (!item) return;
    if (item.kind === "action") {
      // Faux-execute: in a real build this would dispatch to a handler.
      // For the prototype, we flash a transient toast and dismiss.
      cmdkFlashToast(item.label);
    } else if (item.href) {
      // Detect "are we already at this href?" — settings.html#general
      // from the settings page should just close + scroll-to-anchor,
      // not full-load.
      const here = location.pathname.split("/").pop();
      const target = item.href.split("#")[0];
      if (here === target && item.href.includes("#")) {
        location.hash = item.href.split("#")[1];
      } else {
        location.href = resolvePrototypeHref(item.href);
      }
    }
    onClose();
  }

  // Stash the per-render values in refs so the keydown listener doesn't have
  // to be re-registered on every keystroke (flatItems / activate are
  // recreated each render). The effect's deps stay empty; the listener
  // reads the latest state through the refs.
  const flatItemsRef = cmUseRef(flatItems);
  const safeIdxRef = cmUseRef(safeIdx);
  const activateRef = cmUseRef(activate);
  flatItemsRef.current = flatItems;
  safeIdxRef.current = safeIdx;
  activateRef.current = activate;

  cmUseEffect(() => {
    function onKey(e) {
      if (e.key === "ArrowDown") { e.preventDefault(); setIdx(i => Math.min(i + 1, flatItemsRef.current.length - 1)); }
      if (e.key === "ArrowUp")   { e.preventDefault(); setIdx(i => Math.max(i - 1, 0)); }
      if (e.key === "Enter")     { e.preventDefault(); activateRef.current(flatItemsRef.current[safeIdxRef.current]); }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Layout-specific positioning. VS Code is top-anchored and narrow;
  // Spotlight is centered and large; Raycast is top-anchored, wider,
  // chunkier rows.
  const surfaceClass = `cmdk-surface cmdk-${layout}`;

  let runningIdx = -1;

  return (
    <div className="cmdk-overlay" onClick={onClose} role="dialog" aria-label="Command palette">
      <div className={surfaceClass} onClick={(e) => e.stopPropagation()}>
        <div className="cmdk-input-row">
          <Icon name="command" size={14}/>
          <input
            ref={inputRef}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder={placeholderFor(layout, scope)}
            aria-label="Command palette query"
          />
          <span className="cmdk-esc"><span className="kbd">esc</span></span>
        </div>

        <div className="cmdk-list">
          {sections.map(sec => sec.items.length === 0 ? null : (
            <div key={sec.key} className="cmdk-section">
              <div className="cmdk-section-label">{sec.label}</div>
              {sec.items.map(it => {
                runningIdx++;
                const isActive = runningIdx === safeIdx;
                const myIdx = runningIdx;
                return (
                  <button
                    key={it.id}
                    type="button"
                    className={`cmdk-row ${isActive ? "on" : ""} ${it.danger ? "danger" : ""}`}
                    onMouseEnter={() => setIdx(myIdx)}
                    onClick={() => activate(it)}
                  >
                    <span className="cmdk-icon"><Icon name={it.icon} size={13}/></span>
                    <span className="cmdk-main">
                      <span className="cmdk-label">
                        {cmdkHighlight(it.label, q)}
                        {it.group && <span className="cmdk-meta"> · {it.group}</span>}
                      </span>
                      {it._hit && (
                        <span className="cmdk-sub">↳ {cmdkHighlight(it._hit, q)}</span>
                      )}
                      {!it._hit && it.hint && (
                        <span className="cmdk-sub muted">{it.hint}</span>
                      )}
                    </span>
                    <span className="cmdk-tail">
                      {it.kind === "screen" && it.shortcut && (
                        <span className="cmdk-shortcut">
                          <span className="kbd">{it.shortcut[0]}</span>
                          <span className="kbd">{it.shortcut[1]}</span>
                        </span>
                      )}
                      {it.kind === "action"  && <span className="cmdk-tag">action</span>}
                      {it.kind === "screen"  && <span className="cmdk-tag muted">screen</span>}
                      {it.kind === "setting" && <span className="cmdk-tag muted">setting</span>}
                      {isActive && <span className="kbd">⏎</span>}
                    </span>
                  </button>
                );
              })}
            </div>
          ))}
          {flatItems.length === 0 && (
            <div className="cmdk-empty">
              No matches for <span className="mono">"{q}"</span>.
            </div>
          )}
        </div>

        <div className="cmdk-foot">
          <span><span className="kbd">↑</span><span className="kbd">↓</span> navigate</span>
          <span><span className="kbd">⏎</span> open</span>
          <span><span className="kbd">esc</span> close</span>
          <span className="muted" style={{ paddingLeft: "10px", borderLeft: "1px solid var(--sw-line)", marginLeft: "4px" }}>
            <span className="kbd">g</span> then a letter to jump
          </span>
          <span style={{ marginLeft: "auto" }} className="cmdk-layout-switch">
            <span className="muted">layout</span>
            {["vscode", "spotlight", "raycast"].map(L => (
              <button
                key={L}
                type="button"
                className={layout === L ? "on" : ""}
                onClick={() => cmdkSetLayout(L)}
              >{L}</button>
            ))}
          </span>
        </div>
      </div>
    </div>
  );
}

function placeholderFor(layout, scope) {
  if (layout === "raycast")    return "What would you like to do?";
  if (scope === "screens")     return "Jump to a screen…";
  if (scope === "settings")    return "Find a setting…";
  return "Type a command, search settings, jump to a screen…";
}

/* Tiny toast for action feedback. We don't ship a toast system, and a
 * real one is out of scope for this exploration — this is just enough
 * to make the palette feel like it does something. */
function cmdkFlashToast(label) {
  if (typeof document === "undefined") return;
  let host = document.getElementById("cmdk-toast-host");
  if (!host) {
    host = document.createElement("div");
    host.id = "cmdk-toast-host";
    host.className = "cmdk-toast-host";
    document.body.appendChild(host);
  }
  const t = document.createElement("div");
  t.className = "cmdk-toast";
  t.textContent = label + " · queued";
  host.appendChild(t);
  requestAnimationFrame(() => t.classList.add("in"));
  setTimeout(() => {
    t.classList.remove("in");
    setTimeout(() => t.remove(), 220);
  }, 1800);
}

Object.assign(window, { CommandPaletteHost });
