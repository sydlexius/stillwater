/* Onboarding v2 — proposal vs current.
 *
 * Self-hoster framing: split-pane layout (rail + form + live TOML preview).
 * The right pane shows the config-on-disk that the form is building, with
 * the active section highlighted. Power users get an "Edit TOML directly"
 * affordance that swaps the form for an editor on the same data.
 *
 * Flow (v2):
 *   0. Account       — create local admin (break-glass, unskippable)
 *   1. Sources       — switches: Local / Emby / Jellyfin (which surfaces does Stillwater see?)
 *   2. Connections   — only if Emby, Jellyfin, or Lidarr is in play
 *   3. Library       — only if Local was chosen in step 1
 *                       · drops "regular vs classical" (lives in Settings later)
 *                       · if Lidarr enabled, offers "Import layout from Lidarr"
 *                         with explicit copy about what that does
 *                       · platform profile is INFERRED, not surfaced
 *   4. Pre-flight    — sanity checks
 *   5. Match artists — to MusicBrainz
 *
 * Why steps are conditional: a Local-only homelabber shouldn't see a
 * Connections step at all; an Emby-only user shouldn't see Library config.
 * The rail rebuilds after step 1 so users don't see phantom steps before
 * they've made their choice.
 */

const { useState: ufState, useEffect: ufEffect, useRef: ufRef } = React;

function OnboardingProposal() {
  const [sources, setSources] = ufState({ local: true, emby: true, jellyfin: false, lidarr: true });
  const [step, setStep] = ufState(1); // start mid-flow so TOML preview is interesting
  const [editing, setEditing] = ufState(false);

  // Steps are computed from `sources` so the rail mirrors what the user
  // actually has to do — no greyed-out phantom steps.
  const steps = getSteps(sources);
  const safeStep = Math.min(step, steps.length - 1);
  const goto = (i) => setStep(Math.max(0, Math.min(steps.length - 1, i)));

  const StepBody = renderStep(steps[safeStep]?.key, {
    sources, setSources,
    onNext: () => goto(safeStep + 1),
    onBack: safeStep > 0 ? () => goto(safeStep - 1) : null,
  });

  return (
    <div className="sw-ob-page">
      <div className="sw-ob-top">
        <div className="brand">
          <div className="glyph"/>
          <div>
            <div className="name">Stillwater · first-run setup</div>
            <div className="meta">v0.14.2 · /config/stillwater.toml</div>
          </div>
        </div>
        <div className="crumb">
          <b>setup</b><span className="sep">/</span>
          <b>{steps[safeStep]?.key}</b>
        </div>
      </div>

      <div className="sw-ob-grid">
        <aside className="sw-ob-rail">
          <h3 className="heading">Setup outline</h3>
          <ol>
            {steps.map((s, i) => (
              <li
                key={s.key}
                className={i < safeStep ? "done" : i === safeStep ? "active" : ""}
                onClick={() => goto(i)}
              >
                <span className="num">{String(i).padStart(2, "0")}</span>
                <span>{s.label}</span>
              </li>
            ))}
          </ol>
          <div className="rail-foot">
            Already have a config?{" "}
            <a href="#" onClick={(e) => { e.preventDefault(); setEditing(true); }}>Edit TOML directly</a>.
            <br/><br/>
            Press <span className="kbd">⌘ K</span> for the command palette,{" "}
            <span className="kbd">?</span> for shortcuts.
          </div>
        </aside>

        <main className="sw-ob-main">
          {StepBody}
        </main>

        <ConfigPane stepKey={steps[safeStep]?.key} sources={sources}/>
      </div>

      {editing && <TomlEditorOverlay sources={sources} onClose={() => setEditing(false)}/>}
    </div>
  );
}

/* Power-user escape hatch. The form is fine for first-timers, but anyone
 * coming from a docker-compose / NixOS / Ansible flow already knows what
 * they want their config to look like — give them the file. Closing the
 * overlay drops them back to the form, with their source toggles intact.
 *
 * The editor is a contentEditable <pre>, not a real text-area, so it
 * preserves syntax-highlighted output as the starting point. We don't
 * round-trip parse it back into form state — that's a "save & restart"
 * concern, not an OOBE one. */
function TomlEditorOverlay({ sources, onClose }) {
  const sections = buildTomlSections(sources);
  const editableToml = sections.map(serializeTomlSection).join("\n\n");
  const lineCount = editableToml.split("\n").length;

  React.useEffect(() => {
    const onKey = (e) => { if (e.key === "Escape") onClose(); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="sw-ob-editor-overlay" onClick={onClose}>
      <div className="sw-ob-editor" onClick={(e) => e.stopPropagation()}>
        <div className="ed-head">
          <div className="ed-path">
            <Icon name="check" size={12}/>
            <span className="mono">/config/stillwater.toml</span>
            <span className="ed-flag">draft</span>
          </div>
          <div className="ed-actions">
            <button className="btn-ghost" onClick={onClose}>Back to form</button>
            <button className="btn-primary">Save &amp; restart</button>
          </div>
        </div>
        <div className="ed-body">
          <pre className="ed-gutter" aria-hidden="true">
            {Array.from({ length: lineCount }, (_, i) => i + 1).join("\n")}
          </pre>
          <pre className="ed-text" contentEditable suppressContentEditableWarning spellCheck={false}>
            {editableToml}
          </pre>
        </div>
        <div className="ed-foot">
          <span className="ok">● valid TOML</span>
          <span>·</span>
          <span>{lineCount} lines</span>
          <span>·</span>
          <span>secrets resolved from <span className="mono">.env</span></span>
          <span style={{ marginLeft: "auto" }}>
            <span className="kbd">Esc</span> to close
          </span>
        </div>
      </div>
    </div>
  );
}

/* The list of every possible step, filtered by what's relevant given the
 * user's source choices. Order is locked: account → sources → connections
 * → library → preflight → discover. Sources determines which middle steps
 * are visible. */
function getSteps(sources) {
  const hasServer = sources.emby || sources.jellyfin || sources.lidarr;
  return [
    { key: "account",   label: "Admin account" },
    { key: "sources",   label: "Where artists come from" },
    hasServer      && { key: "servers",   label: "Server connections" },
    sources.local  && { key: "library",   label: "Music library path" },
    { key: "preflight", label: "Pre-flight checks" },
    { key: "discover",  label: "Match artists" },
  ].filter(Boolean);
}

function renderStep(key, ctx) {
  switch (key) {
    case "account":   return <AccountStep   {...ctx}/>;
    case "sources":   return <SourcesStep   {...ctx}/>;
    case "servers":   return <ServerStep    {...ctx}/>;
    case "library":   return <LibraryStep   {...ctx}/>;
    case "preflight": return <ConflictStep  {...ctx}/>;
    case "discover":  return <DiscoverStep  {...ctx}/>;
    default:          return null;
  }
}

function StepScaffold({ title, lede, children, onBack, onNext, nextLabel = "Continue", hint }) {
  return (
    <>
      <h1>{title}</h1>
      <p className="lede">{lede}</p>
      <div className="panel">{children}</div>
      <div className="nav">
        <span className="muted" style={{ fontSize: 12 }}>{hint || "Use the outline on the left to revisit any step."}</span>
        <div className="row">
          {onBack && <button className="btn ghost" onClick={onBack}>Back</button>}
          {onNext && <button className="btn primary" onClick={onNext}>{nextLabel} <Icon name="chevron" size={12}/></button>}
        </div>
      </div>
    </>
  );
}

/* Step 0 — local admin account. Break-glass: this account exists even if
 * OIDC/LDAP is configured later, so the user can never lock themselves out
 * of their own homelab. Hence: required, no skip, no "skip for now" link. */
function AccountStep({ onNext }) {
  return (
    <StepScaffold
      title="Create your admin account"
      lede="A local account that always works — even if you wire up OIDC or LDAP later. Use this to recover a borked auth setup."
      onNext={onNext}
      hint="Your password is stored as an Argon2id hash in the SQLite store, never in the TOML."
    >
      <div className="row" style={{ gap: 10, marginBottom: 12 }}>
        <div className="field flex-1">
          <label>Username</label>
          <input className="input" defaultValue="admin"/>
        </div>
        <div className="field flex-1">
          <label>Display name</label>
          <input className="input" defaultValue="Syd"/>
        </div>
      </div>
      <div className="row" style={{ gap: 10, marginBottom: 12 }}>
        <div className="field flex-1">
          <label>Password</label>
          <input className="input" type="password" defaultValue="••••••••••••"/>
        </div>
        <div className="field flex-1">
          <label>Confirm</label>
          <input className="input" type="password" defaultValue="••••••••••••"/>
        </div>
      </div>
      <div className="check" style={{ background: "var(--sw-bg-sunken)" }}>
        <div className="ic"><Icon name="shield" size={12}/></div>
        <div>
          <div className="t">This is your break-glass account.</div>
          <div className="d">Stillwater will refuse to disable it from the UI. To rotate it from the host, run <span className="mono">docker exec stillwater sw user passwd admin</span>.</div>
        </div>
      </div>
    </StepScaffold>
  );
}

/* Step 1 — Sources. Switches, not checkboxes (physical metaphor fits the
 * homelab tone). At least one must be on; we don't enforce it visually but
 * the hint copy nudges. All four sources are first-class — Lidarr included.
 *
 * OOBE limits each source to one instance. The footer note signals that
 * adding more (multiple local libraries, multiple Emby servers, etc.) lives
 * in Settings. Honest scope, not a hidden limitation. */
function SourcesStep({ sources, setSources, onNext, onBack }) {
  const flip = (k) => setSources(s => ({ ...s, [k]: !s[k] }));
  const sourceCount = ["local","emby","jellyfin","lidarr"].filter(k => sources[k]).length;
  const noneOn = sourceCount === 0;
  return (
    <StepScaffold
      title="Where do artists come from?"
      lede="Stillwater curates artists, not music files. Pick the surfaces it should read from. You can mix any combination — and add more of each in Settings later."
      onNext={noneOn ? null : onNext} onBack={onBack}
      hint={noneOn ? "Pick at least one source to continue." : `${sourceCount} source${sourceCount === 1 ? "" : "s"} selected. Add more in Settings later.`}
    >
      <div className="col" style={{ gap: 10 }}>
        <SourceSwitch
          name="Local filesystem"
          desc={<>An on-disk directory laid out as <span className="mono">/music/{"{Artist}"}/{"{Album}"}/</span>. Filenames and tags are ignored.</>}
          on={sources.local} onToggle={() => flip("local")}
          icon="folder"
          tail="Stillwater reads folder names and writes alongside (artist.nfo, folder.jpg)."
        />
        <SourceSwitch
          name="Emby"
          desc="Read the album-artist list from an Emby server."
          on={sources.emby} onToggle={() => flip("emby")}
          icon="server"
          tail="Stillwater pushes metadata changes back to Emby's library."
        />
        <SourceSwitch
          name="Jellyfin"
          desc="Read the album-artist list from a Jellyfin server."
          on={sources.jellyfin} onToggle={() => flip("jellyfin")}
          icon="server"
          tail="Same capabilities as Emby — most folks run only one."
        />
        <SourceSwitch
          name="Lidarr"
          desc="Read the artist list and quality profiles from Lidarr."
          on={sources.lidarr} onToggle={() => flip("lidarr")}
          icon="server"
          tail="Read-only. Stillwater also uses Lidarr to detect NFO write-back conflicts."
        />
      </div>

      <div className="muted" style={{ fontSize: 11.5, marginTop: 12, lineHeight: 1.5 }}>
        OOBE configures one instance per source. Multiple local libraries, multiple Emby/Jellyfin servers, and additional Lidarr instances can be added in <span className="mono">Settings → Sources</span>.
      </div>
    </StepScaffold>
  );
}

function SourceSwitch({ name, desc, on, onToggle, icon, tail }) {
  return (
    <div style={{
      padding: 14, borderRadius: 10,
      border: "1px solid " + (on ? "var(--sw-blue-soft-line, var(--sw-line))" : "var(--sw-line)"),
      background: on ? "var(--sw-bg-sunken)" : "transparent",
      transition: "background 120ms",
    }}>
      <div className="row" style={{ alignItems: "flex-start" }}>
        <div style={{ width: 28, height: 28, borderRadius: 7, display: "grid", placeItems: "center", background: on ? "var(--sw-blue-soft)" : "var(--sw-bg-sunken)", color: on ? "var(--sw-blue-ink)" : "var(--sw-ink-3)", flexShrink: 0 }}>
          <Icon name={icon} size={15}/>
        </div>
        <div className="flex-1" style={{ marginLeft: 10 }}>
          <div style={{ fontSize: 13.5, fontWeight: 600 }}>{name}</div>
          <div className="muted" style={{ fontSize: 12, marginTop: 2 }}>{desc}</div>
          {on && tail && <div className="muted" style={{ fontSize: 11.5, marginTop: 6, color: "var(--sw-ink-3)" }}>↳ {tail}</div>}
        </div>
        <Toggle on={on} onChange={onToggle}/>
      </div>
    </div>
  );
}

/* Step 2 — Connections. Conditional: only shown if any non-Local source is
 * enabled. Each card matches the corresponding source switch.
 *
 * OOBE allows one instance per server type; copy reflects this. Adding
 * additional Emby/Jellyfin/Lidarr servers happens in Settings. */
function ServerStep({ sources, onNext, onBack }) {
  return (
    <StepScaffold
      title="Connect your servers"
      lede="One URL + API key per server. Keys live in .env so they never end up in your TOML or git history."
      onNext={onNext} onBack={onBack}
    >
      <div className="col" style={{ gap: 10 }}>
        {sources.emby && (
          <ServerCard
            name="Emby" placeholder="http://192.168.1.100:8096"
            status="connected" libs={5} envKey="EMBY_API_KEY"
            features={[
              "Read album-artist list",
              "Push metadata changes (artist.nfo, biography, genres)",
              "Trigger refresh after writes",
            ]}
          />
        )}
        {sources.jellyfin && (
          <ServerCard
            name="Jellyfin" placeholder="http://192.168.1.100:8097"
            status="off" envKey="JELLYFIN_API_KEY"
            features={["Read album-artist list", "Push metadata changes", "Trigger refresh after writes"]}
          />
        )}
        {sources.lidarr && (
          <ServerCard
            name="Lidarr" placeholder="http://192.168.1.100:8686"
            status="connected" profiles={3} envKey="LIDARR_API_KEY"
            features={["Read artist list", "Read quality profiles", "Detect NFO write-back risk"]}
          />
        )}
      </div>
    </StepScaffold>
  );
}

function ServerCard({ name, placeholder, status, libs, profiles, envKey, features }) {
  return (
    <div style={{ padding: 14, borderRadius: 10, border: "1px solid var(--sw-line)", background: "var(--sw-bg-sunken)" }}>
      <div className="row" style={{ marginBottom: 12 }}>
        <Icon name="server"/>
        <div className="flex-1">
          <div style={{ fontSize: 13.5, fontWeight: 600 }}>{name}</div>
          <div className="muted" style={{ fontSize: 11.5, marginTop: 1 }}>
            {status === "connected" && <><span style={{ color: "var(--sw-ok)" }}>● Connected</span> · {libs ? `${libs} libraries` : `${profiles} profiles`}</>}
            {status === "off"       && <span>Not yet tested</span>}
          </div>
        </div>
      </div>
      <div className="row" style={{ gap: 6, marginBottom: 8 }}>
        <input className="input mono flex-1" defaultValue={placeholder} style={{ minWidth: 0 }}/>
        <input className="input mono" placeholder="API key" style={{ width: 130 }} type="password" defaultValue="••••••••"/>
        <button className="btn ghost">Test</button>
      </div>
      <div className="muted" style={{ fontSize: 11, fontFamily: "var(--sw-mono)", marginBottom: 8 }}>
        Reads <span style={{ color: "var(--sw-warm)" }}>${"{" + envKey + "}"}</span> from .env if set.
      </div>
      <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "grid", gap: 4 }}>
        {features.map(f => (
          <li key={f} className="row muted" style={{ fontSize: 12 }}>
            <Icon name="check" size={11}/>
            <span>{f}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

/* Step 3 — Library path. Conditional on sources.local. No "regular vs
 * classical" anymore (deferred to Settings — keeps OOBE minimal). Platform
 * profile is *inferred* from runtime context (Docker? root? Emby mount
 * style?), and surfaced as a one-liner the user can change in Settings.
 *
 * Stillwater only needs Artist/Album shape — album titles are used as a
 * disambiguation hint when the artist name alone is too generic to match
 * MusicBrainz. Filenames and ID3 tags are ignored entirely.
 *
 * If Lidarr is enabled, offers an explicit "Import layout from Lidarr"
 * affordance with copy that says exactly what it does. (NB: requires a
 * Lidarr API extension — flagged as such in the help text.)
 */
function LibraryStep({ sources, onNext, onBack }) {
  const [importing, setImporting] = ufState(false);
  return (
    <StepScaffold
      title="Where's your music?"
      lede={<>Stillwater needs the path to your artist folders. The shape it expects is <span className="mono">/music/{"{Artist}"}/{"{Album}"}/</span> — album folders are used only as a hint when the artist name is too ambiguous to match. Track filenames and ID3 tags are ignored.</>}
      onNext={onNext} onBack={onBack}
    >
      <div className="field">
        <label>Path inside container</label>
        <div className="row" style={{ gap: 6 }}>
          <input className="input mono flex-1" defaultValue="/music"/>
          <button className="btn ghost"><Icon name="folder" size={13}/> Browse</button>
        </div>
        <div className="help">
          Maps to <span className="mono">/path/to/your/music</span> on your host via the Docker volume mount.
          Set <span className="mono">SW_MUSIC_PATH</span> in <span className="mono">.env</span> to override at runtime.
        </div>
      </div>

      {sources.lidarr && (
        <>
          <div className="divider"/>
          <div style={{
            padding: 12, borderRadius: 8,
            border: "1px solid var(--sw-line)",
            background: importing ? "var(--sw-blue-soft)" : "var(--sw-bg-sunken)",
          }}>
            <div className="row" style={{ alignItems: "flex-start", gap: 10 }}>
              <Icon name="server" size={14}/>
              <div className="flex-1">
                <div style={{ fontSize: 13, fontWeight: 600 }}>Import layout from Lidarr</div>
                <div className="muted" style={{ fontSize: 12, marginTop: 3 }}>
                  Read your Lidarr root-folder paths and naming format and pre-fill them here. Stillwater will not modify Lidarr — it only <em>reads</em> the layout, then uses it as its own library path.
                </div>
                <div className="muted" style={{ fontSize: 11, marginTop: 6, fontFamily: "var(--sw-mono)" }}>
                  GET /api/v1/rootfolder · GET /api/v1/config/naming
                </div>
              </div>
              <button className="btn ghost sm" onClick={() => setImporting(v => !v)}>
                {importing ? "Cancel" : "Import"}
              </button>
            </div>
            {importing && (
              <div className="row" style={{ marginTop: 10, gap: 8, fontSize: 12, color: "var(--sw-ok)", paddingTop: 10, borderTop: "1px solid var(--sw-line)" }}>
                <Icon name="check" size={12}/>
                <span>Imported root <span className="mono">/music</span> · naming <span className="mono">{"{Artist Name}/{Album Title}"}</span> — matches.</span>
              </div>
            )}
          </div>
        </>
      )}

      <div className="divider"/>
      <div className="row" style={{ gap: 8, fontSize: 12, color: "var(--sw-ink-3)", flexWrap: "wrap" }}>
        <Icon name="folder" size={13}/>
        <span>Detected:</span>
        <span className="mono" style={{ background: "var(--sw-bg-sunken)", padding: "2px 8px", borderRadius: 4 }}>
          /music/{"{Artist}"}/{"{Album}"}/
        </span>
        <span className="sev info" style={{ marginLeft: "auto" }}><span className="dot"/>1,284 artists · 12,409 albums</span>
      </div>

      <div className="muted" style={{ fontSize: 11.5, marginTop: 10 }}>
        Add another local library in <span className="mono">Settings → Sources</span>. Platform profile inferred from runtime: <span className="mono">docker · linuxserver/uid 1000</span> (changeable in Settings).
      </div>
    </StepScaffold>
  );
}

function ConflictStep({ onNext, onBack }) {
  return (
    <StepScaffold
      title="Pre-flight"
      lede="Stillwater is the source of truth for artist.nfo and folder.jpg. If another tool also writes them, you get duplicates — these checks catch that."
      onNext={onNext} onBack={onBack}
    >
      <div className="col" style={{ gap: 8 }}>
        <div className="check">
          <div className="ic"><Icon name="check" size={11}/></div>
          <div>
            <div className="t">Library paths don't overlap between connections</div>
            <div className="d">Emby reads <span className="mono">/music</span>; no other connection touches it.</div>
          </div>
        </div>
        <div className="check">
          <div className="ic"><Icon name="check" size={11}/></div>
          <div>
            <div className="t">Emby NFO save-back is disabled</div>
            <div className="d">Verified via Emby API · <span className="mono">Library &gt; Save artwork into media folders = false</span></div>
          </div>
        </div>
        <div className="check warn">
          <div className="ic"><Icon name="warn" size={12}/></div>
          <div>
            <div className="t">Lidarr "save metadata" is enabled</div>
            <div className="d">
              Lidarr will overwrite <span className="mono">artist.nfo</span> on its next refresh. Either disable
              that in Lidarr (Settings → Metadata → Kodi/Emby), or{" "}
              <a style={{ color: "var(--sw-blue-ink)" }}>let Stillwater manage it for you →</a>
            </div>
          </div>
        </div>
      </div>
      <div className="divider"/>
      <div className="muted" style={{ fontSize: 12, fontFamily: "var(--sw-mono)" }}>
        Re-run anytime: <span className="mono">curl localhost:8080/api/v1/preflight | jq</span>
      </div>
    </StepScaffold>
  );
}

function DiscoverStep({ onBack }) {
  return (
    <StepScaffold
      title="Match artists to MusicBrainz"
      lede="Folder names → MusicBrainz IDs, so providers know who's who. Auto-matches link silently; ambiguous ones land in a review queue."
      onBack={onBack} nextLabel="Apply config & start"
      onNext={() => {}}
    >
      <div className="row" style={{ marginBottom: 14, gap: 10, alignItems: "flex-start" }}>
        <div style={{ width: 32, height: 32, borderRadius: 8, background: "var(--sw-blue-soft)", color: "var(--sw-blue-ink)", display: "grid", placeItems: "center", flexShrink: 0 }}>
          <Icon name="image" size={16}/>
        </div>
        <div>
          <div style={{ fontSize: 13.5 }}><strong>1,284 artists found.</strong> Estimated match time: ~90 seconds.</div>
          <div className="muted" style={{ fontSize: 12, marginTop: 2 }}>This compares folder names + album titles to MusicBrainz. No metadata is written yet — that happens on the first scheduled run.</div>
        </div>
      </div>
      <div className="col" style={{ gap: 6 }}>
        <div className="check">
          <div className="ic"><Icon name="check" size={11}/></div>
          <div className="t">Match by folder name + album titles</div>
        </div>
        <div className="check">
          <div className="ic"><Icon name="check" size={11}/></div>
          <div className="t">Compare against MusicBrainz (cached locally for 30 days)</div>
        </div>
        <div className="check">
          <div className="ic"><Icon name="check" size={11}/></div>
          <div className="t">Queue ambiguous artists for review (typically &lt; 5%)</div>
        </div>
      </div>
      <div className="divider"/>
      <div className="muted" style={{ fontSize: 12, fontFamily: "var(--sw-mono)" }}>
        Headless? <span className="mono">docker exec stillwater sw discover --auto-link=high-confidence</span>
      </div>
    </StepScaffold>
  );
}

/* Right-hand pane: live YAML preview, with the active section highlighted.
 * Sections are computed from the user's source choices — the TOML mirrors
 * the rail. */
function ConfigPane({ stepKey, sources }) {
  const preRef = React.useRef(null);

  const sections = buildTomlSections(sources);
  const activeIdx = sections.findIndex(s => s.stepKey === stepKey);

  React.useEffect(() => {
    const pre = preRef.current;
    if (!pre || activeIdx < 0) return;
    // The dev-mode text-editor wraps children in a __om-t span, breaking
    // direct refs — query by data attribute instead.
    const el = pre.querySelector(`[data-toml-section="${activeIdx}"]`);
    if (!el) return;
    const elTop = el.offsetTop - pre.offsetTop;
    pre.scrollTo({ top: Math.max(0, elTop - 20), behavior: "smooth" });
  }, [activeIdx]);

  const lineCount = sections.reduce((n, s) => n + 1 + s.kvs.length + (s.cmt ? 1 : 0), 0);

  return (
    <aside className="sw-ob-pane">
      <div className="pane-head">
        <span className="pane-title">stillwater.toml</span>
        <span className="pane-sub">live preview · read-only</span>
      </div>

      <pre ref={preRef}>
        {sections.map((sec, i) => (
          <span
            key={sec.id}
            data-toml-section={i}
            className={i === activeIdx ? "y-hl" : ""}
            style={{ display: "block" }}
          >
            {sec.cmt && (
              <span style={{ display: "block" }}>
                <span className="y-cmt">{sec.cmt}</span>
              </span>
            )}
            <span style={{ display: "block" }}>
              <span className="y-key">{sec.array ? `[[${sec.header}]]` : `[${sec.header}]`}</span>
            </span>
            {sec.kvs.map((kv, j) => (
              <span key={j} style={{ display: "block" }}>
                <TomlKV kv={kv}/>
              </span>
            ))}
            {i < sections.length - 1 && <span style={{ display: "block" }}>{"\u00a0"}</span>}
          </span>
        ))}
      </pre>

      <div className="pane-foot">
        <span className="ok">● valid</span>
        <span>·</span>
        <span>{lineCount} lines</span>
        <span>·</span>
        <span>secrets resolved from .env</span>
      </div>
    </aside>
  );
}

/* TOML structure: ordered, conditional. Each section ties back to a
 * stepKey so the active rail step highlights the right block. Repeated
 * `[[connections]]` and `[[libraries]]` use TOML's array-of-tables form. */
function buildTomlSections(sources) {
  const out = [];
  out.push({
    id: "auth", stepKey: "account", header: "auth",
    kvs: [
      { k: "local_admin", v: "admin", str: true },
      { k: "bcrypt_path", v: "/config/users.db", str: true },
    ],
  });
  out.push({
    id: "sources", stepKey: "sources", header: "sources",
    kvs: [
      { k: "local",    v: sources.local    ? "true" : "false" },
      { k: "emby",     v: sources.emby     ? "true" : "false" },
      { k: "jellyfin", v: sources.jellyfin ? "true" : "false" },
      { k: "lidarr",   v: sources.lidarr   ? "true" : "false" },
    ],
  });
  if (sources.emby) {
    out.push({
      id: "conn-emby", stepKey: "servers", header: "connections", array: true,
      kvs: [
        { k: "type",    v: "emby", str: true },
        { k: "url",     v: "http://192.168.1.100:8096", str: true },
        { k: "api_key", env: "EMBY_API_KEY" },
      ],
    });
  }
  if (sources.jellyfin) {
    out.push({
      id: "conn-jellyfin", stepKey: "servers", header: "connections", array: true,
      kvs: [
        { k: "type",    v: "jellyfin", str: true },
        { k: "url",     v: "http://192.168.1.100:8097", str: true },
        { k: "api_key", env: "JELLYFIN_API_KEY" },
      ],
    });
  }
  if (sources.lidarr) {
    out.push({
      id: "conn-lidarr", stepKey: "servers", header: "connections", array: true,
      kvs: [
        { k: "type",    v: "lidarr", str: true },
        { k: "url",     v: "http://192.168.1.100:8686", str: true },
        { k: "api_key", env: "LIDARR_API_KEY" },
      ],
    });
  }
  if (sources.local) {
    out.push({
      id: "lib-main", stepKey: "library", header: "libraries", array: true,
      kvs: [
        { k: "name", v: "Main library", str: true },
        { k: "path", v: "/music", str: true },
      ],
    });
  }
  out.push({
    id: "preflight", stepKey: "preflight", header: "preflight",
    cmt: "# pre-flight runs on every restart",
    kvs: [
      { k: "fail_on_overlap",   v: "true" },
      { k: "warn_on_writeback", v: "true" },
    ],
  });
  out.push({
    id: "run", stepKey: "discover", header: "run_after_setup",
    kvs: [
      { k: "steps", arr: ["discover", "rules"] },
    ],
  });
  return out;
}

function serializeTomlSection(sec) {
  const head = sec.array ? `[[${sec.header}]]` : `[${sec.header}]`;
  const lines = sec.kvs.map(kv => {
    let v;
    if (kv.env)        v = `"\${${kv.env}}"`;
    else if (kv.arr)   v = `[${kv.arr.map(x => `"${x}"`).join(", ")}]`;
    else if (kv.str)   v = `"${kv.v}"`;
    else               v = kv.v;
    return `${kv.k} = ${v}`;
  });
  return [sec.cmt, head, ...lines].filter(Boolean).join("\n");
}

function TomlKV({ kv }) {
  let valEl;
  if (kv.env) {
    valEl = <><span className="y-str">"</span><span className="y-env">${"{" + kv.env + "}"}</span><span className="y-str">"</span></>;
  } else if (kv.arr) {
    valEl = <span className="y-str">[{kv.arr.map((x, i) => <React.Fragment key={i}>{i > 0 && ", "}"{x}"</React.Fragment>)}]</span>;
  } else if (kv.str) {
    valEl = <span className="y-str">"{kv.v}"</span>;
  } else {
    valEl = <span className="y-str">{kv.v}</span>;
  }
  return (
    <>
      <span className="y-key">{kv.k}</span>
      <span> = </span>
      {valEl}
    </>
  );
}


/* "Current" — close to the live wizard layout, kept for comparison. */
function OnboardingCurrent() {
  return (
    <div style={{ minHeight: 720, background: "var(--sw-bg-base)", display: "grid", placeItems: "start center", paddingTop: 60 }}>
      <div style={{ maxWidth: 640, width: "100%" }}>
        <div style={{ textAlign: "center", marginBottom: 24 }}>
          <div style={{ width: 48, height: 48, borderRadius: 10, background: "linear-gradient(135deg, #3b82f6, #6366f1)", margin: "0 auto 10px" }}/>
          <h1 style={{ color: "var(--sw-blue-ink)", fontSize: 28, fontWeight: 700 }}>Welcome to Stillwater</h1>
          <p className="muted" style={{ fontSize: 13 }}>Configure Stillwater to manage your artist metadata. This wizard takes about 5 minutes.</p>
        </div>
        <div className="row" style={{ justifyContent: "center", marginBottom: 22, gap: 0 }}>
          {[1,2,3,4,5,6].map(n => (
            <React.Fragment key={n}>
              <div style={{ width: 32, height: 32, borderRadius: 999, display: "grid", placeItems: "center", fontSize: 13, fontWeight: 500,
                background: n <= 2 ? "var(--sw-blue)" : "rgba(148,163,184,0.18)",
                color: n <= 2 ? "white" : "var(--sw-ink-3)" }}>{n}</div>
              {n < 6 && <div style={{ width: 30, height: 2, background: n < 2 ? "var(--sw-blue)" : "rgba(148,163,184,0.18)", margin: "0 8px" }}/>}
            </React.Fragment>
          ))}
        </div>
        <div className="sw-card" style={{ padding: 24 }}>
          <h2 style={{ fontSize: 17, fontWeight: 600, margin: "0 0 4px" }}>Server connections</h2>
          <p className="muted" style={{ fontSize: 13, marginBottom: 16 }}>
            Connect Stillwater to your media servers. You can always add more later from Settings.
          </p>
          <div className="col" style={{ gap: 14 }}>
            {["Emby", "Jellyfin", "Lidarr"].map(n => (
              <div key={n} style={{ padding: 14, borderRadius: 10, border: "1px solid var(--sw-line)" }}>
                <div className="row" style={{ marginBottom: 8 }}>
                  <strong>{n}</strong>
                  <Toggle on={n === "Emby"}/>
                </div>
                <ul style={{ margin: 0, paddingLeft: 18, fontSize: 12, color: "var(--sw-ink-3)", lineHeight: 1.6 }}>
                  <li>{n === "Lidarr" ? "Read-only access for profiles and metadata" : `Import artists from ${n} libraries`}</li>
                  <li>{n === "Lidarr" ? "Cannot be selected as a music library source" : `Push metadata changes back to ${n}`}</li>
                  <li>{n === "Lidarr" ? "Detects NFO save-back risk" : "Trigger artist refresh after metadata changes"}</li>
                </ul>
              </div>
            ))}
          </div>
        </div>
        <div className="row" style={{ marginTop: 18, justifyContent: "space-between" }}>
          <button className="skip" style={{ color: "var(--sw-ink-3)", fontSize: 13 }}>Skip Setup</button>
          <div className="row">
            <button className="btn ghost">Back</button>
            <button className="btn primary">Next</button>
          </div>
        </div>
      </div>
    </div>
  );
}

Object.assign(window, { OnboardingProposal, OnboardingCurrent });
