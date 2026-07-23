---
description: How locks protect artist metadata from being overwritten by automated refreshes, providers, and connected platforms.
---

<!-- code: internal/artist/model.go (Locked, LockSource, LockedAt, LockedFields), internal/artist/service.go (Lock, Unlock, SetLockedFields, AddLockedField, RemoveLockedField, validLockSources), internal/artist/merge.go (LockedFields handling per merge strategy, FilterDatesByArtistType bypass for locked dates), internal/library/model.go (NFOLockData, #1264) -->
<!-- displaced developer detail: case-insensitive lock comparison, buildLockedSet normalization (drops blanks/whitespace), per-merge-strategy enforcement uniformity, FilterDatesByArtistType bypass mechanics. Belongs in godoc on internal/artist/merge.go. -->

# Field locks

A **lock** is Stillwater's way of saying "don't touch this." It keeps your manual edits from being overwritten the next time a provider refresh runs, a rule fixer fires, or a connected platform pushes its own metadata.

There are two layers of locks, plus a library-wide switch. Each protects against different overwrite paths.

## Layer 1: artist lock (the big switch)

The simplest lock: an entire artist is locked or not. When an artist is locked:

- **Provider refreshes** skip the artist entirely.
- **Rule fixers** see the lock and decline to apply changes.
- **The NFO** Stillwater writes for the artist asks Kodi/Emby/Jellyfin not to overwrite it during their own metadata scans (via `<lockdata>true</lockdata>`).

The lock has two sources:

- **User** -- you clicked the lock toggle. Stays locked until you unlock it.
- **Imported** -- the lock was inferred from an existing NFO file's lockdata flag. Treated identically by Stillwater; the source is just metadata for "where did this lock come from."

Manual edits remain allowed when locked -- the lock blocks *automated* overwrites, not your own keyboard.

## Layer 2: field locks (per-field protection)

Sometimes you want most of an artist's metadata to refresh from providers, but two or three fields you've curated by hand should stay put. That's a per-field lock.

You pin a field (biography, sort name, born year, ...) and Stillwater's refresh, fill-empty, and NFO-import paths all skip that field. Pinned date fields also survive the post-merge "this date doesn't apply to this artist type" cleanup -- if you've pinned a born year on a band, it stays.

Field locks coexist with the artist-level lock. Unlocking the artist doesn't clear field locks; they're independent layers.

**Where to find the lock controls:** open an artist from the **Artists** sidebar item; lock icons sit next to the Biography heading, each tag-group label (Genres / Styles / Moods), and every field in the Details panel.

Locking a field is a read-mode action: every field carries a padlock next to its value (gray and open when unlocked), and clicking it toggles the lock. A locked field's padlock turns **amber and closed**, and its inline editing controls stay hidden even in edit mode, so the value can't be changed by accident. Hover the demo below to see the same field switch from unlocked to locked:

<div class="sw-hover-swap" tabindex="0" markdown="span">
![Name field unlocked: gray open padlock next to the value 'Johann Sebastian Bach'](../assets/screenshots/artist-field-name-unlocked.png)
![Name field locked: amber closed padlock; 'Johann Sebastian Bach' is still visible but no longer editable](../assets/screenshots/artist-field-name-locked.png){ .sw-hover-after }
<span class="sw-hover-hint">Hover or focus to lock</span>
</div>

For context, here's the whole Details section showing where the lock controls live -- next to the Biography header, each Tags group, and every field row:

![Artist Details section: an Identity list (Name / Sort Name / Type / Disambiguation / Gender / Origin / Formed / Born / Disbanded / Died / Years Active), a Tags block with per-group locks on Genres / Styles / Moods, and a Biography column with a lock icon in its header -- every row and group carries its own open-lock padlock in read mode](../assets/screenshots/artist-overview-fields.jpg)

## Library-wide: NFO lockdata switch

Each library has an opt-in switch: when on, **every** NFO that library writes asks platforms not to overwrite it, regardless of per-artist lock state. The library-level switch is the right tool when you want the whole library treated as authoritative -- "Stillwater writes the NFOs; nothing else should rewrite them." The per-artist lock is the right tool when most of the library can be platform-managed but a few records are special.

## What about platforms pushing back?

Stillwater's locks protect against Stillwater itself making automated changes. The NFO lockdata element extends that protection to Kodi/Emby/Jellyfin -- but only when those platforms honor it (Kodi does; Emby and Jellyfin do for most fields).

For the cases where a platform writes anyway, Stillwater has a separate **conflict gate** that watches for incoming writes from connected platforms and pauses Stillwater's own writes when external activity is detected -- preventing a tug-of-war where both sides keep overwriting each other. The "image / NFO writes paused" banner in the UI is the gate in action.

The two mechanisms are complementary:

- **Locks** say "this should never change automatically." Set ahead of time, expressing user intent.
- **The conflict gate** says "right now, something else is writing -- pause until it stops." Reactive, transient, and applies regardless of lock state.

## What you don't need to think about

- **Where the lock is enforced.** Every code path that auto-modifies an artist consults the locks. You set them; the rest of the system honors them.
- **NFO output details.** Locking an artist gets the lockdata flag into the NFO automatically.
- **Conflict-gate coordination with locks.** Independent. A locked artist still benefits from gate pauses; an unlocked artist still gets the same protection from the gate.

What you do think about: which artists deserve a lock, which fields you want pinned even on unlocked artists, and whether the per-library lockdata switch is the right shape for your collection. The [edit-artist how-to](../how-to/edit-artist.md) walks through setting locks in the UI.
