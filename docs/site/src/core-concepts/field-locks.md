---
description: How locks protect artist metadata from being overwritten by automated refreshes, providers, and connected platforms, and which paths each lock layer covers today.
---

<!-- code: internal/artist/model.go (Locked, LockSource, LockedAt, LockedFields), internal/artist/service.go (Lock, Unlock, SetLockedFields, AddLockedField, RemoveLockedField, IsFieldLocked, validLockSources, RenameDirectory ErrRenameLocked), internal/artist/merge.go (ApplyMetadata buildLockedSet/isLocked per merge strategy, applyTypeConsistency lock restore), internal/api/handlers_refresh.go (a.Locked gate, applyProviderName IsFieldLocked, applyMemberRefresh), internal/api/handlers_discography.go (a.Locked gate), internal/api/handlers_discogs.go + handlers_audiodb.go (IsFieldLocked 409), internal/scanner/scanner.go (NFOImport merge, initial_import lock source), internal/connection/locksync.go (platform lock source), internal/rule/fixer.go + fixers*.go (a.Locked only; no LockedFields check), internal/library/model.go (NFOLockData, #1264) -->
<!-- displaced developer detail: case-insensitive lock comparison, buildLockedSet normalization (drops blanks/whitespace), per-merge-strategy enforcement uniformity, applyTypeConsistency restore mechanics. Belongs in godoc on internal/artist/merge.go. -->
<!-- ACCURACY NOTE (#3037): the persist chokepoint (internal/artist/lockguard.go) NOW enforces per-field locks on the whole-row write every rule fixer ends in, for the ARTISTS-ROW fields only. Provider IDs (musicbrainz_id, audiodb_id, discogs_id, wikidata_id, deezer_id, spotify_id) are deliberately excluded until the hydration step ships -- guarding them un-hydrated would restore an empty ID over a real one. The single-field verbs (UpdateField/ClearField) stay unguarded so history revert and blast-radius restore keep working, and "members" lives in its own table. Do not widen the claim on this page past the artists-row fields until lockGuardedFields actually widens. -->

# Field locks

A **lock** is Stillwater's way of saying "don't touch this." It keeps your manual edits from being overwritten the next time a provider refresh runs or a connected platform pushes its own metadata.

There are two layers of locks, plus a library-wide switch. They are enforced in different places and cover different overwrite paths, so it is worth knowing which one to reach for.

## Layer 1: artist lock (the big switch)

The simplest lock: an entire artist is locked or not. When an artist is locked:

- **Provider refreshes** skip the artist entirely -- the per-artist **Refresh** button, a bulk refresh sweep, and the Discography tab's **Fetch discography**.
- **Rules** skip the artist. A locked artist is not evaluated, and a fix requested against one is declined with "artist is locked."
- **Renaming the artist's folder** on disk is refused.
- **The NFO** Stillwater writes for the artist asks Kodi/Emby/Jellyfin not to overwrite it during their own metadata scans (via `<lockdata>true</lockdata>`).

The lock records where it came from, shown next to the lock chip: **user** (you clicked the toggle, or a bulk action locked the artist), **initial import** (the artist was first discovered with an NFO already carrying the lockdata flag, and only that first discovery sets it, so a rescan does not undo an unlock you made), or **platform** (the scheduled lock sync pulled a change from a connected Emby or Jellyfin server). All three behave identically once set.

Manual edits remain allowed when locked. The artist lock blocks *automated* changes, not your own keyboard.

## Layer 2: field locks (per-field protection)

Sometimes you want most of an artist's metadata to refresh from providers, but two or three fields you have curated by hand should stay put. That is a per-field lock.

You pin a field (biography, sort name, born year, and so on), and the paths listed below leave that field alone. Pinned date and gender fields also survive the post-merge "this value does not apply to this artist type" cleanup, so a born year you pinned on a band stays.

**A field lock is honored by:**

- **Provider refreshes**, including the Name and Sort name written by a language-preference refresh, and the band-member list.
- **Bulk fetch metadata**, which fills empty fields. A pinned field is left empty rather than filled.
- **NFO import during a library scan.**
- **Matching an artist to a Discogs or TheAudioDB entry by name.** If that provider's ID field is pinned, the match is refused and Stillwater tells you to unlock the field first.

**A field lock is honored by the rule engine for artist metadata, and not yet for provider IDs.** Rules used to gate on the whole-artist lock only, so on an artist that was not locked an auto-mode rule could overwrite a field you had pinned. That is now blocked where it matters most: a pinned biography, origin, name, sort name, genres, styles, moods, type, gender, disambiguation, or any of the date fields is restored if a rule tries to change it, and the rest of that same write still lands. The rules that write those fields are the ones that fill an empty biography, replace a placeholder biography, fill a missing origin, and promote a localized name and sort name.

**Provider ID fields are the remaining gap.** Pinning a MusicBrainz, Discogs, Deezer, or Spotify ID does not yet stop the rules that adopt a MusicBrainz ID or backfill missing Discogs, Deezer, and Spotify IDs. If you have curated one of those IDs, the reliable protection today is still the **artist lock**, or leaving that rule in manual mode so nothing fires without your click.

Two other paths are deliberately not covered. A **band member list** lock is honored by a refresh but not by a rule. And your own **explicit edits and undos** are never blocked, which is the next section.

Your own edits are not blocked by a field lock either. Pinning a field hides its inline edit controls so you do not change it by accident, but an explicit edit or an undo from the history view still writes it. That is deliberate, since an undo is how you recover a value an automated write already changed.

Field locks are still the mechanism to reach for whenever you have corrected a value a refresh would otherwise replace. A refresh rewrites most fields with whatever the providers return, and re-identifying an artist runs a refresh against the new identity, so an unpinned correction lasts only until the next one of either.

The [refresh how-to](../how-to/refresh-metadata.md#which-values-a-refresh-replaces) lists which fields a refresh replaces outright, which it can clear, and which it never touches.

Field locks coexist with the artist-level lock. Unlocking the artist does not clear field locks; they are independent layers.

**Where to find the lock controls:** open an artist from the **Artists** sidebar item; lock icons sit next to the Biography heading, each tag-group label (Genres / Styles / Moods), and every field in the Details panel. When an artist has anything pinned, a **Locked fields** card lists each one as a chip you can unlock in a click.

Locking a field is a read-mode action: every field carries a padlock next to its value (gray and open when unlocked), and clicking it toggles the lock. A locked field's padlock turns **amber and closed**, and the read view shows the value without its inline edit control. A field lock does not disable the editor in edit mode: it stops automated writes from the paths listed above, not your own deliberate edit. Hover the demo below to see the same field switch from unlocked to locked:

<div class="sw-hover-swap" tabindex="0" markdown="span">
![Name field unlocked: gray open padlock next to the value 'Johann Sebastian Bach'](../assets/screenshots/artist-field-name-unlocked.png)
![Name field locked: amber closed padlock; 'Johann Sebastian Bach' is still visible, with no inline edit control in the read view](../assets/screenshots/artist-field-name-locked.png){ .sw-hover-after }
<span class="sw-hover-hint">Hover or focus to lock</span>
</div>

For context, here is the whole Details section showing where the lock controls live -- next to the Biography header, each Tags group, and every field row:

![Artist Details section: an Identity list (Name / Sort Name / Type / Disambiguation / Gender / Origin / Formed / Born / Disbanded / Died / Years Active), a Tags block with per-group locks on Genres / Styles / Moods, and a Biography column with a lock icon in its header -- every row and group carries its own open-lock padlock in read mode](../assets/screenshots/artist-overview-fields.jpg)

## Library-wide: NFO lockdata switch

Each library has an opt-in switch: when on, every NFO that library writes asks platforms not to overwrite it, regardless of per-artist lock state. The library-level switch is the right tool when you want the whole library treated as authoritative -- "Stillwater writes the NFOs; nothing else should rewrite them." The per-artist lock is the right tool when most of the library can be platform-managed but a few records are special.

## What about platforms pushing back?

Stillwater's locks govern changes Stillwater itself makes. The NFO lockdata element extends that request to Kodi/Emby/Jellyfin, but only as far as those platforms honor it (Kodi does; Emby and Jellyfin do for most fields).

For the cases where a platform writes anyway, Stillwater has a separate **conflict gate** that watches for incoming writes from connected platforms and pauses Stillwater's own writes when external activity is detected, preventing a tug-of-war where both sides keep overwriting each other. The "image / NFO writes paused" banner in the UI is the gate in action.

The two mechanisms are complementary:

- **Locks** say "do not change this automatically." Set ahead of time, expressing your intent.
- **The conflict gate** says "right now, something else is writing, so pause until it stops." Reactive, transient, and applied regardless of lock state.

## What you don't need to think about

- **Getting the lockdata flag into the NFO.** Locking an artist, or turning on the library-wide switch, does that for you.
- **Conflict-gate coordination with locks.** The two are independent. A locked artist still benefits from gate pauses; an unlocked artist gets the same protection from the gate.
- **Unlocking to make your own correction.** Pinning a field hides its inline controls, but an explicit edit or an undo still goes through.

What you do think about: which artists deserve the artist lock, which fields are worth pinning on artists you leave unlocked, whether any rule that writes those fields should be left in manual mode, and whether the per-library lockdata switch is the right shape for your collection. The [edit-artist how-to](../how-to/edit-artist.md) walks through setting locks in the UI.
