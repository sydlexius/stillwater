/* Artists list with bulk-edit.
 *
 * Bulk-edit answers: "I have 1,200 artists, fix something across N of them."
 * Entry points: this list (checkbox-select), Reports (Fix all 47…), Dashboard queue (Fix selected).
 *
 * Behaviour:
 *  - Selection persists across filter changes; a "47 selected" chip stays in the
 *    toolbar even when you've filtered down to 5 rows.
 *  - When a filter is active, the toolbar offers two scopes side-by-side:
 *      "5 selected"  ·  "Apply to all 47 matching"
 *    so there's no ambiguity about what's about to change.
 *  - Dry-run by default. Every action lands in a preview drawer with
 *    "Will change 42, skip 5" and an explicit Apply.
 *  - Long-running ops become a dismissible progress card in the bottom-right.
 *  - Every bulk action is reversible from Activity for 14 days.
 *  - Density auto-tightens in bulk mode.
 */

const SOFT_SELECT_CEILING = 500;
const UNDO_WINDOW_DAYS = 14;

const allArtists = (() => {
  const base = [
    ["Pink Floyd",        "group",  "Main",      ["local","emby","lidarr"], 5, [1,1,1,1,1]],
    ["Radiohead",         "group",  "Main",      ["local","emby","lidarr"], 5, [0,1,1,1,1]],
    ["Brian Eno",         "person", "Main",      ["local","emby"],          4, [1,1,1,1,0]],
    ["Aphex Twin",        "person", "Main",      ["local","emby","lidarr"], 5, [1,1,1,1,1]],
    ["Boards of Canada",  "group",  "Main",      ["local","emby"],          3, [1,1,0,1,0]],
    ["Burial",            "person", "Main",      ["local","lidarr"],        2, [1,0,0,1,0]],
    ["Tim Hecker",        "person", "Main",      ["local","emby"],          4, [1,1,1,0,0]],
    ["Fennesz",           "person", "Main",      ["local"],                 3, [1,1,1,0,0]],
    ["Tame Impala",       "group",  "Main",      ["local","emby","lidarr"], 5, [1,1,1,1,1]],
    ["FKA twigs",         "person", "Main",      ["local","emby","lidarr"], 4, [1,1,1,1,0]],
    ["Massive Attack",    "group",  "Main",      ["local","emby"],          4, [1,1,1,1,0]],
    ["Portishead",        "group",  "Main",      ["local","emby"],          5, [1,1,1,1,1]],
    ["Sigur Rós",         "group",  "Main",      ["local","lidarr"],        3, [1,1,0,1,0]],
    ["Björk",             "person", "Main",      ["local","emby","lidarr"], 5, [1,1,1,1,1]],
    ["Caribou",           "person", "Main",      ["local"],                 2, [0,0,0,0,0]],
    ["Four Tet",          "person", "Main",      ["local","emby"],          4, [1,1,1,1,0]],
    ["The Knife",         "group",  "Main",      ["local"],                 3, [1,1,0,0,0]],
    ["Fever Ray",         "person", "Main",      ["local"],                 2, [1,1,0,0,0]],
    ["Arvo Pärt",         "person", "Classical", ["local"],                 2, [1,1,0,0,0]],
    ["Steve Reich",       "person", "Classical", ["local","emby"],          3, [1,1,0,0,0]],
    ["Philip Glass",      "person", "Classical", ["local","emby"],          4, [1,1,1,0,0]],
    ["Max Richter",       "person", "Classical", ["local","emby","lidarr"], 5, [1,1,1,1,1]],
    ["Nils Frahm",        "person", "Classical", ["local","emby"],          3, [1,1,0,0,0]],
    ["Ólafur Arnalds",    "person", "Classical", ["local","lidarr"],        3, [1,1,0,1,0]],
    ["Ryuichi Sakamoto",  "person", "Classical", ["local","emby"],          5, [1,1,1,1,1]],
    ["Jóhann Jóhannsson", "person", "Classical", ["local"],                 2, [1,0,0,0,0]],
    ["Hauschka",          "person", "Classical", ["local"],                 1, [0,0,0,0,0]],
    ["A Winged Victory",  "group",  "Classical", ["local"],                 1, [0,0,0,0,0]],
    ["Stars of the Lid",  "group",  "Classical", ["local","emby"],          3, [1,1,0,0,0]],
    ["William Basinski",  "person", "Classical", ["local"],                 2, [1,1,0,0,0]],
    ["Loscil",            "person", "Main",      ["local"],                 2, [1,0,0,0,0]],
    ["Jon Hopkins",       "person", "Main",      ["local","emby","lidarr"], 5, [1,1,1,1,1]],
    ["Floating Points",   "person", "Main",      ["local","emby"],          3, [1,1,0,0,0]],
    ["Bonobo",            "person", "Main",      ["local","emby","lidarr"], 5, [1,1,1,1,1]],
    ["The xx",            "group",  "Main",      ["local","emby"],          4, [1,1,1,1,0]],
    ["Mount Kimbie",      "group",  "Main",      ["local"],                 2, [0,0,0,0,0]],
  ];
  return base.map(([name, type, lib, srcs, idsHave, ids]) => {
    const fields = {
      bio:    !!ids[0],
      thumb:  !!ids[1],
      fanart: !!ids[2],
      logo:   !!ids[3],
      nfo:    !!ids[4],
    };
    const fieldsHave = ids.reduce((a, b) => a + b, 0);
    const compliance = Math.round(((fieldsHave / 5) * 0.6 + (idsHave / 5) * 0.4) * 100);
    return { name, type, lib, srcs, idsHave, idsTotal: 5, fields, compliance };
  });
})();

function actionLabel(kind) {
  switch (kind) {
    case "rerun-rules":    return "Re-run rules";
    case "refetch":        return "Re-fetch metadata";
    case "refetch-images": return "Re-fetch images";
    case "set-field":      return "Set / clear field";
    case "tag":            return "Add or remove tag";
    default:               return kind;
  }
}
