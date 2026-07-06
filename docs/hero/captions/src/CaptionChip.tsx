// CaptionChip.tsx - a single kinetic caption line. NO backdrop/bubble (that read
// as subtitles): clean floating white text with a soft dark shadow-halo for
// legibility over any background. Entrance is a clear FADE-IN (opacity) with a
// gentle upward drift; a matching fade-out before the shot cut so nothing hard-cuts.
import React from "react";
import { Easing, interpolate, useCurrentFrame, useVideoConfig } from "remotion";

export type ChipProps = {
  text: string;
  delayFrames: number; // stagger within the shot
  shotFrames: number; // total frames this line lives for
  x: number; // left offset (px)
  y: number; // bottom offset (px)
};

// App-adjacent system sans stack; swappable single token (house style).
const FONT_STACK =
  '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif';

export const CaptionChip: React.FC<ChipProps> = ({ text, delayFrames, shotFrames, x, y }) => {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const local = frame - delayFrames;

  // FADE-IN over ~0.5s with a gentle rise (opacity is the dominant effect).
  const fadeFrames = Math.round(0.5 * fps);
  const appear = interpolate(local, [0, fadeFrames], [0, 1], {
    extrapolateLeft: "clamp",
    extrapolateRight: "clamp",
    easing: Easing.out(Easing.cubic),
  });
  const translateY = interpolate(appear, [0, 1], [16, 0]);

  // Fade OUT over the last ~0.4s of the shot so the cut is never abrupt.
  const exit = interpolate(frame, [shotFrames - 14, shotFrames - 4], [1, 0], {
    extrapolateLeft: "clamp",
    extrapolateRight: "clamp",
  });
  const opacity = Math.min(appear, exit);

  if (local < -2) return null;

  return (
    <div
      style={{
        position: "absolute",
        left: x,
        bottom: y,
        transform: `translateY(${translateY}px)`,
        opacity,
        color: "#f2f5fa",
        fontFamily: FONT_STACK,
        // Cinematic lower-third: lighter weight + generous tracking reads filmic,
        // not UI. Slightly translucent white feels less "hard subtitle".
        fontWeight: 500,
        fontSize: 37,
        letterSpacing: "1.9px",
        lineHeight: 1.15,
        whiteSpace: "nowrap",
        // Large, soft feathered halo (no box) - a wide filmic bloom that lifts the
        // text off busy UI without reading as a subtitle bar. Big blur radius +
        // tighter inner layers for legibility over light or dark regions.
        textShadow:
          "0 6px 60px rgba(0,0,0,0.72), 0 3px 30px rgba(0,0,0,0.6), 0 2px 10px rgba(0,0,0,0.8), 0 1px 3px rgba(0,0,0,0.9)",
      }}
    >
      {text}
    </div>
  );
};
