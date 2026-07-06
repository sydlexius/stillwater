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

// The NEW Oleo squircle mark, inlined verbatim (n=4 superellipse in #2563eb + white
// Oleo "S" glyph).
const MARK_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="100%" height="100%">
  <path d="M 31.850,16.000 L 31.848,18.483 L 31.840,19.511 L 31.829,20.299 L 31.812,20.962 L 31.790,21.545 L 31.764,22.071 L 31.733,22.554 L 31.697,23.001 L 31.656,23.419 L 31.611,23.813 L 31.560,24.186 L 31.505,24.540 L 31.445,24.877 L 31.380,25.200 L 31.310,25.509 L 31.235,25.805 L 31.155,26.090 L 31.070,26.364 L 30.980,26.628 L 30.885,26.882 L 30.785,27.128 L 30.679,27.365 L 30.569,27.593 L 30.453,27.814 L 30.332,28.027 L 30.205,28.233 L 30.073,28.432 L 29.935,28.624 L 29.792,28.810 L 29.643,28.989 L 29.489,29.162 L 29.328,29.328 L 29.162,29.489 L 28.989,29.643 L 28.810,29.792 L 28.624,29.935 L 28.432,30.073 L 28.233,30.205 L 28.027,30.332 L 27.814,30.453 L 27.593,30.569 L 27.365,30.679 L 27.128,30.785 L 26.882,30.885 L 26.628,30.980 L 26.364,31.070 L 26.090,31.155 L 25.805,31.235 L 25.509,31.310 L 25.200,31.380 L 24.877,31.445 L 24.540,31.505 L 24.186,31.560 L 23.813,31.611 L 23.419,31.656 L 23.001,31.697 L 22.554,31.733 L 22.071,31.764 L 21.545,31.790 L 20.962,31.812 L 20.299,31.829 L 19.511,31.840 L 18.483,31.848 L 16.000,31.850 L 13.517,31.848 L 12.489,31.840 L 11.701,31.829 L 11.038,31.812 L 10.455,31.790 L 9.929,31.764 L 9.446,31.733 L 8.999,31.697 L 8.581,31.656 L 8.187,31.611 L 7.814,31.560 L 7.460,31.505 L 7.123,31.445 L 6.800,31.380 L 6.491,31.310 L 6.195,31.235 L 5.910,31.155 L 5.636,31.070 L 5.372,30.980 L 5.118,30.885 L 4.872,30.785 L 4.635,30.679 L 4.407,30.569 L 4.186,30.453 L 3.973,30.332 L 3.767,30.205 L 3.568,30.073 L 3.376,29.935 L 3.190,29.792 L 3.011,29.643 L 2.838,29.489 L 2.672,29.328 L 2.511,29.162 L 2.357,28.989 L 2.208,28.810 L 2.065,28.624 L 1.927,28.432 L 1.795,28.233 L 1.668,28.027 L 1.547,27.814 L 1.431,27.593 L 1.321,27.365 L 1.215,27.128 L 1.115,26.882 L 1.020,26.628 L 0.930,26.364 L 0.845,26.090 L 0.765,25.805 L 0.690,25.509 L 0.620,25.200 L 0.555,24.877 L 0.495,24.540 L 0.440,24.186 L 0.389,23.813 L 0.344,23.419 L 0.303,23.001 L 0.267,22.554 L 0.236,22.071 L 0.210,21.545 L 0.188,20.962 L 0.171,20.299 L 0.160,19.511 L 0.152,18.483 L 0.150,16.000 L 0.152,13.517 L 0.160,12.489 L 0.171,11.701 L 0.188,11.038 L 0.210,10.455 L 0.236,9.929 L 0.267,9.446 L 0.303,8.999 L 0.344,8.581 L 0.389,8.187 L 0.440,7.814 L 0.495,7.460 L 0.555,7.123 L 0.620,6.800 L 0.690,6.491 L 0.765,6.195 L 0.845,5.910 L 0.930,5.636 L 1.020,5.372 L 1.115,5.118 L 1.215,4.872 L 1.321,4.635 L 1.431,4.407 L 1.547,4.186 L 1.668,3.973 L 1.795,3.767 L 1.927,3.568 L 2.065,3.376 L 2.208,3.190 L 2.357,3.011 L 2.511,2.838 L 2.672,2.672 L 2.838,2.511 L 3.011,2.357 L 3.190,2.208 L 3.376,2.065 L 3.568,1.927 L 3.767,1.795 L 3.973,1.668 L 4.186,1.547 L 4.407,1.431 L 4.635,1.321 L 4.872,1.215 L 5.118,1.115 L 5.372,1.020 L 5.636,0.930 L 5.910,0.845 L 6.195,0.765 L 6.491,0.690 L 6.800,0.620 L 7.123,0.555 L 7.460,0.495 L 7.814,0.440 L 8.187,0.389 L 8.581,0.344 L 8.999,0.303 L 9.446,0.267 L 9.929,0.236 L 10.455,0.210 L 11.038,0.188 L 11.701,0.171 L 12.489,0.160 L 13.517,0.152 L 16.000,0.150 L 18.483,0.152 L 19.511,0.160 L 20.299,0.171 L 20.962,0.188 L 21.545,0.210 L 22.071,0.236 L 22.554,0.267 L 23.001,0.303 L 23.419,0.344 L 23.813,0.389 L 24.186,0.440 L 24.540,0.495 L 24.877,0.555 L 25.200,0.620 L 25.509,0.690 L 25.805,0.765 L 26.090,0.845 L 26.364,0.930 L 26.628,1.020 L 26.882,1.115 L 27.128,1.215 L 27.365,1.321 L 27.593,1.431 L 27.814,1.547 L 28.027,1.668 L 28.233,1.795 L 28.432,1.927 L 28.624,2.065 L 28.810,2.208 L 28.989,2.357 L 29.162,2.511 L 29.328,2.672 L 29.489,2.838 L 29.643,3.011 L 29.792,3.190 L 29.935,3.376 L 30.073,3.568 L 30.205,3.767 L 30.332,3.973 L 30.453,4.186 L 30.569,4.407 L 30.679,4.635 L 30.785,4.872 L 30.885,5.118 L 30.980,5.372 L 31.070,5.636 L 31.155,5.910 L 31.235,6.195 L 31.310,6.491 L 31.380,6.800 L 31.445,7.123 L 31.505,7.460 L 31.560,7.814 L 31.611,8.187 L 31.656,8.581 L 31.697,8.999 L 31.733,9.446 L 31.764,9.929 L 31.790,10.455 L 31.812,11.038 L 31.829,11.701 L 31.840,12.489 L 31.848,13.517 Z" fill="#2563eb"/>
  <path transform="translate(8.1551 27.6011) scale(0.033241 -0.033241)" d="M432 196Q432 100 359.0 44.0Q286 -12 182 -12Q112 -12 66.5 23.5Q21 59 16 84Q27 117 63.0 158.0Q99 199 128 208Q179 175 223 61Q288 63 288 132Q288 170 257.5 225.5Q227 281 191.0 329.5Q155 378 124.5 438.5Q94 499 94 546Q94 622 155.5 666.0Q217 710 294.0 710.0Q371 710 413.5 677.0Q456 644 456.0 602.0Q456 560 424.0 516.0Q392 472 360 446L328 420Q340 404 357.0 379.0Q374 354 403.0 294.0Q432 234 432 196ZM274 493 289 474Q299 479 320.5 495.0Q342 511 355.5 524.0Q369 537 380.0 555.5Q391 574 391.0 594.5Q391 615 367.0 629.0Q343 643 309.5 643.0Q276 643 253.5 627.5Q231 612 231.0 590.5Q231 569 245.5 540.5Q260 512 274 493Z" fill="#fff"/>
</svg>`;

const MARK = 156;

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
