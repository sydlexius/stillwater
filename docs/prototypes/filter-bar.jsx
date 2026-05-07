/* UniversalFilterBar — the same component used on Dashboard, Artists, Reports.
 * Solves the "filtering is inconsistent" pain point.
 *
 * API:
 *   <UniversalFilterBar
 *     scope=["all", "compliant", "pending"]   // segmented quick-filter
 *     activeScope
 *     facets={[{key, label, options:[{value,label,count}]}]}  // dimensions
 *     active={severity:"warning", category:"image"}            // current chips
 *     savedViews={[{id,label}]}                                // bookmarked combinations
 *     onSearch, onChip, onClear
 *   />
 *
 * Keyboard:
 *   /     focus search
 *   f     open filter flyout
 *   esc   clear focus / close flyout
 */

const { useState: ufState, useEffect: ufEffect, useRef: ufRef } = React;

function UniversalFilterBar({
  placeholder = "Search artists, files, rules…",
  scope = ["All", "Needs action", "Compliant"],
  activeScope = "All",
  setActiveScope = () => {},
  facets = [],
  active = {},
  setActive = () => {},
  savedViews = [],
  resultCount,
}) {
  const [query, setQuery] = ufState("");
  const [showFlyout, setShowFlyout] = ufState(false);
  const inputRef = ufRef(null);

  ufEffect(() => {
    function onKey(e) {
      const tag = document.activeElement?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA") return;
      if (e.key === "/") { e.preventDefault(); inputRef.current?.focus(); }
      if (e.key === "f") { e.preventDefault(); setShowFlyout(v => !v); }
      if (e.key === "Escape") { setShowFlyout(false); }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const activeChips = Object.entries(active).filter(([_, v]) => v);

  function clearChip(key) {
    setActive({ ...active, [key]: null });
  }
  function clearAll() {
    setActive({});
    setQuery("");
  }

  return (
    <div style={{ position: "relative" }}>
      <div className="sw-fbar">
        <div className="search">
          <Icon name="search" size={14} />
          <input
            ref={inputRef}
            className="input with-icon"
            placeholder={placeholder}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            style={{ height: 32 }}
          />
          <span className="kbd" style={{ position: "absolute", right: 8, top: "50%", transform: "translateY(-50%)" }}>/</span>
        </div>
        <div className="scope">
          {scope.map(s => (
            <button key={s} className={activeScope === s ? "active" : ""} onClick={() => setActiveScope(s)}>{s}</button>
          ))}
        </div>
        <button className={`btn ghost ${showFlyout ? "" : ""}`} onClick={() => setShowFlyout(v => !v)}>
          <Icon name="filter" size={14} />
          <span>Filters</span>
          {activeChips.length > 0 && <span className="chip active" style={{ height: 18, marginLeft: 4 }}>{activeChips.length}</span>}
          <span className="kbd">f</span>
        </button>
        {savedViews.length > 0 && (
          <div className="saved row" style={{ gap: 6 }}>
            <span className="muted" style={{ fontSize: 12 }}>·</span>
            {savedViews.map(v => (
              <button key={v.id} className="btn ghost sm">{v.label}</button>
            ))}
          </div>
        )}
        {resultCount != null && (
          <div className="muted" style={{ fontSize: 12, marginLeft: "auto" }}>
            {resultCount.toLocaleString()} {resultCount === 1 ? "result" : "results"}
          </div>
        )}
      </div>

      {(activeChips.length > 0 || query) && (
        <div className="sw-fbar-chips">
          {query && (
            <span className="chip active">
              search: <span className="mono">{query}</span>
              <button className="x" aria-label="Clear search" onClick={() => setQuery("")}><Icon name="x" size={10} /></button>
            </span>
          )}
          {activeChips.map(([k, v]) => {
            const f = facets.find(f => f.key === k);
            const opt = f?.options.find(o => o.value === v);
            return (
              <span key={k} className="chip active">
                <span className="muted" style={{ marginRight: 4 }}>{f?.label || k}:</span>
                {opt?.label || v}
                <button className="x" aria-label={`Remove ${f?.label || k} filter`} onClick={() => clearChip(k)}><Icon name="x" size={10} /></button>
              </span>
            );
          })}
          <button className="btn ghost sm" onClick={clearAll}>Clear all</button>
        </div>
      )}

      {showFlyout && (
        <FilterFlyout
          facets={facets}
          active={active}
          setActive={setActive}
          onClose={() => setShowFlyout(false)}
        />
      )}
    </div>
  );
}

function FilterFlyout({ facets, active, setActive, onClose }) {
  return (
    <div
      style={{
        position: "absolute", right: 0, top: "calc(100% + 8px)", width: 380, zIndex: 30,
        background: "var(--sw-bg-raised)", border: "1px solid var(--sw-line-strong)", borderRadius: 12,
        boxShadow: "var(--sw-shadow-2)", overflow: "hidden",
      }}
    >
      <div style={{ display: "flex", alignItems: "center", padding: "12px 16px", borderBottom: "1px solid var(--sw-line)" }}>
        <strong style={{ fontSize: 13 }}>Filters</strong>
        <span className="muted" style={{ fontSize: 11.5, marginLeft: 8 }}>Same on Dashboard, Artists, Reports.</span>
        <button className="btn ghost icon" style={{ marginLeft: "auto" }} onClick={onClose}>
          <Icon name="x" size={14} />
        </button>
      </div>
      <div style={{ padding: 12, maxHeight: 460, overflowY: "auto" }}>
        {facets.map(f => (
          <div key={f.key} style={{ marginBottom: 14 }}>
            <div style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.06em", color: "var(--sw-ink-3)", marginBottom: 6, padding: "0 4px" }}>
              {f.label}
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: 1 }}>
              {f.options.map(opt => {
                const isActive = active[f.key] === opt.value;
                return (
                  <button
                    key={opt.value}
                    className="row"
                    onClick={() => setActive({ ...active, [f.key]: isActive ? null : opt.value })}
                    style={{
                      padding: "6px 10px", borderRadius: 6, fontSize: 13,
                      background: isActive ? "var(--sw-blue-soft)" : "transparent",
                      color: isActive ? "var(--sw-blue-ink)" : "var(--sw-ink-2)",
                      justifyContent: "space-between",
                    }}
                  >
                    <span className="row">
                      {isActive ? <Icon name="check" size={12} /> : <span style={{ width: 12 }} />}
                      {opt.icon && <span style={{ color: opt.iconColor || "currentColor" }}>{opt.icon}</span>}
                      <span>{opt.label}</span>
                    </span>
                    <span className="muted mono" style={{ fontSize: 11 }}>{opt.count?.toLocaleString() ?? ""}</span>
                  </button>
                );
              })}
            </div>
          </div>
        ))}
      </div>
      <div style={{ display: "flex", padding: "10px 12px", borderTop: "1px solid var(--sw-line)", gap: 8, alignItems: "center" }}>
        <button className="btn ghost sm" onClick={() => setActive({})}>Clear</button>
        <span className="muted" style={{ fontSize: 11.5, marginLeft: "auto" }}>Press <span className="kbd">esc</span> to close</span>
      </div>
    </div>
  );
}

Object.assign(window, { UniversalFilterBar, FilterFlyout });
