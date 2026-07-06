// HeroStitched.tsx - the SEPARATE-CLIPS hero: each screen is its own trimmed clip
// (recorded by nav-clips.mjs), played back-to-back via <Series>, with a dip-to-
// black at the EXACT clip boundaries (no guessing cut points in a continuous
// recording - the boundary is exactly the cumulative clip duration). Captions are
// per-clip (timed within each clip). Bookend fades at the very start and end.
import React from "react";
import {
  AbsoluteFill,
  interpolate,
  OffthreadVideo,
  Sequence,
  Series,
  staticFile,
  useCurrentFrame,
  useVideoConfig,
} from "remotion";
import { CaptionChip } from "./CaptionChip";
import { Outro } from "./Outro";
import { SHOTS } from "./shots";
import { CLIPS } from "./clips.generated";

const PHRASES: Record<string, string[]> = Object.fromEntries(SHOTS.map((s) => [s.name, s.phrases]));
const STAGGER_SEC = 0.42;
export const OUTRO_SEC = 2.6; // Oleo wordmark outro appended after the loop clip (single source of truth; Root reuses it)

const FADE_OUT = 0.55;
const FADE_IN = 0.65;
const HOLD = 0.09;
const BOOKEND = 0.7;

const chipPlacement = (idx: number, count: number) => {
  const baseBottom = 108;
  const rhythm = 74;
  const bottom = baseBottom + (count - 1 - idx) * rhythm;
  const x = 88 + (idx % 2 === 0 ? 0 : 40);
  return { x, y: bottom };
};

// Captions for ONE clip - each group's window runs to the next group (or clip end).
const ClipCaptions: React.FC<{ captions: { group: string; atSec: number }[]; clipFrames: number }> = ({ captions, clipFrames }) => {
  const { fps } = useVideoConfig();
  return (
    <>
      {captions.map((cap, ci) => {
        const phrases = PHRASES[cap.group] || [];
        const from = Math.round(cap.atSec * fps);
        const nextAt = captions[ci + 1] ? Math.round(captions[ci + 1].atSec * fps) : clipFrames;
        const shotFrames = Math.max(1, nextAt - from);
        return (
          <Sequence key={`${cap.group}-${ci}`} from={from} durationInFrames={shotFrames} name={cap.group}>
            {phrases.map((ph, i) => {
              const { x, y } = chipPlacement(i, phrases.length);
              return (
                <CaptionChip key={`${cap.group}-${i}`} text={ph} delayFrames={Math.round(i * STAGGER_SEC * fps)} shotFrames={shotFrames} x={x} y={y} />
              );
            })}
          </Sequence>
        );
      })}
    </>
  );
};

// Dip-to-black at each exact clip boundary + start/end bookends.
const StitchFades: React.FC = () => {
  const frame = useCurrentFrame();
  const { fps, durationInFrames } = useVideoConfig();
  const t = frame / fps;
  const endSec = durationInFrames / fps;
  const clamp = { extrapolateLeft: "clamp" as const, extrapolateRight: "clamp" as const };
  let op = 0;
  op = Math.max(op, interpolate(t, [0, BOOKEND], [1, 0], clamp));
  op = Math.max(op, interpolate(t, [endSec - BOOKEND, endSec], [0, 1], clamp));
  // Dip at every clip boundary AND at the last-clip -> outro boundary.
  let acc = 0;
  for (let i = 0; i < CLIPS.length; i++) {
    acc += CLIPS[i].durSec;
    op = Math.max(op, interpolate(t, [acc - FADE_OUT, acc - HOLD, acc + HOLD, acc + FADE_IN], [0, 1, 1, 0], clamp));
  }
  return <AbsoluteFill style={{ backgroundColor: "#000", opacity: op, pointerEvents: "none" }} />;
};

export const HeroStitched: React.FC = () => {
  const { fps } = useVideoConfig();
  return (
    <AbsoluteFill style={{ backgroundColor: "#000" }}>
      <Series>
        {CLIPS.map((clip) => {
          const f = Math.max(1, Math.round(clip.durSec * fps));
          return (
            <Series.Sequence key={clip.name} durationInFrames={f}>
              <AbsoluteFill>
                <OffthreadVideo src={staticFile(clip.src)} muted />
              </AbsoluteFill>
              <ClipCaptions captions={clip.captions} clipFrames={f} />
            </Series.Sequence>
          );
        })}
        <Series.Sequence durationInFrames={Math.round(OUTRO_SEC * fps)}>
          <Outro color="blue" />
        </Series.Sequence>
      </Series>
      <StitchFades />
    </AbsoluteFill>
  );
};

export const STITCHED_TOTAL_FRAMES = (fps: number) =>
  CLIPS.reduce((a, c) => a + Math.max(1, Math.round(c.durSec * fps)), 0) + Math.round(OUTRO_SEC * fps);
