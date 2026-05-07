/* Artist detail — single hero page.
 *
 * Locked decisions (see docs/milestone-55/06-artist-detail.md):
 *  - Hero/reference page, not a workbench. Findings are visible on the field
 *    they affect (chip on the field), not isolated to a tab.
 *  - Single scrolling page with section anchors. Sticky mini-header shows
 *    name + finding count once you've scrolled past the hero.
 *  - Every field is editable; manual edits are badged "manual" and wear that
 *    badge until cleared.
 *  - Provider conflicts surface inline: the resolved value is shown with a
 *    small ⊘N indicator that opens a popover listing each provider's value
 *    and a "use this" action.
 *  - No releases/discography (Stillwater is artist-level).
 *  - Identity merge/split deferred — quiet kebab item only.
 *  - Activity is a 5-row strip linking to Logs filtered by artist_id.
 *
 * Tweaks:
 *  - headerDensity: "medium" | "rich"   (medium per locked answer; rich shown for compare)
 *  - conflictsMode: "inline" | "asFinding"
 *  - findingsPlacement: "embedded" | "sidebar"  (embedded section vs. right-rail sticky)
 */

const { useState: aState, useEffect: aEffect, useRef: aRef, useMemo: aMemo } = React;

const TWEAK_DEFAULTS = /*EDITMODE-BEGIN*/{
  "headerDensity":     "medium",
  "conflictsMode":     "inline",
  "findingsPlacement": "embedded"
}/*EDITMODE-END*/;

/* ---------- helpers ---------- */

function SeverityDot({ sev, size = 6 }) {
  const c = sev === "err" ? "var(--sw-err)" : sev === "warn" ? "var(--sw-warm)" : "var(--sw-blue)";
  return <span style={{ display: "inline-block", width: size, height: size, borderRadius: 999, background: c, flex: "none" }} />;
}

function ScoreCell({ score }) {
  const tone = score === 100 ? "ok" : score >= 70 ? "info" : score >= 40 ? "warn" : "err";
  const color = tone === "ok" ? "var(--sw-ok)" : tone === "warn" ? "var(--sw-warm)" : tone === "err" ? "var(--sw-err)" : "var(--sw-blue)";
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 6, fontVariantNumeric: "tabular-nums", fontSize: 13, fontWeight: 600, color }}>
      <span style={{ width: 6, height: 6, borderRadius: 999, background: color }}></span>
      {score}%
    </span>
  );
}

function CoverageBar({ fields, idsHave, idsTotal }) {
  const order = [["biography","Bio"],["primary","Pri"],["backdrop","Bd"],["logo","Lg"],["banner","Bn"],["nfo","NFO"]];
  return (
    <div className="row" style={{ gap: 6, alignItems: "center" }}>
      <div className="row" style={{ gap: 2 }}>
        {order.map(([k, lbl]) => (
          <span key={k} title={k + (fields[k] ? " — present" : " — missing")}
            style={{ width: 12, height: 12, borderRadius: 2,
              background: fields[k] ? "var(--sw-ok)" : "var(--sw-warm-soft, rgba(245,158,11,0.18))",
              border: "1px solid " + (fields[k] ? "var(--sw-ok)" : "var(--sw-warm)"),
              fontSize: 8, color: fields[k] ? "#fff" : "var(--sw-warm)",
              textAlign: "center", lineHeight: "10px", fontWeight: 700 }}>{lbl}</span>
        ))}
      </div>
      <span className="muted" style={{ fontSize: 11, fontVariantNumeric: "tabular-nums" }}>
        {idsHave}/{idsTotal} IDs
      </span>
    </div>
  );
}

function SourcePill({ source }) {
  if (!source) return null;
  const labels = {
    musicbrainz: "MB", discogs: "DG", wikidata: "WD", spotify: "SP",
    fanart: "FA", lastfm: "LFM", allmusic: "AM", nfo: "NFO", local: "Local", manual: "Manual"
  };
  return (
    <span className="sw-ad-src" title={`Source: ${source}`}>
      {labels[source] || source}
    </span>
  );
}

function FindingChip({ finding, onClick }) {
  if (!finding) return null;
  const label = finding.title || finding.rule;
  return (
    <button type="button" className={`sw-ad-finding sev-${finding.severity}`} onClick={onClick} title={finding.message}>
      <SeverityDot sev={finding.severity} />
      <span className="rule">{label}</span>
    </button>
  );
}

function ConflictBadge({ conflicts, onClick }) {
  if (!conflicts || conflicts.length === 0) return null;
  return (
    <button type="button" className="sw-ad-conflict" onClick={onClick} title="Provider values disagree">
      ⊘ {conflicts.length}
    </button>
  );
}

function ManualBadge({ manual }) {
  if (!manual) return null;
  return <span className="sw-ad-manual" title="Manually overridden">manual</span>;
}

function LockedBadge({ locked }) {
  if (!locked) return null;
  return <span className="sw-ad-locked" title="This field is locked from rule-based rewrites">locked</span>;
}

/* Generic field row: label · value · annotations */
function Field({ label, field, mono, render, onEdit, onShowConflicts, onShowFinding }) {
  if (!field) return null;
  const display = render ? render(field.value) : field.value;
  return (
    <div className="sw-ad-field">
      <div className="sw-ad-field-label">{label}</div>
      <div className="sw-ad-field-value">
        <div className={`sw-ad-field-display ${mono ? "mono" : ""}`}>{display ?? <span className="muted">—</span>}</div>
        <div className="sw-ad-field-annot">
          <SourcePill source={field.source} />
          <ManualBadge manual={field.manual} />
          <LockedBadge locked={field.locked} />
          <ConflictBadge conflicts={field.conflicts} onClick={onShowConflicts} />
          <FindingChip finding={field.finding} onClick={onShowFinding} />
          <button type="button" className="sw-ad-field-edit" onClick={onEdit} title="Edit">
            <Icon name="pencil" size={11}/>
          </button>
        </div>
      </div>
    </div>
  );
}

/* ---------- main ---------- */

function ArtistDetailProposal() {
  const a = ARTIST_DETAIL;

  const [tweaks, setTweak] = useTweaks(TWEAK_DEFAULTS);

  // Sticky mini-header — appears after hero scrolls out.
  const heroRef = aRef(null);
  const [stuck, setStuck] = aState(false);
  aEffect(() => {
    const onScroll = () => {
      if (!heroRef.current) return;
      const r = heroRef.current.getBoundingClientRect();
      setStuck(r.bottom < 56);
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    onScroll();
    return () => window.removeEventListener("scroll", onScroll);
  }, []);

  const [conflictPopover, setConflictPopover] = aState(null); // {field, conflicts, value}
  const [activeFindingId, setActiveFindingId] = aState(null);
  const [lightbox, setLightbox] = aState(null); // {kind, itemId}
  const [uploadModal, setUploadModal] = aState(null); // {mode:"add"|"replace", kind, replacing}
  // Local copy of artwork so promote/delete/upload can mutate without re-fetching.
  const [artwork, setArtwork] = aState(a.artwork);
  const setPrimary = (kind, itemId) => {
    setArtwork(prev => prev.map(k => k.kind !== kind ? k : {
      ...k, items: k.items.map(it => ({ ...it, primary: it.id === itemId }))
    }));
  };
  const deleteItem = (kind, itemId) => {
    setArtwork(prev => prev.map(k => {
      if (k.kind !== kind) return k;
      const items = k.items.filter(it => it.id !== itemId);
      // If we removed the primary, promote the first remaining (if any).
      if (items.length && !items.some(it => it.primary)) items[0] = { ...items[0], primary: true };
      return { ...k, items };
    }));
  };
  // addItem inserts a new artwork item alongside existing ones. Used by Crop/Trim
  // (and would be used by upload). Returns the new id so the lightbox can re-target.
  const addItem = (kind, partial) => {
    const id = `${kind}-${Date.now().toString(36)}`;
    setArtwork(prev => prev.map(k => {
      if (k.kind !== kind) return k;
      const item = {
        id,
        url: partial.url,
        width: partial.width,
        height: partial.height,
        source: partial.source || "manual",
        primary: false,
        ...partial,
      };
      return { ...k, items: [...k.items, item] };
    }));
    return id;
  };

  const findingsCount = a.findings.length;
  const findingsBySev = a.findings.reduce((acc, f) => { acc[f.severity] = (acc[f.severity] || 0) + 1; return acc; }, {});

  const placeFindingsInSidebar = tweaks.findingsPlacement === "sidebar";

  return (
    <div className="sw-anno sw-ad-root" style={{ position: "relative" }}>
      <Sidebar active="artists" actionsCount={47} />
      <div className="sw-main sw-ad-main">

        {/* Sticky mini-header */}
        <div className={`sw-ad-stick ${stuck ? "show" : ""}`} aria-hidden={!stuck}>
          <div className="sw-ad-stick-inner">
            <a className="sw-ad-back" href="artists.html"><Icon name="arrow-left" size={12}/> Artists</a>
            <span className="sw-ad-stick-name">{a.name.value}</span>
            <span className="muted" style={{ fontSize: 12 }}>{a.type.value}</span>
            <span className="sw-ad-stick-findings" title={`${findingsCount} open findings`}>
              {findingsBySev.err  > 0 && <><SeverityDot sev="err"/> <span>{findingsBySev.err}</span></>}
              {findingsBySev.warn > 0 && <><SeverityDot sev="warn"/> <span>{findingsBySev.warn}</span></>}
              {findingsBySev.info > 0 && <><SeverityDot sev="info"/> <span>{findingsBySev.info}</span></>}
            </span>
            <div style={{ marginLeft: "auto" }} className="row" >
              <button type="button" className="btn ghost sm">Re-run rules</button>
              <button type="button" className="btn primary sm">Edit</button>
            </div>
          </div>
        </div>

        {/* Breadcrumb */}
        <div className="sw-ad-crumb">
          <a href="artists.html">Artists</a>
          <span className="sep">/</span>
          <span className="muted">{a.library.value}</span>
          <span className="sep">/</span>
          <span>{a.name.value}</span>
        </div>

        {/* Hero */}
        <section ref={heroRef} className={`sw-ad-hero density-${tweaks.headerDensity}`}>
          <div className="sw-ad-hero-image">
            <div className="sw-ad-image-slot" title="No image — fanart.tv fetch failed">
              <Icon name="image" size={28}/>
              <div style={{ fontSize: 11, marginTop: 6 }} className="muted">No artwork</div>
            </div>
          </div>

          <div className="sw-ad-hero-meta">
            <div className="sw-ad-hero-toprow">
              <span className="sw-ad-type-pill">{a.type.value}</span>
              <span className="sw-ad-library-pill"><Icon name="folder" size={10}/> {a.library.value}</span>
              <span className="muted" style={{ fontSize: 11.5 }}>· last scan {a.lastScan}</span>
            </div>

            <h1 className="sw-ad-name">
              {a.name.value}
              {a.name.finding && (
                <button type="button" className={`sw-ad-name-flag sev-${a.name.finding.severity}`}
                        onClick={() => setActiveFindingId("f1")}
                        title={a.name.finding.message}>
                  <SeverityDot sev={a.name.finding.severity}/> name conflict
                </button>
              )}
            </h1>

            {a.disambig.value && (
              <div className="sw-ad-disambig">{a.disambig.value}</div>
            )}

            {tweaks.headerDensity === "rich" && a.aliases.value.length > 0 && (
              <div className="sw-ad-aliases">
                <span className="muted" style={{ fontSize: 11.5 }}>also known as</span>
                {a.aliases.value.map(al => <span key={al} className="sw-ad-alias">{al}</span>)}
              </div>
            )}

            {/* Compliance summary */}
            <div className="sw-ad-summary">
              <div className="sw-ad-summary-score">
                <ScoreCell score={a.compliance.score} />
                <span className="muted" style={{ fontSize: 11.5 }}>compliance</span>
              </div>
              <div className="sw-ad-summary-divider" />
              <div className="sw-ad-summary-findings">
                <a href="#findings" className="sw-ad-summary-link">
                  {findingsBySev.err  > 0 && <span className="row" style={{ gap: 4 }}><SeverityDot sev="err"/>  <b>{findingsBySev.err}</b> error{findingsBySev.err  === 1 ? "" : "s"}</span>}
                  {findingsBySev.warn > 0 && <span className="row" style={{ gap: 4 }}><SeverityDot sev="warn"/> <b>{findingsBySev.warn}</b> warning{findingsBySev.warn === 1 ? "" : "s"}</span>}
                  {findingsBySev.info > 0 && <span className="row" style={{ gap: 4 }}><SeverityDot sev="info"/> <b>{findingsBySev.info}</b> info</span>}
                  {findingsCount === 0 && <span className="muted">No open findings</span>}
                </a>
              </div>
              <div className="sw-ad-summary-divider" />
              <div className="sw-ad-summary-coverage">
                <CoverageBar fields={a.compliance.fields} idsHave={a.compliance.idsHave} idsTotal={a.compliance.idsTotal} />
              </div>
            </div>
          </div>

          <div className="sw-ad-hero-actions">
            <button type="button" className="btn primary"><Icon name="bolt" size={12}/> Re-run rules</button>
            <button type="button" className="btn ghost"><Icon name="download" size={12}/> Re-fetch metadata</button>
            <button type="button" className="btn ghost"><Icon name="pencil" size={12}/> Edit</button>
            <details className="sw-ad-kebab">
              <summary>···</summary>
              <div className="sw-ad-kebab-menu">
                <button type="button">Lock all fields</button>
                <button type="button">Export as NFO</button>
                <button type="button">Open in MusicBrainz ↗</button>
                <div className="sep"/>
                <button type="button" className="muted">Report identity issue…</button>
                <button type="button" className="danger">Remove from library…</button>
              </div>
            </details>
          </div>
        </section>

        {/* Two-column body */}
        <div className={`sw-ad-body ${placeFindingsInSidebar ? "with-rail" : ""}`}>
          <div className="sw-ad-content">

            {/* Identifiers */}
            <SectionCard id="identifiers" title="Identifiers" meta={`${a.identifiers.filter(i => i.value).length} of ${a.identifiers.length} linked`}>
              <div className="sw-ad-ids">
                {a.identifiers.map(id => (
                  <div key={id.kind} className={`sw-ad-id ${!id.value ? "missing" : ""}`}>
                    <div className="sw-ad-id-kind">{id.kind}</div>
                    <div className="sw-ad-id-val mono">
                      {id.value || <span className="muted">— not linked —</span>}
                    </div>
                    <div className="sw-ad-id-annot">
                      <SourcePill source={id.source} />
                      {id.finding && <FindingChip finding={id.finding} onClick={() => setActiveFindingId("f3")} />}
                      {id.value && <button type="button" className="sw-ad-field-edit" title="Copy"><Icon name="copy" size={11}/></button>}
                    </div>
                  </div>
                ))}
              </div>
              <div className="sw-ad-ids-actions">
                <button type="button" className="btn ghost sm"><Icon name="search" size={11}/> Match identifiers</button>
                <button type="button" className="btn ghost sm"><Icon name="plus" size={11}/> Add manually</button>
              </div>
            </SectionCard>

            {/* Artwork */}
            <SectionCard id="artwork" title="Artwork" meta={`${artwork.filter(k => k.status === "ok").length} of ${artwork.length} kinds resolved`}>
              <div className="sw-ad-artwork">
                {artwork.map(k => (
                  <ArtworkKindRow
                    key={k.kind}
                    kindGroup={k}
                    onOpen={(item) => setLightbox({ kind: k.kind, itemId: item.id })}
                    onAdd={() => setUploadModal({ mode: "add", kind: k.kind, replacing: null })}
                    onSetPrimary={(itemId) => setPrimary(k.kind, itemId)}
                    onShowFinding={() => {
                      if (k.finding) {
                        const match = a.findings.find(f => f.rule === k.finding.rule);
                        if (match) setActiveFindingId(match.id);
                      }
                    }}
                  />
                ))}
              </div>
              <div className="sw-ad-ids-actions">
                <button type="button" className="btn ghost sm"><Icon name="refresh" size={11}/> Re-fetch artwork</button>
                <button type="button" className="btn ghost sm" onClick={() => setUploadModal({ mode: "add", kind: null, replacing: null })}><Icon name="upload" size={11}/> Upload manually</button>
              </div>
            </SectionCard>

            {/* Core metadata */}
            <SectionCard id="metadata" title="Metadata">
              <div className="sw-ad-fields">
                <Field label="Name"          field={a.name}     onShowFinding={() => setActiveFindingId("f1")} onShowConflicts={() => setConflictPopover({ field: "Name", conflicts: a.name.conflicts, value: a.name.value })}/>
                <Field label="Type"          field={a.type}/>
                <Field label="Aliases"       field={a.aliases}  render={vs => vs.join(" · ")}/>
                <Field label="Formed"        field={a.formed}/>
                <Field label="Origin"        field={a.origin}   onShowConflicts={() => setConflictPopover({ field: "Origin", conflicts: a.origin.conflicts, value: a.origin.value })}/>
                <Field label="Members"       field={a.members}  render={vs => vs.join(", ")}/>
                <Field label="Genres"        field={a.genres}   render={vs => vs.join(", ")} onShowConflicts={() => setConflictPopover({ field: "Genres", conflicts: a.genres.conflicts, value: a.genres.value })}/>
                <Field label="Disambiguation" field={a.disambig}/>
              </div>

              <div className="sw-ad-bio">
                <div className="sw-ad-bio-head">
                  <div className="sw-ad-field-label">Biography</div>
                  <div className="sw-ad-field-annot">
                    <SourcePill source={a.biography.source} />
                    <LockedBadge locked={a.biography.locked} />
                    <FindingChip finding={a.biography.finding} onClick={() => setActiveFindingId("f2")}/>
                    <button type="button" className="sw-ad-field-edit" title="Edit"><Icon name="pencil" size={11}/></button>
                  </div>
                </div>
                <p className="sw-ad-bio-body">{a.biography.value}</p>
              </div>
            </SectionCard>

            {/* Provider links */}
            <SectionCard id="providers" title="Provider links" meta={`${a.providers.filter(p => p.status === "linked").length} linked · ${a.providers.filter(p => p.status === "error").length} error · ${a.providers.filter(p => p.status === "unlinked").length} unlinked`}>
              <div className="sw-ad-providers">
                {a.providers.map(p => (
                  <div key={p.id} className={`sw-ad-provider status-${p.status}`}>
                    <div className="sw-ad-provider-name">{p.name}</div>
                    <div className="sw-ad-provider-status">
                      <span className="dot"/> {p.status}
                      {p.lastFetched && <span className="muted" style={{ fontSize: 11 }}> · {p.lastFetched}</span>}
                    </div>
                    {p.error && <div className="sw-ad-provider-error">{p.error}</div>}
                    <div className="sw-ad-provider-actions">
                      {p.status === "linked"   && <a href={p.url} target="_blank" rel="noreferrer" className="btn ghost sm">Open ↗</a>}
                      {p.status === "linked"   && <button type="button" className="btn ghost sm">Refresh</button>}
                      {p.status === "error"    && <button type="button" className="btn ghost sm">Retry</button>}
                      {p.status === "unlinked" && <button type="button" className="btn ghost sm">Link</button>}
                    </div>
                  </div>
                ))}
              </div>
            </SectionCard>

            {/* Findings (when embedded) */}
            {!placeFindingsInSidebar && (
              <SectionCard id="findings" title="Open findings" meta={`${findingsCount} open`}>
                <FindingsList findings={a.findings} activeId={activeFindingId} onSelect={setActiveFindingId}/>
              </SectionCard>
            )}

            {/* Activity */}
            <SectionCard id="activity" title="Recent activity" meta={<a href="logs.html" className="sw-ad-section-link">Open in Logs ↗</a>}>
              <div className="sw-ad-activity">
                {a.activity.map((ev, i) => (
                  <div key={i} className={`sw-ad-act kind-${ev.kind}`}>
                    <div className="sw-ad-act-time muted mono">{ev.time}</div>
                    <div className="sw-ad-act-kind">{ev.kind}</div>
                    <div className="sw-ad-act-msg">{ev.message}</div>
                    <div className="sw-ad-act-who muted">{ev.who}</div>
                  </div>
                ))}
              </div>
            </SectionCard>

          </div>

          {/* Optional sidebar (Tweak: findingsPlacement = sidebar) */}
          {placeFindingsInSidebar && (
            <aside className="sw-ad-rail">
              <div className="sw-ad-rail-stick">
                <div className="sw-card">
                  <div className="head">
                    <h2>Open findings</h2>
                    <span className="meta">{findingsCount}</span>
                  </div>
                  <div className="body" style={{ padding: 0 }}>
                    <FindingsList findings={a.findings} activeId={activeFindingId} onSelect={setActiveFindingId} compact/>
                  </div>
                </div>
              </div>
            </aside>
          )}
        </div>

        <div className="sw-ad-footnote muted">
          {a.createdAt} · last metadata change {a.lastChange} · MBID <span className="mono">{a.identifiers[0].value}</span>
        </div>
      </div>

      {/* Conflict popover */}
      {conflictPopover && (
        <ConflictPopover
          spec={conflictPopover}
          mode={tweaks.conflictsMode}
          onClose={() => setConflictPopover(null)}
        />
      )}

      {/* Artwork lightbox */}
      {lightbox && (
        <ArtworkLightbox
          artwork={artwork}
          state={lightbox}
          onNavigate={(next) => setLightbox(next)}
          onClose={() => setLightbox(null)}
          onSetPrimary={(kind, itemId) => setPrimary(kind, itemId)}
          onReplace={(kind, item) => setUploadModal({ mode: "replace", kind, replacing: item })}
          onDelete={(kind, itemId) => { deleteItem(kind, itemId); setLightbox(null); }}
          onCrop={(kind, partial) => {
            const newId = addItem(kind, { ...partial, source: "crop" });
            setLightbox({ kind, itemId: newId });
          }}
          onTrim={(kind, partial) => {
            const newId = addItem(kind, { ...partial, source: "trim" });
            setLightbox({ kind, itemId: newId });
          }}
        />
      )}

      {/* Upload / replace modal */}
      {uploadModal && (
        <ArtworkUploadModal
          state={uploadModal}
          artwork={artwork}
          onClose={() => setUploadModal(null)}
        />
      )}

      <TweaksPanel title="Tweaks">
        <TweakSection label="Header"/>
        <TweakRadio label="Density" value={tweaks.headerDensity}
          options={["medium", "rich"]}
          onChange={v => setTweak("headerDensity", v)}/>
        <TweakSection label="Conflicts"/>
        <TweakRadio label="Surface" value={tweaks.conflictsMode}
          options={["inline", "asFinding"]}
          onChange={v => setTweak("conflictsMode", v)}/>
        <TweakSection label="Findings"/>
        <TweakRadio label="Placement" value={tweaks.findingsPlacement}
          options={["embedded", "sidebar"]}
          onChange={v => setTweak("findingsPlacement", v)}/>
      </TweaksPanel>
    </div>
  );
}

/* ---------- subcomponents ---------- */

function SectionCard({ id, title, meta, children }) {
  return (
    <section id={id} className="sw-card sw-ad-section">
      <div className="head">
        <h2>{title}</h2>
        {meta && <span className="meta">{meta}</span>}
      </div>
      <div className="body">{children}</div>
    </section>
  );
}

function FindingsList({ findings, activeId, onSelect, compact }) {
  return (
    <div className={`sw-ad-findings ${compact ? "compact" : ""}`}>
      {findings.map(f => (
        <div key={f.id} className={`sw-ad-finding-row sev-${f.severity} ${activeId === f.id ? "active" : ""}`}
             onClick={() => onSelect(f.id === activeId ? null : f.id)}>
          <div className="sw-ad-finding-head">
            <SeverityDot sev={f.severity} size={7}/>
            <span className="rule">{f.title || f.rule}</span>
            <span className="muted" style={{ fontSize: 11.5 }}>· {f.field}</span>
            {f.title && <span className="sw-ad-finding-ruleid mono" title="Rule ID">{f.rule}</span>}
          </div>
          <div className="sw-ad-finding-msg">{f.message}</div>
          {(activeId === f.id || compact) && (
            <>
              <div className="sw-ad-finding-fix">
                <span className="label">Suggested fix:</span> {f.suggestedFix}
              </div>
              {f.evidence && (
                <div className="sw-ad-finding-evidence mono muted">{f.evidence}</div>
              )}
              {!compact && (
                <div className="sw-ad-finding-actions" onClick={e => e.stopPropagation()}>
                  <button type="button" className="btn primary sm">Apply fix</button>
                  <button type="button" className="btn ghost sm">Lock field</button>
                  <button type="button" className="btn ghost sm">Snooze</button>
                  <button type="button" className="btn ghost sm">Mark won't fix</button>
                </div>
              )}
            </>
          )}
        </div>
      ))}
    </div>
  );
}

function ArtworkKindRow({ kindGroup, onOpen, onAdd, onSetPrimary, onShowFinding }) {
  const { kind, label, role, status, items, finding, aliases } = kindGroup;
  const isBackdrop = kind === "backdrop"; // only kind that grows large; reflows as wrapping grid
  const empty = items.length === 0;
  return (
    <div className={`sw-ad-aw-row status-${status} kind-${kind}`}>
      <div className="sw-ad-aw-rowhead">
        <div className="sw-ad-aw-rowhead-left">
          <div className="sw-ad-aw-label">
            {label}
            {items.length > 1 && <span className="sw-ad-aw-count muted">{items.length}</span>}
          </div>
          <div className="sw-ad-aw-role muted">{role}</div>
        </div>
        <div className="sw-ad-aw-rowhead-right">
          {finding && (
            <button type="button" className={`sw-ad-aw-flag sev-${finding.severity}`}
                    title={finding.message} onClick={onShowFinding}>
              <SeverityDot sev={finding.severity}/> {finding.title}
            </button>
          )}
          <button type="button" className="btn ghost sm" onClick={onAdd}>
            <Icon name="upload" size={11}/> Add {label.toLowerCase()}
          </button>
        </div>
      </div>
      <div className={`sw-ad-aw-tiles ${isBackdrop ? "wrap" : "strip"}`}>
        {empty ? (
          <button type="button" className="sw-ad-aw-empty" onClick={onAdd}>
            <Icon name={status === "error" ? "warn" : "image"} size={22}/>
            <div className="sw-ad-aw-empty-label">
              {status === "error" ? "Fetch failed — upload manually" : "Not found — upload manually"}
            </div>
          </button>
        ) : (
          items.map(it => (
            <ArtworkTile key={it.id} item={it} kind={kind}
              onOpen={() => onOpen(it)}
              onSetPrimary={() => onSetPrimary(it.id)}/>
          ))
        )}
      </div>
    </div>
  );
}

function ArtworkTile({ item, kind, onOpen, onSetPrimary }) {
  const { url, width, height, source, primary } = item;
  const dim = width && height ? `${width}×${height}` : null;
  return (
    <div className={`sw-ad-aw-tile kind-${kind} ${primary ? "is-primary" : ""}`}>
      <button type="button" className="sw-ad-aw-tile-img" data-kind={kind} onClick={onOpen}>
        <img src={url} alt="" onError={e => { e.target.style.display = "none"; }}/>
      </button>
      <button
        type="button"
        className="sw-ad-aw-star"
        onClick={(e) => { e.stopPropagation(); if (!primary) onSetPrimary(); }}
        aria-label={primary ? "Primary" : "Set as primary"}
        title={primary ? "Primary" : "Set as primary"}>
        <svg viewBox="0 0 16 16" width="13" height="13" fill={primary ? "currentColor" : "none"} stroke="currentColor" strokeWidth="1.4">
          <path d="M8 1.5l2.06 4.18 4.6.67-3.33 3.25.79 4.6L8 12l-4.12 2.18.79-4.6L1.34 6.35l4.6-.67z" strokeLinejoin="round"/>
        </svg>
      </button>
      <div className="sw-ad-aw-tile-meta">
        {source && <SourcePill source={source}/>}
        {dim && <span className="muted mono">{dim}</span>}
      </div>
    </div>
  );
}

function ArtworkLightbox({ artwork, state, onNavigate, onClose, onSetPrimary, onReplace, onDelete, onCrop, onTrim }) {
  // Build a flat ordered list of all items across kinds, in artwork order.
  // Navigation goes within-kind first, then across kinds.
  const flat = aMemo(() => {
    const out = [];
    artwork.forEach(k => k.items.forEach(it => out.push({ kind: k.kind, kindLabel: k.label, item: it })));
    return out;
  }, [artwork]);
  const idx = flat.findIndex(e => e.kind === state.kind && e.item.id === state.itemId);
  const cur = flat[idx];

  // mode: "view" | "crop" | "trim"
  const [mode, setMode] = aState("view");
  // Reset to view whenever we navigate to a different item.
  aEffect(() => { setMode("view"); }, [state.kind, state.itemId]);

  aEffect(() => {
    const onKey = (e) => {
      if (e.key === "Escape") {
        if (mode !== "view") setMode("view");
        else onClose();
      }
      if (mode !== "view") return; // disable nav while editing
      if (e.key === "ArrowRight" && idx >= 0 && idx < flat.length - 1) {
        const n = flat[idx + 1]; onNavigate({ kind: n.kind, itemId: n.item.id });
      }
      if (e.key === "ArrowLeft" && idx > 0) {
        const n = flat[idx - 1]; onNavigate({ kind: n.kind, itemId: n.item.id });
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [idx, flat, mode]);

  if (!cur) return null;
  const { kind, kindLabel, item } = cur;
  const dim = item.width && item.height ? `${item.width}×${item.height}` : "—";
  const sizeKb = item.width && item.height ? Math.round(item.width * item.height / 8000) : null; // fake
  const isLogo = kind === "logo"; // checker bg for transparency

  // Aspect lock per kind (Crop default).
  const kindAspect = kind === "primary" ? "1:1"
    : kind === "backdrop" ? "16:9"
    : kind === "banner"   ? "16:3"
    : "free";

  return (
    <div className="sw-ad-lb-backdrop" onClick={onClose}>
      <div className="sw-ad-lb" onClick={e => e.stopPropagation()}>
        <div className="sw-ad-lb-head">
          <div>
            <div className="sw-ad-lb-eyebrow muted">
              {mode === "crop" ? `Crop · ${kindLabel}` : mode === "trim" ? `Trim · ${kindLabel}` : `${kindLabel} · ${idx + 1} of ${flat.length}`}
            </div>
            <div className="sw-ad-lb-title mono">{item.url.split("/").pop()}</div>
          </div>
          <div className="row" style={{ gap: 12 }}>
            <span className="muted mono" style={{ fontSize: 12 }}>{dim}</span>
            {sizeKb && <span className="muted mono" style={{ fontSize: 12 }}>~{sizeKb} KB</span>}
            <button type="button" className="sw-ad-lb-close" onClick={onClose} aria-label="Close">✕</button>
          </div>
        </div>
        <div className={`sw-ad-lb-stage ${isLogo ? "checker" : ""}`}>
          {mode === "view" && <>
            <button type="button" className="sw-ad-lb-nav prev" disabled={idx <= 0}
                    onClick={() => { const n = flat[idx - 1]; onNavigate({ kind: n.kind, itemId: n.item.id }); }}
                    aria-label="Previous">‹</button>
            <img className="sw-ad-lb-img" src={item.url} alt=""/>
            <button type="button" className="sw-ad-lb-nav next" disabled={idx >= flat.length - 1}
                    onClick={() => { const n = flat[idx + 1]; onNavigate({ kind: n.kind, itemId: n.item.id }); }}
                    aria-label="Next">›</button>
          </>}
          {mode === "crop" && <CropOverlay item={item} kindAspect={kindAspect}/>}
          {mode === "trim" && <TrimOverlay item={item}/>}
        </div>
        <div className="sw-ad-lb-foot">
          {mode === "view" ? <>
            <div className="row" style={{ gap: 8 }}>
              {item.primary ? (
                <span className="sw-ad-lb-primary-badge">★ Primary</span>
              ) : (
                <button type="button" className="btn ghost sm" onClick={() => onSetPrimary(kind, item.id)}>
                  <span style={{ marginRight: 4 }}>★</span> Set as primary
                </button>
              )}
              <SourcePill source={item.source}/>
            </div>
            <div className="row" style={{ gap: 8 }}>
              <button type="button" className="btn ghost sm" onClick={() => setMode("crop")}>
                <Icon name="crop" size={11}/> Crop
              </button>
              {kind === "logo" && (
                <button type="button" className="btn ghost sm" onClick={() => setMode("trim")}>
                  <Icon name="scissors" size={11}/> Trim
                </button>
              )}
              <button type="button" className="btn ghost sm" onClick={() => onReplace(kind, item)}>
                <Icon name="refresh" size={11}/> Replace
              </button>
              <button type="button" className="btn ghost sm">
                <Icon name="download" size={11}/> Download
              </button>
              <button type="button" className="btn ghost sm danger" onClick={() => onDelete(kind, item.id)}>
                <Icon name="trash" size={11}/> Delete
              </button>
            </div>
          </> : <>
            <div className="muted" style={{ fontSize: 12 }}>
              {mode === "crop"
                ? "Apply creates a new artwork item — original is kept."
                : "Trim removes transparent / solid borders. New artwork item — original is kept."}
            </div>
            <div className="row" style={{ gap: 8 }}>
              <button type="button" className="btn ghost sm" onClick={() => setMode("view")}>Cancel</button>
              <button type="button" className="btn primary sm" onClick={() => {
                // Stub: in real impl this would upload the cropped/trimmed image and
                // get a new URL. For the prototype we reuse the original URL.
                if (mode === "crop") {
                  onCrop(kind, { url: item.url, width: item.width, height: item.height });
                } else {
                  onTrim(kind, { url: item.url, width: item.width, height: item.height });
                }
              }}>
                Apply
              </button>
            </div>
          </>}
        </div>
      </div>
    </div>
  );
}

/* ─────────────────────────────────────────────────────────────────
 *  Crop overlay
 * ─────────────────────────────────────────────────────────────────
 *  Visual prototype only — no actual pixel cropping. Shows:
 *   - the source image with a draggable crop rect overlay (rect itself
 *     is fixed in the prototype but visualized convincingly)
 *   - aspect-lock chips (per-kind default + Free)
 *   - dimmed area outside the crop rect, grid lines (rule of thirds) inside
 *   - hint about target dim (rec'd output for the kind)
 */
function CropOverlay({ item, kindAspect }) {
  const [aspect, setAspect] = aState(kindAspect);
  // Mock crop rect as percentages — gives the right visual without real interaction.
  const inset = aspect === "free" ? { l: 8, r: 8, t: 8, b: 8 }
    : aspect === "1:1"  ? { l: 14, r: 14, t: 6,  b: 6  }
    : aspect === "16:9" ? { l: 6,  r: 6,  t: 12, b: 12 }
    : /* 16:3 banner */    { l: 4,  r: 4,  t: 24, b: 24 };
  return (
    <div className="sw-ad-crop">
      <img className="sw-ad-crop-img" src={item.url} alt=""/>
      {/* Dim mask: 4 panels around the crop rect */}
      <div className="sw-ad-crop-dim top"    style={{ height: `${inset.t}%` }}/>
      <div className="sw-ad-crop-dim bottom" style={{ height: `${inset.b}%` }}/>
      <div className="sw-ad-crop-dim left"   style={{ top: `${inset.t}%`, bottom: `${inset.b}%`, width: `${inset.l}%` }}/>
      <div className="sw-ad-crop-dim right"  style={{ top: `${inset.t}%`, bottom: `${inset.b}%`, width: `${inset.r}%` }}/>
      {/* Crop rect with grid + handles */}
      <div className="sw-ad-crop-rect" style={{
        top:    `${inset.t}%`,
        left:   `${inset.l}%`,
        right:  `${inset.r}%`,
        bottom: `${inset.b}%`,
      }}>
        <div className="sw-ad-crop-grid"/>
        <span className="sw-ad-crop-handle tl"/>
        <span className="sw-ad-crop-handle tr"/>
        <span className="sw-ad-crop-handle bl"/>
        <span className="sw-ad-crop-handle br"/>
        <span className="sw-ad-crop-handle t"/>
        <span className="sw-ad-crop-handle b"/>
        <span className="sw-ad-crop-handle l"/>
        <span className="sw-ad-crop-handle r"/>
      </div>
      {/* Aspect chips, top-left of stage */}
      <div className="sw-ad-crop-chips">
        {[kindAspect, "free"].filter((v, i, a) => a.indexOf(v) === i).map(a => (
          <button key={a} type="button"
                  className={`sw-ad-crop-chip ${aspect === a ? "on" : ""}`}
                  onClick={() => setAspect(a)}>
            {a === "free" ? "Free" : a}
          </button>
        ))}
      </div>
    </div>
  );
}

/* ─────────────────────────────────────────────────────────────────
 *  Trim overlay (logo only)
 * ─────────────────────────────────────────────────────────────────
 *  Removes transparent or solid borders. Prototype shows:
 *   - threshold slider (alpha cutoff, 0–32)
 *   - before/after side-by-side preview, with a "trimmed" border drawn
 *     around the simulated trim area
 *   - hint about how many px would be trimmed each side
 */
function TrimOverlay({ item }) {
  const [threshold, setThreshold] = aState(8);
  // Fake "trimmed" inset based on threshold; real impl scans alpha.
  const trimPct = Math.min(20, threshold * 1.2);
  return (
    <div className="sw-ad-trim">
      <div className="sw-ad-trim-cols">
        <div className="sw-ad-trim-col">
          <div className="sw-ad-trim-label">Before</div>
          <div className="sw-ad-trim-frame checker">
            <img src={item.url} alt=""/>
          </div>
          <div className="muted mono" style={{ fontSize: 11 }}>{item.width}×{item.height}</div>
        </div>
        <div className="sw-ad-trim-col">
          <div className="sw-ad-trim-label">After</div>
          <div className="sw-ad-trim-frame checker">
            <div className="sw-ad-trim-trimmed" style={{ inset: `${trimPct}%` }}>
              <img src={item.url} alt=""/>
            </div>
          </div>
          <div className="muted mono" style={{ fontSize: 11 }}>
            {Math.round(item.width  * (1 - trimPct / 50))}×
            {Math.round(item.height * (1 - trimPct / 50))}
            {" "}<span className="muted">(−{Math.round(trimPct)}% each side)</span>
          </div>
        </div>
      </div>
      <div className="sw-ad-trim-controls">
        <label className="sw-ad-trim-slider">
          <span>Edge threshold</span>
          <input type="range" min="0" max="32" step="1" value={threshold}
                 onChange={e => setThreshold(parseInt(e.target.value, 10))}/>
          <span className="mono" style={{ width: 24, textAlign: "right" }}>{threshold}</span>
        </label>
        <div className="muted" style={{ fontSize: 11 }}>
          0 = trim only fully transparent · higher values trim near-transparent and solid borders
        </div>
      </div>
    </div>
  );
}

function ArtworkUploadModal({ state, artwork, onClose }) {
  // mode: "add" | "replace"
  // For add: kind may be null (chip selector active) or preselected.
  // For replace: kind is locked to the kind of `replacing`.
  const [kind, setKind] = aState(state.kind);
  const [files, setFiles] = aState([]); // { name, sizeKb, dim?: "WxH" }
  const [url, setUrl] = aState("");
  aEffect(() => { setKind(state.kind); }, [state.kind]);
  const isReplace = state.mode === "replace";
  const onPick = (e) => {
    const list = Array.from(e.target.files || []);
    setFiles(list.map(f => ({ name: f.name, sizeKb: Math.round(f.size / 1024) })));
  };
  const KIND_HINTS = {
    primary:  { plex: "Poster",   kodi: "thumb",     embyjf: "Primary",  rec: "Square (1000×1000+) PNG/JPG" },
    backdrop: { plex: "Art",      kodi: "fanart",    embyjf: "Backdrop", rec: "Wide 16:9 (1920×1080+)" },
    logo:     { plex: "—",        kodi: "clearlogo", embyjf: "Logo",     rec: "Wide PNG with transparency (1000×400+, ~2.5:1)" },
    banner:   { plex: "—",        kodi: "banner",    embyjf: "Banner",   rec: "Wide thin (1000×185)" },
  };
  const hint = kind ? KIND_HINTS[kind] : null;
  const replacing = state.replacing;
  const kindLabel = kind ? (artwork.find(k => k.kind === kind)?.label || kind) : null;
  return (
    <div className="sw-ad-lb-backdrop" onClick={onClose}>
      <div className="sw-ad-up" onClick={e => e.stopPropagation()}>
        <div className="sw-ad-lb-head">
          <div>
            <div className="sw-ad-lb-eyebrow muted">{isReplace ? "Replace artwork" : "Upload artwork"}</div>
            <div className="sw-ad-lb-title">
              {isReplace
                ? <>Replace {kindLabel?.toLowerCase()} <span className="muted mono" style={{ fontSize: 12, fontWeight: 400 }}>{replacing?.url.split("/").pop()}</span></>
                : kindLabel ? `Add ${kindLabel.toLowerCase()}` : "Add artwork"}
            </div>
          </div>
          <button type="button" className="sw-ad-lb-close" onClick={onClose} aria-label="Close">✕</button>
        </div>
        <div className="sw-ad-up-body">
          {!isReplace && !state.kind && (
            <div className="sw-ad-up-section">
              <div className="sw-ad-up-section-label">Kind</div>
              <div className="sw-ad-up-kinds">
                {artwork.map(k => (
                  <button key={k.kind} type="button"
                          className={`sw-ad-up-kindchip ${kind === k.kind ? "on" : ""}`}
                          onClick={() => setKind(k.kind)}>
                    {k.label}
                  </button>
                ))}
              </div>
            </div>
          )}
          {hint && (
            <div className="sw-ad-up-hint">
              <div className="sw-ad-up-hint-rec">{hint.rec}</div>
              <div className="sw-ad-up-hint-aliases muted">
                Plex: <span className="mono">{hint.plex}</span>
                <span className="dot">·</span>
                Kodi: <span className="mono">{hint.kodi}</span>
                <span className="dot">·</span>
                Emby/Jellyfin: <span className="mono">{hint.embyjf}</span>
              </div>
            </div>
          )}
          {isReplace && replacing && (
            <div className="sw-ad-up-replace-preview">
              <div className="sw-ad-up-rp-col">
                <div className="muted" style={{ fontSize: 11, marginBottom: 6 }}>Current</div>
                <img src={replacing.url} alt=""/>
                <div className="muted mono" style={{ fontSize: 11, marginTop: 6 }}>{replacing.width}×{replacing.height}</div>
              </div>
              <div className="sw-ad-up-rp-arrow">→</div>
              <div className="sw-ad-up-rp-col">
                <div className="muted" style={{ fontSize: 11, marginBottom: 6 }}>New</div>
                <div className="sw-ad-up-rp-empty">
                  {files[0] ? <span className="mono">{files[0].name}</span> : <span className="muted">Drop or pick a file…</span>}
                </div>
              </div>
            </div>
          )}
          <label className="sw-ad-up-drop">
            <input type="file" multiple={!isReplace} accept="image/*" onChange={onPick} hidden/>
            <Icon name="upload" size={20}/>
            <div>
              <strong>Drop {isReplace ? "an image" : "images"} here</strong>
              <div className="muted" style={{ fontSize: 12 }}>or click to choose · PNG, JPG, WebP</div>
            </div>
          </label>
          {files.length > 0 && (
            <div className="sw-ad-up-files">
              {files.map((f, i) => (
                <div key={i} className="sw-ad-up-file">
                  <Icon name="image" size={14}/>
                  <span className="mono" style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{f.name}</span>
                  <span className="muted mono" style={{ fontSize: 11 }}>{f.sizeKb} KB</span>
                </div>
              ))}
            </div>
          )}
          <div className="sw-ad-up-or"><span>or</span></div>
          <div className="sw-ad-up-url">
            <Icon name="link" size={14}/>
            <input
              type="url"
              placeholder="Paste image URL (https://…)"
              value={url}
              onChange={e => setUrl(e.target.value)}
              spellCheck={false}/>
          </div>
        </div>
        <div className="sw-ad-up-foot">
          <button type="button" className="btn ghost" onClick={onClose}>Cancel</button>
          <button type="button" className="btn primary"
                  disabled={!kind || (files.length === 0 && !url.trim())}
                  onClick={onClose}>
            {isReplace
              ? "Replace"
              : url.trim() && files.length === 0
                ? "Fetch & upload"
                : `Upload${files.length > 1 ? ` (${files.length})` : ""}`}
          </button>
        </div>
      </div>
    </div>
  );
}

function ConflictPopover({ spec, mode, onClose }) {
  // Anchored to viewport center; in real product this would anchor to the field.
  return (
    <div className="sw-ad-popover-backdrop" onClick={onClose}>
      <div className="sw-ad-popover" onClick={e => e.stopPropagation()}>
        <div className="sw-ad-popover-head">
          <div>
            <div className="sw-ad-popover-eyebrow muted">Provider conflict</div>
            <div className="sw-ad-popover-title">{spec.field}</div>
          </div>
          <button type="button" className="sw-ad-popover-close" onClick={onClose}>✕</button>
        </div>
        <div className="sw-ad-popover-body">
          {mode === "asFinding" ? (
            <div className="muted" style={{ fontSize: 12.5, padding: "12px 4px" }}>
              In <em>as-finding</em> mode this would already be a finding row; the popover is suppressed
              and the finding chip on the field links into the Findings section instead.
            </div>
          ) : (
            <>
              <div className="sw-ad-popover-row resolved">
                <span className="src">RESOLVED</span>
                <span className="val">{Array.isArray(spec.value) ? spec.value.join(", ") : spec.value}</span>
                <span className="muted" style={{ fontSize: 11 }}>currently shown</span>
              </div>
              {(spec.conflicts || []).map((c, i) => (
                <div key={i} className="sw-ad-popover-row">
                  <span className="src">{c.provider.toUpperCase()}</span>
                  <span className="val">{Array.isArray(c.value) ? c.value.join(", ") : c.value}</span>
                  <button type="button" className="btn ghost sm">Use this</button>
                </div>
              ))}
              <div className="sw-ad-popover-row manual">
                <span className="src">MANUAL</span>
                <input type="text" placeholder="Type a value to override…" />
                <button type="button" className="btn primary sm">Set</button>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

Object.assign(window, { ArtistDetailProposal });
