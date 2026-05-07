/* Shared shell: sidebar + page chrome.
 * Mirrors the live Stillwater sidebar (Dashboard, Artists, Reports, Activity, Compliance, Settings)
 * but tightens visual rhythm and adds keyboard hints.
 */

const { useState } = React;

function Icon({ name, size = 16 }) {
  // Minimal stroke icons — kept abstract; we never invent imagery.
  const s = size;
  const stroke = { stroke: "currentColor", strokeWidth: 1.6, fill: "none", strokeLinecap: "round", strokeLinejoin: "round" };
  switch (name) {
    case "home": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M3 11l9-7 9 7v9a2 2 0 0 1-2 2h-4v-7h-6v7H5a2 2 0 0 1-2-2z"/></svg>;
    case "artists": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><circle cx="9" cy="8" r="3"/><path d="M3 20a6 6 0 0 1 12 0"/><circle cx="17" cy="6" r="2"/><path d="M21 18a4 4 0 0 0-4-4"/></svg>;
    case "reports": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M4 19V5"/><path d="M4 19h16"/><rect x="7" y="11" width="3" height="6"/><rect x="12" y="7" width="3" height="10"/><rect x="17" y="13" width="3" height="4"/></svg>;
    case "activity": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M3 12h4l3-8 4 16 3-8h4"/></svg>;
    case "compliance": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M12 3l8 4v6c0 5-3.5 7.5-8 8-4.5-.5-8-3-8-8V7z"/><path d="M9 12l2 2 4-4"/></svg>;
    case "settings": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1A2 2 0 1 1 4.3 17l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1A1.7 1.7 0 0 0 4.7 9a1.7 1.7 0 0 0-.3-1.8l-.1-.1A2 2 0 1 1 7 4.3l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/></svg>;
    case "search": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>;
    case "filter": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M3 5h18l-7 9v6l-4-2v-4z"/></svg>;
    case "clock": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>;
    case "music": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>;
    case "image": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><rect x="3" y="4" width="18" height="16" rx="2"/><circle cx="9" cy="10" r="2"/><path d="m21 16-5-5-9 9"/></svg>;
    case "tag": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M3 12V3h9l9 9-9 9z"/><circle cx="7.5" cy="7.5" r="1"/></svg>;
    case "x": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M6 6l12 12M18 6 6 18"/></svg>;
    case "check": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="m5 12 5 5L20 7"/></svg>;
    case "chevron": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="m9 6 6 6-6 6"/></svg>;
    case "plus": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M12 5v14M5 12h14"/></svg>;
    case "warn": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M12 3 2 21h20z"/><path d="M12 9v5"/><circle cx="12" cy="17" r=".5" fill="currentColor"/></svg>;
    case "info": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><circle cx="12" cy="12" r="9"/><path d="M12 11v6"/><circle cx="12" cy="8" r=".5" fill="currentColor"/></svg>;
    case "key": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><circle cx="8" cy="15" r="4"/><path d="m11 12 9-9"/><path d="m17 6 3 3"/></svg>;
    case "server": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><circle cx="7" cy="7" r=".5" fill="currentColor"/><circle cx="7" cy="17" r=".5" fill="currentColor"/></svg>;
    case "folder": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M3 6a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>;
    case "bell": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M6 8a6 6 0 1 1 12 0c0 7 3 8 3 8H3s3-1 3-8"/><path d="M10 21a2 2 0 0 0 4 0"/></svg>;
    case "shield": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M12 3l8 3v6c0 5-3.5 7.5-8 9-4.5-1.5-8-4-8-9V6z"/></svg>;
    case "users": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><circle cx="9" cy="8" r="3"/><path d="M3 20a6 6 0 0 1 12 0"/><circle cx="17" cy="7" r="2.5"/><path d="M22 18a4 4 0 0 0-5-3.9"/></svg>;
    case "wrench": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M14 7a4 4 0 0 1 5.5 5.5L21 14l-2 2-1.5-1.5A4 4 0 0 1 12 9l-7 7a2 2 0 0 0 0 3 2 2 0 0 0 3 0l7-7"/></svg>;
    case "terminal": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><rect x="3" y="4" width="18" height="16" rx="2"/><path d="m7 9 3 3-3 3"/><path d="M13 15h4"/></svg>;
    case "logs": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M5 4h14v16H5z"/><path d="M9 8h6M9 12h6M9 16h4"/></svg>;
    case "download": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M12 4v12"/><path d="m7 11 5 5 5-5"/><path d="M5 20h14"/></svg>;
    case "command": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M9 6V3a3 3 0 1 0-3 3h12a3 3 0 1 0-3-3v3"/><path d="M9 18v3a3 3 0 1 1-3-3h12a3 3 0 1 1-3 3v-3"/><rect x="9" y="6" width="6" height="12"/></svg>;
    case "bolt": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M13 3 4 14h7l-1 7 9-11h-7z"/></svg>;
    case "chevron-down": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="m6 9 6 6 6-6"/></svg>;
    case "refresh": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M21 12a9 9 0 1 1-3-6.7L21 8"/><path d="M21 3v5h-5"/></svg>;
    case "upload": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M12 16V4"/><path d="m7 9 5-5 5 5"/><path d="M5 20h14"/></svg>;
    case "copy": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h8"/></svg>;
    case "pencil": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M4 20h4l11-11-4-4L4 16z"/><path d="m13.5 5.5 4 4"/></svg>;
    case "trash": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M4 7h16"/><path d="M9 7V4h6v3"/><path d="M6 7v13a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V7"/><path d="M10 11v6M14 11v6"/></svg>;
    case "link": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M10 14a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1"/><path d="M14 10a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1"/></svg>;
    case "crop": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M6 2v16a1 1 0 0 0 1 1h15"/><path d="M22 18H7a1 1 0 0 1-1-1V2"/><path d="M2 6h4M18 22v-4"/></svg>;
    case "scissors": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><circle cx="6" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M20 4L8.12 15.88M14.47 14.48L20 20M8.12 8.12L12 12"/></svg>;
    case "arrow-left": return <svg width={s} height={s} viewBox="0 0 24 24" {...stroke}><path d="M19 12H5"/><path d="m11 6-6 6 6 6"/></svg>;
    default: return null;
  }
}

function Sidebar({ active = "dashboard", rail = "full", actionsCount = 24 }) {
  const items = [
    { id: "dashboard", label: "Dashboard", icon: "home", badge: actionsCount },
    { id: "artists", label: "Artists", icon: "artists" },
    { id: "reports", label: "Reports", icon: "reports" },
    { id: "activity", label: "Activity", icon: "activity" },
    { id: "compliance", label: "Compliance", icon: "compliance" },
  ];
  const sysItems = [
    { id: "logs", label: "Logs", icon: "logs" },
    { id: "settings", label: "Settings", icon: "settings" },
  ];
  return (
    <aside className="sw-side">
      <div className="brand">
        <div className="mark"></div>
        {rail === "full" && <span className="name">Stillwater</span>}
        {rail === "full" && <span className="ver">v0.9.4</span>}
      </div>
      <nav className="nav">
        {items.map(it => (
          <a key={it.id} className={`sw-nav-item ${active === it.id ? "active" : ""}`}>
            <Icon name={it.icon} />
            {rail === "full" && <span>{it.label}</span>}
            {rail === "full" && it.badge > 0 && <span className="badge">{it.badge}</span>}
          </a>
        ))}
        {rail === "full" && <div className="sw-nav-section">System</div>}
        {sysItems.map(it => (
          <a key={it.id} className={`sw-nav-item ${active === it.id ? "active" : ""}`}>
            <Icon name={it.icon} />
            {rail === "full" && <span>{it.label}</span>}
          </a>
        ))}
      </nav>
      {rail === "full" && (
        <div className="footer">
          <div className="avatar">SD</div>
          <div style={{ fontSize: 12, lineHeight: 1.2 }}>
            <div>syd</div>
            <div className="muted" style={{ fontSize: 11 }}>admin</div>
          </div>
        </div>
      )}
    </aside>
  );
}

function PageHead({ title, sub, right }) {
  return (
    <div className="sw-page-head">
      <div>
        <h1>{title}</h1>
        {sub && <div className="sub">{sub}</div>}
      </div>
      <div className="row">{right}</div>
    </div>
  );
}

function Callout({ n, x, y, pos = "bottom", tone = "warm", children, w }) {
  const cls = `sw-callout ${tone === "cool" ? "cool" : ""}`;
  return (
    <div className={cls} style={{ left: x, top: y, maxWidth: w }} data-pos={pos}>
      {n != null && <span className="num">{String(n).padStart(2, "0")}</span>}{children}
    </div>
  );
}

Object.assign(window, { Icon, Sidebar, PageHead, Callout });
