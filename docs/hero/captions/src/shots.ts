// shots.ts - the storyboard caption copy, keyed by clip/caption group. Each
// group's phrases are placed over its clip by HeroStitched. Caption timing comes
// from clips.generated.ts (written by build-clips.mjs from the recorded clips);
// the durationSec values here are the storyboard's nominal per-shot lengths.

export const FPS = 30;

export type Shot = {
  name: string;
  durationSec: number;
  phrases: string[];
};

export const SHOTS: Shot[] = [
  { name: "dashboard", durationSec: 2.5, phrases: ["What needs attention", "And what's already fixed"] },
  { name: "artists-grid", durationSec: 3.4, phrases: ["Every album artist", "Local, Emby, and Jellyfin", "In one place"] },
  { name: "artist-detail", durationSec: 2.4, phrases: ["One complete view", "Artwork and metadata together"] },
  { name: "metadata", durationSec: 2.5, phrases: ["Pull metadata", "From multiple sources"] },
  { name: "edit", durationSec: 2.4, phrases: ["Edit any field", "Lock it to protect your work"] },
  { name: "images", durationSec: 2.8, phrases: ["Find artwork", "And apply it"] },
  { name: "dashboard-loop", durationSec: 2.0, phrases: ["Your library, in order"] },
];
