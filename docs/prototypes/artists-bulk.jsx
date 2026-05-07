/* Bulk-edit pieces.
 *
 * BulkActionBar — selection chip + targeted action buttons. Visible only when
 *   (selected.size > 0) or a filter is active. When both are true it shows the
 *   "Selected vs Matching" duality so the scope of the next action is explicit.
 *
 * BulkPreviewDrawer — mandatory dry-run. "Will change N, skip M, conflicts K".
 *   Apply is the only commit path; closing without Apply leaves data untouched.
 *
 * ProgressStack — bottom-right, dismissible, multiple jobs stack vertically.
 * Toast — completion message with Undo (when undoable, 14-day window).
 */

function BulkActionBar({
  selectedCount, visibleCount, filterIsActive, totalCount,
  ceiling, onClear, onAction,
}) {
  // Default scope: prefer "selected" when there's a selection, otherwise the
  // filter narrows the set to "matching". The scope buttons let the user
  // override that default — e.g. with a selection AND a filter, switch to
  // "matching" to apply across the filtered superset rather than the picks.
  const defaultTarget = selectedCount > 0 ? "selected" : "matching";
  const [target, setTarget] = React.useState(defaultTarget);

  // Re-anchor the target when prerequisites flip (selection cleared,
  // filter cleared) so we never end up pointing at a disabled button.
  React.useEffect(() => {
    if (target === "selected" && selectedCount === 0) setTarget("matching");
    if (target === "matching" && !filterIsActive) setTarget("selected");
  }, [selectedCount, filterIsActive, target]);

  if (selectedCount === 0 && !filterIsActive) return null;

  const overCeiling = visibleCount > ceiling;

  return (
    <div className="sw-bulkbar" role="region" aria-label="Bulk actions">
      <div className="row" style={{ gap: 10, flexWrap: "wrap", alignItems: "center" }}>
        {selectedCount > 0 && (
          <span className="sw-bulk-chip">
            <span className="dot"></span>
            <strong>{selectedCount}</strong> selected
            <button className="x" onClick={onClear} aria-label="Clear selection">
              <Icon name="x" size={11}/>
            </button>
          </span>
        )}

        {filterIsActive && selectedCount === 0 && (
          <span className="sw-bulk-chip is-matching">
            <Icon name="filter" size={11}/>
            <strong>{visibleCount}</strong> match the current filter
          </span>
        )}

        {filterIsActive && selectedCount > 0 && (
          <span className="muted" style={{ fontSize: 12 }}>
            (filter shows <strong>{visibleCount}</strong> of {totalCount})
          </span>
        )}

        <span className="flex-1"></span>

        <span className="muted" style={{ fontSize: 11.5 }}>Apply to:</span>

        <div className="sw-scope-toggle" role="group" aria-label="Action target">
          <button
            className={target === "selected" ? "active" : ""}
            disabled={selectedCount === 0}
            onClick={() => setTarget("selected")}
            aria-pressed={target === "selected"}
          >
            Selected ({selectedCount})
          </button>
          <button
            className={target === "matching" ? "active" : ""}
            disabled={!filterIsActive}
            onClick={() => setTarget("matching")}
            aria-pressed={target === "matching"}
            title={overCeiling ? `Above ${ceiling}-row soft ceiling — confirm in preview` : undefined}
          >
            All matching ({visibleCount}){overCeiling && " ⚠"}
          </button>
        </div>

        <BulkActionMenu
          target={target}
          onAction={onAction}
          disabled={selectedCount === 0 && !filterIsActive}
        />
      </div>
    </div>
  );
}

function BulkActionMenu({ target, onAction, disabled }) {
  const [open, setOpen] = React.useState(false);
  const ref = React.useRef(null);

  React.useEffect(() => {
    function onDoc(e) { if (ref.current && !ref.current.contains(e.target)) setOpen(false); }
    if (open) document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  const items = [
    { kind: "rerun-rules",    label: "Re-run rules",         icon: "check", desc: "No data changes — refresh violation list" },
    { kind: "refetch",        label: "Re-fetch metadata",    icon: "refresh", desc: "Pull fresh data from enabled providers" },
    { kind: "refetch-images", label: "Re-fetch images",      icon: "image", desc: "Replace any image worse than the rules require" },
    { kind: "set-field",      label: "Set / clear field…",   icon: "tag",   desc: "Bio, language, sort name, manual overrides" },
    { kind: "tag",            label: "Add or remove tag…",   icon: "tag",   desc: "Apply a label like 'review' or 'imported'" },
  ];

  return (
    <div ref={ref} style={{ position: "relative" }}>
      <button
        className="btn primary"
        disabled={disabled}
        onClick={() => setOpen(o => !o)}
        aria-expanded={open}
        aria-haspopup="menu"
      >
        <Icon name="bolt" size={12}/> Bulk action <Icon name="chevron-down" size={11}/>
      </button>
      {open && (
        <div className="sw-menu" role="menu">
          <div className="sw-menu-head">
            <Icon name="check" size={11}/> Dry-run preview shown before any change
          </div>
          {items.map(it => (
            <button
              key={it.kind}
              role="menuitem"
              className="sw-menu-item"
              onClick={() => { setOpen(false); onAction(it.kind, target); }}
            >
              <span className="sw-menu-icon"><Icon name={it.icon} size={13}/></span>
              <div style={{ minWidth: 0 }}>
                <div className="lbl">{it.label}</div>
                <div className="desc">{it.desc}</div>
              </div>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function BulkPreviewDrawer({ action, rows, selectedNames, onCancel, onApply }) {
  const targetRows = action.target === "selected"
    ? rows.filter(r => selectedNames.has(r.name)).concat(
        // pull selected-but-out-of-filter rows in too
        allArtists.filter(r => selectedNames.has(r.name) && !rows.find(x => x.name === r.name))
      )
    : rows;

  // Pretend each action affects a slightly different subset.
  const summary = computeBulkSummary(action.kind, targetRows);

  return (
    <div className="sw-drawer" role="dialog" aria-label="Bulk action preview">
      <div className="sw-drawer-head">
        <div>
          <div className="muted" style={{ fontSize: 11.5 }}>Dry-run preview · nothing has changed yet</div>
          <h3 style={{ margin: "2px 0 0", fontSize: 16 }}>{actionLabel(action.kind)}</h3>
        </div>
        <button className="btn ghost sm" onClick={onCancel} aria-label="Close">
          <Icon name="x" size={12}/>
        </button>
      </div>

      <div className="sw-drawer-body">
        <div className="sw-summary-row">
          <SummaryStat label="Will change" value={summary.willChange} tone="ok"/>
          <SummaryStat label="No change"   value={summary.noChange}   tone="muted"/>
          <SummaryStat label="Skipped"     value={summary.skipped}    tone="warn" hint={summary.skipReason}/>
          {summary.conflicts > 0 && <SummaryStat label="Conflicts" value={summary.conflicts} tone="err" hint="Manual override exists"/>}
        </div>

        {action.kind === "set-field" && <SetFieldOptions />}
        {action.kind === "tag"       && <TagOptions />}
        {action.kind === "refetch"   && <RefetchOptions />}
        {action.kind === "refetch-images" && <RefetchImageOptions />}
        {action.kind === "rerun-rules" && <RerunRulesNote/>}

        <div className="sw-card" style={{ marginTop: 12 }}>
          <div className="head">
            <h2>Affected artists ({summary.willChange})</h2>
            <span className="meta">first {Math.min(8, summary.willChange)} shown</span>
          </div>
          <div className="body" style={{ padding: 0, maxHeight: 220, overflow: "auto" }}>
            {targetRows.slice(0, 8).map((r, i) => (
              <div key={r.name} className="row" style={{ padding: "8px 14px", borderTop: i ? "1px solid var(--sw-line)" : "none", gap: 10 }}>
                <span style={{ width: 4, height: 14, borderRadius: 2, background: i % 5 === 4 ? "var(--sw-warm)" : "var(--sw-ok)" }}></span>
                <span style={{ fontSize: 12.5, fontWeight: 500 }}>{r.name}</span>
                <span className="muted" style={{ fontSize: 11.5 }}>{r.lib}</span>
                <span className="flex-1"></span>
                <span className="muted mono" style={{ fontSize: 11 }}>{i % 5 === 4 ? "skip — manual override" : previewDelta(action.kind, r)}</span>
              </div>
            ))}
          </div>
        </div>

        {/* Undo contract — must match the action contract in artists.jsx.
            `rerun-rules`, `refetch`, and `refetch-images` are not undoable
            because they re-derive state from external sources rather than
            mutating it; everything else lands an undo entry in Activity. */}
        {(() => {
          const NON_UNDOABLE = ["rerun-rules", "refetch", "refetch-images"];
          const undoable = !NON_UNDOABLE.includes(action.kind);
          return (
        <div className="sw-card" style={{ marginTop: 12 }}>
          <div className="head"><h2>Behaviour</h2></div>
          <div className="body" style={{ fontSize: 12.5 }}>
            <div className="row" style={{ gap: 8, padding: "6px 0" }}>
              {undoable ? (
                <>
                  <span className="sev info"><span className="dot"></span>Reversible</span>
                  <span className="muted">Undo from Activity for {UNDO_WINDOW_DAYS} days.</span>
                </>
              ) : (
                <>
                  <span className="sev warn"><span className="dot"></span>Not undoable</span>
                  <span className="muted">This action re-derives data from providers and can't be reverted.</span>
                </>
              )}
            </div>
            <div className="row" style={{ gap: 8, padding: "6px 0" }}>
              <span className="sev info"><span className="dot"></span>Skip rules</span>
              <span className="muted">Manual overrides and locked fields are never touched.</span>
            </div>
            <div className="row" style={{ gap: 8, padding: "6px 0" }}>
              <span className="sev info"><span className="dot"></span>Throttled</span>
              <span className="muted">Provider rate limits respected — long jobs run in the background.</span>
            </div>
          </div>
        </div>
          );
        })()}
      </div>

      <div className="sw-drawer-foot">
        <span className="muted" style={{ fontSize: 12 }}>
          {summary.total} artists in scope · <kbd className="kbd">Esc</kbd> to close
        </span>
        <span className="flex-1"></span>
        <button className="btn ghost" onClick={onCancel}>Cancel</button>
        <button className="btn primary" onClick={() => onApply(summary)}>
          Apply to {summary.willChange}
        </button>
      </div>
    </div>
  );
}

function computeBulkSummary(kind, rows) {
  const total = rows.length;
  // Mock realistic skip counts by kind.
  const skipped =
    kind === "rerun-rules"    ? 0 :
    kind === "refetch"        ? Math.floor(total * 0.10) :
    kind === "refetch-images" ? Math.floor(total * 0.08) :
    kind === "set-field"      ? Math.floor(total * 0.20) :
    /* tag */                   Math.floor(total * 0.05);
  const noChange =
    kind === "rerun-rules"    ? 0 :
    Math.floor(total * 0.15);
  const conflicts =
    kind === "set-field"      ? Math.max(1, Math.floor(total * 0.04)) : 0;
  const willChange = Math.max(0, total - skipped - noChange - conflicts);
  const skipReason =
    kind === "set-field" ? "Field locked" :
    kind === "refetch"   ? "Provider key missing" :
    kind === "refetch-images" ? "Already meets rules" :
    kind === "tag"       ? "Already has tag" :
    "";
  return { total, willChange, noChange, skipped, conflicts, skipReason };
}

function previewDelta(kind, r) {
  if (kind === "rerun-rules")    return r.compliance < 100 ? "violations re-evaluated" : "no change";
  if (kind === "refetch")        return r.fields.bio ? "bio + ids" : "bio + thumb + ids";
  if (kind === "refetch-images") return r.fields.fanart ? "thumb only" : "thumb + fanart";
  if (kind === "set-field")      return "language → en";
  if (kind === "tag")            return "+ tag:review";
  return "";
}

function SummaryStat({ label, value, tone, hint }) {
  const color = tone === "ok"   ? "var(--sw-ok)"
              : tone === "warn" ? "var(--sw-warm)"
              : tone === "err"  ? "var(--sw-err)"
              :                   "var(--sw-ink-3)";
  return (
    <div className="sw-summary-stat">
      <div style={{ fontSize: 22, fontWeight: 600, color, fontVariantNumeric: "tabular-nums" }}>{value}</div>
      <div style={{ fontSize: 11.5, color: "var(--sw-ink-3)" }}>{label}</div>
      {hint && <div style={{ fontSize: 10.5, color: "var(--sw-ink-4)", marginTop: 2 }}>{hint}</div>}
    </div>
  );
}

function SetFieldOptions() {
  return (
    <div className="sw-card" style={{ marginBottom: 12 }}>
      <div className="head"><h2>Field to change</h2></div>
      <div className="body">
        <SettingRow label="Field" desc="Pick the metadata or override field to set.">
          <select className="select" defaultValue="language">
            <option value="language">Language</option>
            <option value="sort_name">Sort name</option>
            <option value="bio_override">Bio (override)</option>
            <option value="locked_fields">Locked fields</option>
          </select>
        </SettingRow>
        <SettingRow label="New value" desc="Leave blank to clear.">
          <input className="input" defaultValue="en" style={{ width: 180 }}/>
        </SettingRow>
        <SettingRow label="Only when empty" desc="Skip artists that already have a value.">
          <Toggle on={true}/>
        </SettingRow>
      </div>
    </div>
  );
}
function TagOptions() {
  return (
    <div className="sw-card" style={{ marginBottom: 12 }}>
      <div className="head"><h2>Tag to apply</h2></div>
      <div className="body">
        <SettingRow label="Action" desc="">
          <div className="row" style={{ gap: 4 }}>
            <button className="chip active">Add tag</button>
            <button className="chip">Remove tag</button>
          </div>
        </SettingRow>
        <SettingRow label="Tag" desc="Used in filters and saved views.">
          <input className="input" defaultValue="review" style={{ width: 180 }}/>
        </SettingRow>
      </div>
    </div>
  );
}
function RefetchOptions() {
  return (
    <div className="sw-card" style={{ marginBottom: 12 }}>
      <div className="head"><h2>Re-fetch options</h2></div>
      <div className="body">
        <SettingRow label="Providers" desc="Only enabled providers are queried.">
          <div className="row" style={{ gap: 6, flexWrap: "wrap" }}>
            {["MusicBrainz", "Wikipedia", "AudioDB", "Last.fm", "Discogs"].map(p => (
              <span key={p} className="chip active">{p}</span>
            ))}
          </div>
        </SettingRow>
        <SettingRow label="Fields" desc="Field-priority chain decides which value wins.">
          <div className="row" style={{ gap: 6, flexWrap: "wrap" }}>
            {["Bio","Members","Genres","Country","Year"].map(f => <span key={f} className="chip active">{f}</span>)}
          </div>
        </SettingRow>
        <SettingRow label="Respect manual overrides" desc="Don't replace fields the user has explicitly set.">
          <Toggle on={true}/>
        </SettingRow>
      </div>
    </div>
  );
}
function RefetchImageOptions() {
  return (
    <div className="sw-card" style={{ marginBottom: 12 }}>
      <div className="head"><h2>Re-fetch images</h2></div>
      <div className="body">
        <SettingRow label="Image types" desc="Order = priority for the slot.">
          <div className="row" style={{ gap: 6, flexWrap: "wrap" }}>
            {["Thumb","Fanart","Logo","Banner"].map(t => <span key={t} className="chip active">{t}</span>)}
          </div>
        </SettingRow>
        <SettingRow label="Replace policy" desc="When to overwrite an existing image.">
          <select className="select" defaultValue="below">
            <option value="below">Only if existing fails rules</option>
            <option value="lower">Only if new image scores higher</option>
            <option value="always">Always replace</option>
          </select>
        </SettingRow>
      </div>
    </div>
  );
}
function RerunRulesNote() {
  return (
    <div className="sw-card" style={{ marginBottom: 12, background: "var(--sw-bg-sunken)" }}>
      <div className="body" style={{ fontSize: 12.5 }}>
        Re-runs the 22 enabled rules against current data. <strong>Doesn't change artist data</strong> —
        just refreshes the violation list. Use this after editing rule severity or thresholds.
      </div>
    </div>
  );
}

function ProgressStack({ jobs }) {
  if (!jobs.length) return null;
  return (
    <div className="sw-progress-stack" role="status" aria-live="polite">
      {jobs.map(j => (
        <div key={j.id} className="sw-progress-card">
          <div className="row" style={{ gap: 8, alignItems: "center" }}>
            <span className="sw-spinner" aria-hidden="true"></span>
            <strong style={{ fontSize: 12.5 }}>{j.label}</strong>
            <span className="flex-1"></span>
            <span className="muted mono" style={{ fontSize: 11 }}>{Math.floor(j.pct)}%</span>
          </div>
          <div className="sw-progress-track">
            <div className="sw-progress-fill" style={{ width: j.pct + "%" }}></div>
          </div>
          <div className="muted" style={{ fontSize: 11 }}>
            {Math.floor((j.pct/100) * j.total)} of {j.total} processed
          </div>
        </div>
      ))}
    </div>
  );
}

function Toast({ toast, onDismiss }) {
  const { useEffect } = React;
  useEffect(() => {
    const t = setTimeout(onDismiss, 6000);
    return () => clearTimeout(t);
  }, [onDismiss]);
  return (
    <div className="sw-toast" role="status">
      <span className="sev info"><span className="dot"></span></span>
      <span style={{ fontSize: 12.5 }}>{toast.msg}</span>
      <span className="flex-1"></span>
      {toast.undo && <button className="btn ghost sm">Undo</button>}
      <button className="btn ghost sm" onClick={onDismiss} aria-label="Dismiss">
        <Icon name="x" size={11}/>
      </button>
    </div>
  );
}

Object.assign(window, {
  BulkActionBar, BulkPreviewDrawer, ProgressStack, Toast,
});
