// Outro.tsx - ~3s brand outro appended after the hero's dashboard-loop beat. Uses
// the NEW Oleo squircle mark (/tmp/favicon-oleo -> web favicon, inlined verbatim)
// and the OLEO SCRIPT brand wordmark (SIL OFL, VENDORED offline via @font-face -
// no fetch-at-render). Entrance is a slow zoom-in + fade-in (tasteful, not springy).
//
// Two wordmark COLOR options via the `color` prop, for the maintainer to pick:
//   'blue'    = #2563eb, the squircle blue exactly as requested (note: low-contrast
//               on the dark background - shown so he can judge it).
//   'legible' = a lighter brand-blue tint with a soft glow - legible on dark, still
//               on-brand.
// Separate scene so it swaps cleanly onto the final composite once the Reframed
// base lands. 16:9 1600x900.
import React, { useEffect, useState } from "react";
import {
  AbsoluteFill,
  continueRender,
  delayRender,
  Easing,
  interpolate,
  staticFile,
  useCurrentFrame,
  useVideoConfig,
} from "remotion";

// Oleo Script TTF is vendored offline (public/fonts). Loaded INSIDE the component
// (below) so it only blocks the Outro's own render - a module-level delayRender
// would also stall the HeroStitched render (which imports this file via Root).
const OLEO = "OleoScriptHero";

export const Outro: React.FC<{ color: "blue" | "legible" }> = ({ color }) => {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();

  // Load the vendored Oleo font only while the Outro renders.
  const [fontHandle] = useState(() => delayRender());
  useEffect(() => {
    const f = new FontFace(OLEO, `url(${staticFile("fonts/OleoScript-Regular.ttf")}) format('truetype')`);
    f.load().then(() => { document.fonts.add(f); continueRender(fontHandle); }).catch(() => continueRender(fontHandle));
  }, [fontHandle]);

  // Slow zoom-in + fade-in over ~1.6s, eased, then settle.
  const inFrames = Math.round(1.6 * fps);
  const t = interpolate(frame, [0, inFrames], [0, 1], { extrapolateLeft: "clamp", extrapolateRight: "clamp", easing: Easing.out(Easing.cubic) });
  const scale = interpolate(t, [0, 1], [0.86, 1]);
  const opacity = interpolate(t, [0, 1], [0, 1]);

  const isBlue = color === "blue";
  const wordColor = isBlue ? "#2563eb" : "#8fb4ff";
  const wordGlow = isBlue
    ? "0 2px 20px rgba(0,0,0,0.5)"
    : "0 0 26px rgba(59,130,246,0.55), 0 2px 18px rgba(0,0,0,0.5)";

  return (
    <AbsoluteFill
      style={{
        background: "radial-gradient(120% 120% at 50% 44%, #16203a 0%, #0a0f1b 55%, #05070d 100%)",
        alignItems: "center",
        justifyContent: "center",
      }}
    >
      <AbsoluteFill style={{ boxShadow: "inset 0 0 340px rgba(0,0,0,0.6)", pointerEvents: "none" }} />

      <div style={{ transform: `scale(${scale})`, opacity, display: "flex", flexDirection: "column", alignItems: "center", gap: 30 }}>
        {/* Wordmark only - the "Stillwater" name in the Oleo Script brand face
            (no squircle mark, per the maintainer). */}
        <div
          style={{
            fontFamily: `${OLEO}, cursive`,
            fontSize: 168,
            lineHeight: 1,
            color: wordColor,
            textShadow: wordGlow,
            paddingBottom: 18, // Oleo descenders; keep the lockup visually centered
          }}
        >
          Stillwater
        </div>

        <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 14 }}>
          <div style={{ fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif', fontWeight: 400, fontSize: 26, letterSpacing: "0.6px", color: "#aebacd" }}>
            One place to manage artist metadata, for every server you run.
          </div>
        </div>
      </div>
    </AbsoluteFill>
  );
};
