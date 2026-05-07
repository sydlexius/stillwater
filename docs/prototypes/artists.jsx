/* Artists list — main screen.
 * Bulk pieces (toolbar, dry-run drawer, progress card) live in artists-bulk.jsx.
 */

function ArtistsListProposal({ density: densityProp = "comfy" }) {
  const { useState, useMemo } = React;

  const [scope, setScope] = useState("All"); // All / Group / Person
  const [activeFacets, setActiveFacets] = useState({}); // {key: "value"}
  const [sortBy, setSortBy] = useState("compliance");
  const [sortDir, setSortDir] = useState("asc");

  // Selection persists across filter changes
  const [selected, setSelected] = useState(() => new Set());

  // Bulk action lifecycle
  const [bulkAction, setBulkAction] = useState(null); // {kind, target: 'selected'|'matching'}
  const [progressJobs, setProgressJobs] = useState([]);
  const [toast, setToast] = useState(null);

  const density = (selected.size > 0 || bulkAction) ? "compact" : densityProp;

  const filtered = useMemo(() => {
    let rows = allArtists;
    if (scope === "Group")  rows = rows.filter(r => r.type === "group");
    if (scope === "Person") rows = rows.filter(r => r.type === "person");
    if (activeFacets.library) {
      const v = activeFacets.library;
      rows = rows.filter(r => r.lib.toLowerCase().split(" ")[0] === v);
    }
    if (activeFacets.compliance) {
      const v = activeFacets.compliance;
      rows = rows.filter(r => {
        if (v === "perfect")  return r.compliance === 100;
        if (v === "good")     return r.compliance >= 70 && r.compliance < 100;
        if (v === "warn")     return r.compliance >= 40 && r.compliance < 70;
        if (v === "critical") return r.compliance < 40;
        return true;
      });
    }
    if (activeFacets.field) {
      const v = activeFacets.field;
      rows = rows.filter(r => !r.fields[v]);
    }
    rows = [...rows].sort((a, b) => {
      let cmp = 0;
      if (sortBy === "name")       cmp = a.name.localeCompare(b.name);
      if (sortBy === "compliance") cmp = a.compliance - b.compliance;
      if (sortBy === "library")    cmp = a.lib.localeCompare(b.lib);
      return sortDir === "asc" ? cmp : -cmp;
    });
    return rows;
  }, [scope, activeFacets, sortBy, sortDir]);

  const filterIsActive = scope !== "All" || Object.values(activeFacets).some(v => v);

  function toggleOne(name) {
    setSelected(prev => {
      const n = new Set(prev);
      if (n.has(name)) n.delete(name); else n.add(name);
      return n;
    });
  }
  function toggleAllVisible() {
    setSelected(prev => {
      const allVisibleSelected = filtered.length > 0 && filtered.every(r => prev.has(r.name));
      const n = new Set(prev);
      if (allVisibleSelected) filtered.forEach(r => n.delete(r.name));
      else filtered.forEach(r => n.add(r.name));
      return n;
    });
  }
  function clearSelection() { setSelected(new Set()); }

  function openBulkAction(kind, target) {
    setBulkAction({ kind, target });
  }
  function commitBulkAction(kind, target, summary) {
    setBulkAction(null);
    if (target === "selected") clearSelection();
    const id = "job-" + Date.now();
    const total = target === "matching" ? filtered.length : summary.total;
    setProgressJobs(jobs => [...jobs, { id, label: actionLabel(kind), pct: 0, total }]);
    let p = 0;
    const tick = () => {
      p += Math.random() * 22 + 8;
      if (p >= 100) {
        setProgressJobs(jobs => jobs.filter(j => j.id !== id));
        const undoable = !["rerun-rules", "refetch", "refetch-images"].includes(kind);
        setToast({
          kind: "ok",
          msg: `${actionLabel(kind)} complete · ${summary.willChange} of ${summary.total} updated`,
          undo: undoable,
        });
      } else {
        setProgressJobs(jobs => jobs.map(j => j.id === id ? { ...j, pct: Math.min(p, 99) } : j));
        setTimeout(tick, 450);
      }
    };
    setTimeout(tick, 350);
  }

  return (
    <div data-density={density} className="sw-anno" style={{ position: "relative" }}>
      <Sidebar active="artists" actionsCount={47} />
      <div className="sw-main">
        <PageHead
          title="Artists"
          sub="36 in library · 14 with violations"
          right={<>
            <button className="btn ghost"><Icon name="tag" size={12}/> Saved views</button>
            <button className="btn primary"><Icon name="plus" size={12}/> Add artist</button>
          </>}
        />

        <UniversalFilterBar
          placeholder="Filter by name, MBID, file path…"
          scope={["All", "Group", "Person"]}
          activeScope={scope}
          setActiveScope={setScope}
          facets={[
            { key: "library", label: "Library", options: [
              { value: "main",      label: "Main",      count: 26 },
              { value: "classical", label: "Classical", count: 12 },
            ]},
            { key: "compliance", label: "Compliance", options: [
              { value: "critical", label: "< 40 %",  count: 6 },
              { value: "warn",     label: "40–69 %", count: 11 },
              { value: "good",     label: "70–99 %", count: 12 },
              { value: "perfect",  label: "100 %",   count: 7 },
            ]},
            { key: "field", label: "Missing field", options: [
              { value: "bio",    label: "Biography", count: 4 },
              { value: "thumb",  label: "Thumb",     count: 5 },
              { value: "fanart", label: "Fanart",    count: 13 },
              { value: "logo",   label: "Logo",      count: 18 },
              { value: "nfo",    label: "NFO",       count: 7 },
            ]},
          ]}
          active={activeFacets}
          setActive={setActiveFacets}
          resultCount={filtered.length}
        />

        <BulkActionBar
          selectedCount={selected.size}
          visibleCount={filtered.length}
          filterIsActive={filterIsActive}
          totalCount={allArtists.length}
          ceiling={SOFT_SELECT_CEILING}
          onClear={clearSelection}
          onAction={(kind, target) => openBulkAction(kind, target)}
        />

        <div className="sw-card sw-list" style={{ marginTop: 12, padding: 0, overflow: "hidden" }}>
          <ArtistsTable
            rows={filtered}
            selected={selected}
            onToggleOne={toggleOne}
            onToggleAllVisible={toggleAllVisible}
            sortBy={sortBy}
            sortDir={sortDir}
            onSort={(col) => {
              if (sortBy === col) setSortDir(d => d === "asc" ? "desc" : "asc");
              else { setSortBy(col); setSortDir("asc"); }
            }}
          />
        </div>

        <div className="muted" style={{ marginTop: 10, fontSize: 11.5 }}>
          Tip: <span className="kbd">Shift</span>+click to select a range · <span className="kbd">⌘A</span> to select all visible · <span className="kbd">Esc</span> to clear
        </div>
      </div>

      {bulkAction && (
        <BulkPreviewDrawer
          action={bulkAction}
          rows={filtered}
          selectedNames={selected}
          onCancel={() => setBulkAction(null)}
          onApply={(summary) => commitBulkAction(bulkAction.kind, bulkAction.target, summary)}
        />
      )}

      <ProgressStack jobs={progressJobs} />

      {toast && <Toast toast={toast} onDismiss={() => setToast(null)} />}
    </div>
  );
}

function ArtistsTable({ rows, selected, onToggleOne, onToggleAllVisible, sortBy, sortDir, onSort }) {
  const allVisibleSelected = rows.length > 0 && rows.every(r => selected.has(r.name));
  const someVisibleSelected = !allVisibleSelected && rows.some(r => selected.has(r.name));

  return (
    <table className="sw-list-table">
      <colgroup>
        <col style={{ width: 36 }} />
        <col />
        <col style={{ width: 110 }} />
        <col style={{ width: 80 }} />
        <col style={{ width: 130 }} />
        <col style={{ width: 110 }} />
        <col style={{ width: 110 }} />
      </colgroup>
      <thead>
        <tr>
          <th>
            <Checkbox
              checked={allVisibleSelected}
              indeterminate={someVisibleSelected}
              onChange={onToggleAllVisible}
              ariaLabel="Select all visible"
            />
          </th>
          <SortHeader col="name"       label="Artist"     sortBy={sortBy} sortDir={sortDir} onSort={onSort}/>
          <SortHeader col="library"    label="Library"    sortBy={sortBy} sortDir={sortDir} onSort={onSort}/>
          <th>Type</th>
          <th>Sources</th>
          <th>Coverage</th>
          <SortHeader col="compliance" label="Score"      sortBy={sortBy} sortDir={sortDir} onSort={onSort} align="right"/>
        </tr>
      </thead>
      <tbody>
        {rows.length === 0 && (
          <tr><td colSpan="7" style={{ padding: 28, textAlign: "center" }} className="muted">No artists match the current filter.</td></tr>
        )}
        {rows.map(r => (
          <tr key={r.name} className={selected.has(r.name) ? "is-selected" : ""}>
            <td>
              <Checkbox checked={selected.has(r.name)} onChange={() => onToggleOne(r.name)} ariaLabel={`Select ${r.name}`}/>
            </td>
            <td>
              <div className="row" style={{ gap: 10 }}>
                <div style={{ width: 22, height: 22, borderRadius: "50%", background: "linear-gradient(135deg,#475569,#1e293b)", display: "grid", placeItems: "center", fontSize: 10, fontWeight: 600, color: "var(--sw-ink-2)" }}>
                  {r.name.split(" ").map(s => s[0]).slice(0, 2).join("").toUpperCase()}
                </div>
                <span style={{ fontSize: 13, fontWeight: 500 }}>{r.name}</span>
              </div>
            </td>
            <td className="muted" style={{ fontSize: 12 }}>{r.lib}</td>
            <td className="muted" style={{ fontSize: 12 }}>{r.type}</td>
            <td>
              <div className="row" style={{ gap: 4 }}>
                {["local","emby","lidarr"].map(s => (
                  <span key={s}
                    title={s + (r.srcs.includes(s) ? " — present" : " — missing")}
                    style={{
                      display: "inline-block", width: 16, height: 14, borderRadius: 3,
                      background: r.srcs.includes(s) ? "var(--sw-ok-soft, rgba(34,197,94,0.18))" : "transparent",
                      border: "1px solid " + (r.srcs.includes(s) ? "var(--sw-ok)" : "var(--sw-line)"),
                      color: r.srcs.includes(s) ? "var(--sw-ok)" : "var(--sw-ink-4)",
                      fontSize: 9, fontWeight: 700, textAlign: "center", lineHeight: "12px",
                      fontFamily: "var(--sw-mono)",
                    }}>
                    {s[0].toUpperCase()}
                  </span>
                ))}
              </div>
            </td>
            <td>
              <CoverageBar fields={r.fields} idsHave={r.idsHave} idsTotal={r.idsTotal}/>
            </td>
            <td style={{ textAlign: "right" }}>
              <ScoreCell score={r.compliance}/>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function SortHeader({ col, label, sortBy, sortDir, onSort, align }) {
  const active = sortBy === col;
  return (
    <th style={{ textAlign: align || "left", cursor: "pointer", userSelect: "none" }} onClick={() => onSort(col)}>
      <span className="row" style={{ gap: 4, justifyContent: align === "right" ? "flex-end" : "flex-start" }}>
        {label}
        {active && <span style={{ fontSize: 10, color: "var(--sw-ink-3)" }}>{sortDir === "asc" ? "▲" : "▼"}</span>}
      </span>
    </th>
  );
}

function Checkbox({ checked, indeterminate, onChange, ariaLabel }) {
  const ref = React.useRef(null);
  React.useEffect(() => {
    if (ref.current) ref.current.indeterminate = !!indeterminate;
  }, [indeterminate]);
  return (
    <input
      ref={ref}
      type="checkbox"
      checked={!!checked}
      onChange={onChange}
      aria-label={ariaLabel}
      className="sw-checkbox"
    />
  );
}

function CoverageBar({ fields, idsHave, idsTotal }) {
  const order = [["bio","B"],["thumb","T"],["fanart","F"],["logo","L"],["nfo","N"]];
  return (
    <div className="row" style={{ gap: 6, alignItems: "center" }}>
      <div className="row" style={{ gap: 2 }}>
        {order.map(([k, lbl]) => (
          <span key={k}
            title={k + (fields[k] ? " — present" : " — missing")}
            style={{
              width: 12, height: 12, borderRadius: 2,
              background: fields[k] ? "var(--sw-ok)" : "var(--sw-warm-soft, rgba(245,158,11,0.18))",
              border: "1px solid " + (fields[k] ? "var(--sw-ok)" : "var(--sw-warm)"),
              fontSize: 8, color: fields[k] ? "#fff" : "var(--sw-warm)",
              textAlign: "center", lineHeight: "10px", fontWeight: 700,
            }}>{lbl}</span>
        ))}
      </div>
      <span className="muted" style={{ fontSize: 11, fontVariantNumeric: "tabular-nums" }}>
        {idsHave}/{idsTotal} IDs
      </span>
    </div>
  );
}

function ScoreCell({ score }) {
  const tone = score === 100 ? "ok" : score >= 70 ? "info" : score >= 40 ? "warn" : "err";
  const color = tone === "ok" ? "var(--sw-ok)" : tone === "warn" ? "var(--sw-warm)" : tone === "err" ? "var(--sw-err)" : "var(--sw-blue)";
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 6, fontVariantNumeric: "tabular-nums", fontSize: 12.5, fontWeight: 600, color }}>
      <span style={{ width: 6, height: 6, borderRadius: 999, background: color }}></span>
      {score}%
    </span>
  );
}

Object.assign(window, { ArtistsListProposal });
