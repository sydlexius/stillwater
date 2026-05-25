---
description: Search across every settings tab to find a specific control without clicking through tabs.
---

<!-- code: web/templates/settings.templ (BuildSettingsSearchIndex, settingsSearchScript, settingsTabBar). -->

# Find a setting

Settings spans eleven tabs with dozens of controls each. The search box at the top of the page filters tabs and individual controls by label or help text, so you can jump straight to what you need.

## Search the page

1. Open **Settings**.
2. Click into the search box above the tab bar, or press **/** from anywhere on the page.
3. Type a few characters. As you type:
    - Tabs without a matching control grey out.
    - Tabs that match show a small badge with the number of matches.
    - Individual matched controls inside the active tab gain a blue outline.
4. Press **Enter** to jump to the first matched control. The matching tab activates and the control briefly pulses to draw your eye.

## Clear the search

Empty the input (or press **Backspace** until it is empty). All tabs return to normal, all control outlines disappear.

The search query is ephemeral by design -- it clears on page reload. There is no setting to persist it.

## Tips

- **Search the help text too.** The index covers each control's help-icon tooltip, not just its label. Searching for `cache` finds the Image Cache controls even when the on-screen heading does not use that word.
- **The keyboard shortcut respects focus.** If you are typing in a text field, pressing `/` types a literal slash. The shortcut only fires when nothing else has keyboard focus.
- **Locale-aware.** Search labels follow your active UI language; switching the language updates the index without reloading the page on the next render.

## What is not searchable

The index is a curated set of headings and controls (about 70 entries spanning every tab) rather than every line of help-text content. If a specific knob does not match a likely keyword, browse to its tab manually and use the section's help icon for context. To request additional search coverage for a control you reach for often, open an issue.
