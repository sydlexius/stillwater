---
description: Turn rules on and off, switch between manual and auto modes, tune thresholds.
---

<!-- code: web/templates/settings.templ rules tab (rules grouped by category, automation mode toggles, conflict-gated chips), internal/api/router.go (PUT /api/v1/rules/{id}, /toggle, /run), internal/rule/service.go (defaults). -->

# Enable and configure rules

Stillwater ships with 27 built-in rules. Some are enabled out of the box; the rest are opinionated, or make destructive changes, and start disabled. This page covers turning them on and off, picking the right automation mode, tuning thresholds, and scheduling recurring evaluation.

For the *concept* (the three states, fix-all, the conflict gate), see [rules in core concepts](../core-concepts/rules.md). For the full per-rule reference, see [rules catalog](../reference/rules-catalogue.md).

## Turn a rule on or off

1. Go to **Settings > Rules**.
2. Find the rule. Rules are grouped by category (NFO, Image, Metadata). Inside the Image category, rules are further sub-grouped by image type -- Thumbnail, Fanart, Logo, Banner, Backdrop, and General -- so the per-asset knobs sit together.
3. Toggle the **Enabled** switch.

If you cannot remember which tab a setting lives on, use the search box at the top of the Settings page; see [find a setting](find-a-setting.md).

A disabled rule never evaluates. It doesn't appear in violation counts and doesn't surface in the artist's violations list. Re-enable to start finding violations again; the next rule run picks them up.

<!-- SCREENSHOT: Settings > Rules | state: rules tab with mix of enabled/disabled + manual/auto + a conflict-gated chip | annotation: enable toggle + mode picker + conflict-gate indicator -->

## Switch between manual and auto

Once a rule is enabled, you choose its automation mode:

- **Manual** -- the rule finds violations; fixes wait for you to click. Best for rules where the fix is opinionated (e.g., renaming a directory) or destructive (e.g., deleting extraneous image files).
- **Auto** -- the rule finds violations and the fixer runs immediately as part of evaluation. Best for rules where the fix is straightforward and safe (e.g., fetching a higher-resolution thumb).

To switch: click the mode dropdown on the rule's row and pick the other option.

The shipped defaults aim for low risk -- most rules ship in auto mode for safe fixes, the more opinionated ones (extraneous images, directory rename, metadata-quality) ship manual.

## Tune a rule's thresholds

Many rules have thresholds you can adjust. To open a rule's config:

1. Click the rule's name (or the gear icon on its row).
2. The config panel shows the rule's tunable knobs -- minimum dimensions, aspect ratios, biography minimum length, similarity tolerances, etc.
3. Edit and click **Save**.

Common tuning examples:

- **Lower fanart minimum resolution.** Default is 1920x1080. If your providers can't reliably deliver 1080p, drop to 1280x720 to stop the rule nagging.
- **Higher biography minimum length.** Default is 10 characters (which catches "?" and "N/A" placeholders). Raise to 100 if you want substantive bios.
- **Article handling for directory-name matching.** Default is "prefix" (`The Beatles` stays `The Beatles`). Switch to "strip" if your media platform sorts by content, not the leading article.

The full list of knobs per rule is in the [rules catalog](../reference/rules-catalogue.md).

## Run rules now

Three ways to trigger evaluation:

### Per artist

1. Open the artist's page.
2. Click **Re-evaluate**.

The artist's violations list refreshes. Useful after editing an artist or changing a rule's threshold to confirm the result.

### Bulk

1. Go to the artist list (or a saved view).
2. Click **Bulk actions > Run rules**.
3. Confirm scope.

The bulk run is **incremental by default** -- only artists that have changed since their last evaluation are re-checked. To force a full re-evaluation, toggle the **Force full re-evaluation** option.

### Scheduled

1. Go to **Settings > Rules**.
2. Find the **Scheduled evaluation** dropdown.
3. Pick an interval (5/15/30 min, hourly, every 6 or 12 hours, daily, or disabled).

The scheduler runs the incremental path -- only dirty artists -- so the recurring run stays fast even on large libraries.

## Apply fixes

When violations show up, you can apply fixes one at a time or in bulk.

### Per violation

1. Open the artist's **Violations** tab.
2. Each fixable violation shows a **Fix** button.
3. Click to apply.

The action's outcome -- fixed, dismissed, or still open -- displays in line.

### Fix-all

1. Go to the artist list, scope to "has open violations", or use the global Fix-all action.
2. Click **Fix all**.
3. Stillwater queues every fixable violation in scope. Progress streams into the event banner.

Behind the scenes Fix-all calls the same per-violation fixer in a loop -- same conflict-gate check, same fix outcomes. Only one Fix-all run can be in flight at a time across the install (a second click is rejected with a brief message).

For the full Fix-all behavior, see [rules in core concepts](../core-concepts/rules.md#fix-all).

## When a rule has a conflict-gated chip

If you see an amber chip next to a rule on the Rules tab, the conflict gate is currently blocking writes for that category (image or NFO). Even an auto-mode rule won't apply fixes that would touch disk while the gate is engaged. Resolve the underlying conflict (typically an active platform refresh against a shared library) and the chip clears.

## When a rule is skipped for an artist

A rule can report a third outcome besides passed and failed: **skipped**. A rule is skipped when it needs data that a particular artist does not have, so it genuinely cannot reach a verdict.

The clearest example is duplicate-image detection. It works by comparing image fingerprints. For an artist with a local folder, Stillwater reads the files and fingerprints them on demand. For an artist that exists only through a platform connection (imported from Emby or Jellyfin, with no folder on disk), there are no files to read, so it can only compare the fingerprints already stored for that artist's images. If fewer than two of them have one, there is nothing to compare and the rule is skipped.

Skipped is not a quiet failure, and it is not a pass:

- **A skipped rule does not count toward the health score at all.** The score is calculated over the rules that could actually run, so it never credits an artist for a check that never happened.
- **An artist with nothing to compare still passes.** If an artist has fewer than two images, no duplicate is possible, so the rule is satisfied and passes. That is a real result, not a skip.
- **Skips are surfaced, not swallowed.** The artist health response reports which rules were skipped and why.

If you see a rule skipped across many artists, that is a signal about missing data rather than a problem with the rule. Duplicate detection skipped for platform-only artists usually means their images have not been fingerprinted yet.

## When a run can't save its results

A rule run has two halves: working out what is wrong, and writing down what it
found and fixed. The second half can fail on its own -- most often because the
database is momentarily locked by another operation.

When that happens the run tells you so rather than reporting a clean result:

- **In the browser**, the run reports that its results could not be saved
  instead of showing a violation count.
- **Through the API**, the request fails rather than returning success, and the
  response carries a `persist_failures` count -- how many artists could not be
  fully written.
- **For a background run** (Run Rules across the library, or a single rule
  across all artists), the count appears in the run status you poll, and the
  failure is written to the log.

Treat a non-zero `persist_failures` as "this run's results are not in the
database". Nothing was corrupted and no fix was half-applied -- a run that
cannot record its work leaves the artist marked for re-evaluation, so running
it again is the correct response.

If it keeps happening, that points at the database rather than the rules: check
the log for the underlying write error.

## Don't reach for "disable" first

When a rule is producing too much noise, the right response is usually:

1. Lower the threshold so the rule fires less often, or
2. Switch the rule from auto to manual so violations pile up but don't churn,

before disabling. Disable is for rules that don't apply to your collection at all -- not for rules that are momentarily noisy.

## See also

- [Rules concept](../core-concepts/rules.md)
- [Rules catalog](../reference/rules-catalogue.md) -- every built-in rule and its knobs.
- [Run scans](run-scans.md) -- discovery populates the artist set that rules then evaluate.
