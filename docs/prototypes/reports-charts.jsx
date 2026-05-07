/* Reports — chart strip.
 *
 * 4 charts above the matrix table, all driven by the same `artists` array
 * the table renders. Charts are SVG-only — no chart lib — kept small and
 * tight: a KPI, a donut, a stacked bar, a horizontal bar.
 *
 * Each chart card has a header with the chart title, a "by ___" dimension
 * picker (mocked dropdown — clicking it shows the chip), and a kebab for
 * remove/duplicate. Clicking a slice/bar would filter the table below
 * (mocked here — the cursor changes and an outline appears on hover).
 *
 * Adding charts: an "+ Add chart" tile sits at the end of the strip. In a
 * real build it would open a small popover; here clicking it cycles a
 * placeholder card in.
 */

const { useState: ufState, useEffect: ufEffect, useRef: ufRef } = React;

const SW = {
  blue:  "var(--sw-blue)",
  warm:  "var(--sw-warm)",
  ok:    "var(--sw-ok)",
  warn:  "var(--sw-warn)",
  err:   "var(--sw-err)",
  ink:   "var(--sw-ink)",
  ink2:  "var(--sw-ink-2)",
  ink3:  "var(--sw-ink-3)",
  ink4:  "var(--sw-ink-4)",
  line:  "var(--sw-line)",
  bg:    "var(--sw-bg-raised)",
  sunken:"var(--sw-bg-sunken)",
};

function ChartStrip({ artists, totalIDs }) {
  const [open, setOpen] = ufState(true);
  const [charts, setCharts] = ufState([
    { id: "c1", kind: "kpi",     title: "Compliance score",   by: "all" },
    { id: "c2", kind: "donut",   title: "Severity breakdown", by: "severity" },
    { id: "c3", kind: "stacked", title: "Coverage by field",  by: "field" },
    { id: "c4", kind: "hbar",    title: "Top missing fields", by: "field" },
  ]);

  const MAX_CHARTS = 6;
  function removeChart(id) { setCharts(cs => cs.filter(c => c.id !== id)); }
  function addChart() {
    setCharts(cs => {
      if (cs.length >= MAX_CHARTS) return cs;
      const types = ["donut", "stacked", "hbar", "kpi"];
      const t = types[cs.length % types.length];
      return [...cs, { id: "c" + Date.now(), kind: t, title: "New chart", by: "field" }];
    });
  }

  return (
    <div style={{ marginTop: 4, marginBottom: 12 }}>
      <div className="row" style={{ gap: 8, marginBottom: 8 }}>
        <button
          className="btn ghost sm"
          onClick={() => setOpen(o => !o)}
          style={{ height: 24, padding: "0 8px", fontSize: 12 }}
          aria-expanded={open}
        >
          <Icon name="chevron" size={10} style={{ transform: open ? "rotate(90deg)" : "rotate(0deg)", transition: "transform .15s" }}/>
          Charts <span className="muted">({charts.length})</span>
        </button>
        <span className="muted" style={{ fontSize: 11.5 }}>Hover a slice for details.</span>
        <span className="flex-1"></span>
        {open && charts.length > 0 && charts.length < MAX_CHARTS && (
          <button
            className="btn ghost sm"
            onClick={addChart}
            style={{ height: 24, padding: "0 8px", fontSize: 12 }}
          >
            <Icon name="plus" size={11}/> Add chart
          </button>
        )}
      </div>

      {open && (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))",
            gap: 10,
          }}
        >
          {charts.map(c => (
            <ChartCard key={c.id} chart={c} artists={artists} totalIDs={totalIDs} onRemove={() => removeChart(c.id)} />
          ))}
          {charts.length < MAX_CHARTS && (
            <button
              onClick={addChart}
              style={{
                background: "transparent",
                border: "1px dashed var(--sw-line-strong, var(--sw-line))",
                borderRadius: 10, color: "var(--sw-ink-3)", fontSize: 12,
                minHeight: 156, display: "grid", placeItems: "center", gap: 6,
                cursor: "pointer", padding: 12, fontFamily: "inherit",
              }}
            >
              <Icon name="plus" size={16}/>
              <span>Add chart</span>
              <span className="muted" style={{ fontSize: 10.5 }}>donut · stacked · hbar · KPI</span>
            </button>
          )}
        </div>
      )}
    </div>
  );
}

function ChartCard({ chart, artists, totalIDs, onRemove }) {
  return (
    <div
      style={{
        background: SW.bg,
        border: "1px solid " + SW.line,
        borderRadius: 10,
        padding: "10px 12px 12px",
        minHeight: 156,
        display: "flex", flexDirection: "column", gap: 8,
      }}
    >
      <div className="row" style={{ gap: 6, alignItems: "flex-start" }}>
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ fontSize: 12, fontWeight: 600, color: SW.ink, lineHeight: 1.2 }}>{chart.title}</div>
          <div className="row" style={{ gap: 4, marginTop: 3 }}>
            <span className="muted" style={{ fontSize: 10.5 }}>by</span>
            <button
              className="chip"
              style={{ height: 18, padding: "0 6px", fontSize: 10.5, lineHeight: "16px" }}
            >
              {dimensionLabel(chart.by)} <Icon name="chevron" size={8}/>
            </button>
          </div>
        </div>
        <button
          onClick={onRemove}
          aria-label="Remove chart"
          style={{
            background: "transparent", border: 0, color: SW.ink4,
            cursor: "pointer", padding: 2, borderRadius: 4, lineHeight: 1,
          }}
          onMouseEnter={(e) => e.currentTarget.style.color = SW.ink2}
          onMouseLeave={(e) => e.currentTarget.style.color = SW.ink4}
        >
          <Icon name="x" size={12}/>
        </button>
      </div>

      <div style={{ flex: 1, display: "flex", alignItems: "center", justifyContent: "center" }}>
        {chart.kind === "kpi"     && <KPIChart artists={artists} totalIDs={totalIDs} />}
        {chart.kind === "donut"   && <DonutChart artists={artists} />}
        {chart.kind === "stacked" && <StackedBarChart artists={artists} />}
        {chart.kind === "hbar"    && <HBarChart artists={artists} />}
      </div>
    </div>
  );
}

function dimensionLabel(by) {
  return ({
    all: "all artists",
    severity: "severity",
    field: "field",
    provider: "provider",
    library: "library",
    server: "server",
  })[by] || by;
}

/* ---------- KPI: overall compliance ---------- */
function KPIChart({ artists, totalIDs }) {
  // Fake stable scoring: % of (bio + thumb + fanart + nfo + ids) achieved.
  const fields = ["bio", "thumb", "fanart", "nfo"];
  const totalSlots = artists.length * (fields.length + 1); // +1 for IDs as a single slot
  let filled = 0;
  artists.forEach(a => {
    fields.forEach(f => { if (a[f]) filled += 1; });
    const haveIds = totalIDs > 0 ? Object.values(a.ids || {}).filter(Boolean).length / totalIDs : 0;
    filled += haveIds; // partial credit
  });
  const pct = totalSlots > 0 ? filled / totalSlots : 0;
  const pctNum = Math.round(pct * 100);
  const tone = pct >= 0.85 ? SW.ok : pct >= 0.6 ? SW.warn : SW.err;

  return (
    <div style={{ width: "100%", textAlign: "center" }}>
      <div style={{ fontSize: 36, fontWeight: 600, letterSpacing: "-0.02em", color: tone, lineHeight: 1, fontVariantNumeric: "tabular-nums" }}>
        {pctNum}%
      </div>
      <div className="muted" style={{ fontSize: 11, marginTop: 4 }}>
        across {artists.length} artist{artists.length === 1 ? "" : "s"}
      </div>
      <div style={{ marginTop: 10, height: 4, background: SW.sunken, borderRadius: 999, overflow: "hidden" }}>
        <div style={{ width: pctNum + "%", height: "100%", background: tone, transition: "width .3s" }}></div>
      </div>
    </div>
  );
}

/* ---------- Donut: severity breakdown of remaining gaps ---------- */
function DonutChart({ artists }) {
  // Fake severity from missing fields:
  //   missing nfo or all ids => err
  //   missing bio or thumb   => warn
  //   missing fanart only    => info
  let err = 0, warn = 0, info = 0, ok = 0;
  artists.forEach(a => {
    const missingAllIds = !Object.values(a.ids || {}).some(Boolean);
    if (!a.nfo || missingAllIds) err += 1;
    else if (!a.bio || !a.thumb) warn += 1;
    else if (!a.fanart) info += 1;
    else ok += 1;
  });
  const segs = [
    { label: "Error",   v: err,  color: SW.err  },
    { label: "Warn",    v: warn, color: SW.warn },
    { label: "Info",    v: info, color: SW.blue },
    { label: "OK",      v: ok,   color: SW.ok   },
  ].filter(s => s.v > 0);
  // Real total for display; `denom` only feeds the SVG arc math so we don't
  // divide by zero when there are no segments. The center label still renders
  // the true total (which is 0) instead of a synthetic 1.
  const total = segs.reduce((a, b) => a + b.v, 0);
  const denom = total || 1;

  // SVG donut (90 viewbox, r=32 / inner 22)
  const cx = 50, cy = 50, r = 32, ir = 22;
  let acc = 0;
  const arcs = segs.map(s => {
    const start = acc / denom;
    acc += s.v;
    const end = acc / denom;
    return { ...s, d: arcPath(cx, cy, r, ir, start, end) };
  });

  return (
    <div className="row" style={{ gap: 10, alignItems: "center", width: "100%" }}>
      <svg viewBox="0 0 100 100" width="84" height="84" aria-label="Severity donut">
        {arcs.map((a, i) => (
          <path key={i} d={a.d} fill={a.color}>
            <title>{a.label}: {a.v}</title>
          </path>
        ))}
        <text x={cx} y={cy - 1} textAnchor="middle" dominantBaseline="middle"
          style={{ fontSize: 14, fontWeight: 600, fill: "var(--sw-ink)", fontVariantNumeric: "tabular-nums" }}>
          {total}
        </text>
        <text x={cx} y={cy + 11} textAnchor="middle" dominantBaseline="middle"
          style={{ fontSize: 6, fill: "var(--sw-ink-3)", letterSpacing: "0.06em", textTransform: "uppercase" }}>
          ARTISTS
        </text>
      </svg>
      <div style={{ flex: 1, display: "grid", gap: 3, fontSize: 11 }}>
        {segs.map(s => (
          <div key={s.label} className="row" style={{ gap: 6 }}>
            <span style={{ width: 8, height: 8, borderRadius: 2, background: s.color, display: "inline-block" }}></span>
            <span style={{ flex: 1, color: SW.ink2 }}>{s.label}</span>
            <span style={{ color: SW.ink3, fontVariantNumeric: "tabular-nums" }}>{s.v}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function arcPath(cx, cy, ro, ri, t0, t1) {
  // t0, t1 in [0,1], 0 = top, clockwise
  if (t1 - t0 >= 0.999) {
    // full circle
    return [
      `M ${cx} ${cy - ro}`,
      `A ${ro} ${ro} 0 1 1 ${cx - 0.001} ${cy - ro}`,
      `M ${cx} ${cy - ri}`,
      `A ${ri} ${ri} 0 1 0 ${cx - 0.001} ${cy - ri}`,
      "Z",
    ].join(" ");
  }
  const a0 = (t0 * 2 * Math.PI) - Math.PI / 2;
  const a1 = (t1 * 2 * Math.PI) - Math.PI / 2;
  const x0o = cx + ro * Math.cos(a0), y0o = cy + ro * Math.sin(a0);
  const x1o = cx + ro * Math.cos(a1), y1o = cy + ro * Math.sin(a1);
  const x0i = cx + ri * Math.cos(a0), y0i = cy + ri * Math.sin(a0);
  const x1i = cx + ri * Math.cos(a1), y1i = cy + ri * Math.sin(a1);
  const large = (t1 - t0) > 0.5 ? 1 : 0;
  return [
    `M ${x0o} ${y0o}`,
    `A ${ro} ${ro} 0 ${large} 1 ${x1o} ${y1o}`,
    `L ${x1i} ${y1i}`,
    `A ${ri} ${ri} 0 ${large} 0 ${x0i} ${y0i}`,
    "Z",
  ].join(" ");
}

/* ---------- Stacked bar: have / miss per field ---------- */
function StackedBarChart({ artists }) {
  const fields = [
    { key: "bio",    label: "Bio" },
    { key: "thumb",  label: "Thumb" },
    { key: "fanart", label: "Fanart" },
    { key: "nfo",    label: "NFO" },
  ];
  const total = artists.length;
  const denom = total || 1;
  const rows = fields.map(f => {
    const have = artists.filter(a => a[f.key]).length;
    return { ...f, have, miss: total - have };
  });

  return (
    <div style={{ width: "100%", display: "grid", gap: 6 }}>
      {rows.map(r => {
        const havePct = (r.have / denom) * 100;
        return (
          <div key={r.key} className="row" style={{ gap: 8, fontSize: 10.5 }}>
            <span style={{ width: 42, color: SW.ink3 }}>{r.label}</span>
            <div
              style={{ flex: 1, height: 14, background: "rgba(248, 113, 113, 0.18)", borderRadius: 3, position: "relative", overflow: "hidden" }}
              title={`${r.label}: ${r.have} have, ${r.miss} missing`}
            >
              <div style={{ width: havePct + "%", height: "100%", background: SW.ok, transition: "width .3s" }}></div>
            </div>
            <span style={{ width: 28, textAlign: "right", color: SW.ink2, fontVariantNumeric: "tabular-nums" }}>{r.have}</span>
          </div>
        );
      })}
    </div>
  );
}

/* ---------- Horizontal bar: top missing fields ---------- */
function HBarChart({ artists }) {
  const fields = [
    { key: "bio",    label: "Biography" },
    { key: "thumb",  label: "Thumb image" },
    { key: "fanart", label: "Fanart" },
    { key: "nfo",    label: "NFO" },
  ];
  const total = artists.length;
  const rows = fields
    .map(f => ({ ...f, miss: artists.filter(a => !a[f.key]).length }))
    .sort((a, b) => b.miss - a.miss);
  // `max || 1` keeps the bar-width math safe when nothing is missing; the
  // displayed `total` (the `/ {total}` denominator label) always shows the
  // real artist count, including 0.
  const max = rows[0]?.miss || 1;

  return (
    <div style={{ width: "100%", display: "grid", gap: 6 }}>
      {rows.map(r => {
        const pct = (r.miss / max) * 100;
        const width = r.miss === 0 ? "0%" : Math.max(pct, 4) + "%";
        return (
          <div key={r.key} className="row" style={{ gap: 8, fontSize: 11 }}>
            <span style={{ width: 80, color: SW.ink2, fontSize: 10.5 }}>{r.label}</span>
            <div style={{ flex: 1, height: 12, background: SW.sunken, borderRadius: 3, position: "relative", overflow: "hidden" }}>
              <div style={{ width, height: "100%", background: SW.warm, transition: "width .3s" }}></div>
            </div>
            <span style={{ width: 32, textAlign: "right", color: SW.warm, fontVariantNumeric: "tabular-nums", fontWeight: 600 }}>{r.miss}</span>
            <span className="muted" style={{ fontSize: 10, width: 28, textAlign: "right" }}>/ {total}</span>
          </div>
        );
      })}
    </div>
  );
}

/* Expose for cross-script use (each <script type="text/babel"> is isolated). */
window.ChartStrip = ChartStrip;
