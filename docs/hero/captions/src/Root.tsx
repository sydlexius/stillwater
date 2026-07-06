import React from "react";
import { Composition } from "remotion";
import { HeroStitched, STITCHED_TOTAL_FRAMES } from "./HeroStitched";
import { Outro } from "./Outro";
import { FPS } from "./shots";

const OUTRO_FRAMES = Math.round(2.5 * FPS); // ~2.5s outro

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
