---
description: Turn rules on and off, switch between manual and auto modes, tune thresholds.
---

<!-- code: web/templates/settings.templ rules tab (rules grouped by category, automation mode toggles, conflict-gated chips), internal/api/router.go (PUT /api/v1/rules/{id}, /toggle, /run), internal/rule/service.go (defaults). -->

# Enable and configure rules

Stillwater ships with 22 built-in rules. Most are enabled out of the box; some are opinionated and start disabled. This page covers turning them on and off, picking the right automation mode, tuning thresholds, and scheduling recurring evaluation.

For the *concept* (the three states, fix-all, the conflict gate), see [rules in core concepts](../core-concepts/rules.md). For the full per-rule reference, see [rules catalogue](../reference/rules-catalogue.md).

## Turn a rule on or off

1. Go to **Settings > Rules**.
2. Find the rule -- they're grouped by category (NFO, Image, Metadata).
3. Toggle the **Enabled** switch.

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

The full list of knobs per rule is in the [rules catalogue](../reference/rules-catalogue.md).

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

## Don't reach for "disable" first

When a rule is producing too much noise, the right response is usually:

1. Lower the threshold so the rule fires less often, or
2. Switch the rule from auto to manual so violations pile up but don't churn,

before disabling. Disable is for rules that don't apply to your collection at all -- not for rules that are momentarily noisy.

## See also

- [Rules concept](../core-concepts/rules.md)
- [Rules catalogue](../reference/rules-catalogue.md) -- every built-in rule and its knobs.
- [Run scans](run-scans.md) -- discovery is what populates the artists rules then check.
