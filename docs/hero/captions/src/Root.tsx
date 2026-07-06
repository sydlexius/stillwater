import React from "react";
import { Composition } from "remotion";
import { HeroStitched, OUTRO_SEC, STITCHED_TOTAL_FRAMES } from "./HeroStitched";
import { Outro } from "./Outro";
import { FPS } from "./shots";

// Reuse HeroStitched's OUTRO_SEC so the standalone preview compositions stay in
// sync with the stitched render (was hardcoded 2.5s, drifting from 2.6s).
const OUTRO_FRAMES = Math.round(OUTRO_SEC * FPS);

export const RemotionRoot: React.FC = () => {
  return (
    <>
      <Composition
        id="HeroStitched"
        component={HeroStitched}
        durationInFrames={STITCHED_TOTAL_FRAMES(FPS)}
        fps={FPS}
        width={1600}
        height={900}
        defaultProps={{}}
      />
      <Composition
        id="OutroBlue"
        component={Outro}
        durationInFrames={OUTRO_FRAMES}
        fps={FPS}
        width={1600}
        height={900}
        defaultProps={{ color: "blue" as const }}
      />
      <Composition
        id="OutroLegible"
        component={Outro}
        durationInFrames={OUTRO_FRAMES}
        fps={FPS}
        width={1600}
        height={900}
        defaultProps={{ color: "legible" as const }}
      />
    </>
  );
};
