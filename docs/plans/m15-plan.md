# Milestone 15 -- Code Health & Audits

## Goal

Audit the codebase for dead code, concurrency hazards, and memory leaks introduced
by Milestone 14 (scraper package, DuckDuckGo and Deezer providers, parallel image
probing, new API endpoints, and templ components).

## Acceptance Criteria

- [ ] All clearly unused/dead Go code and assets identified and removed (#120)
- [ ] All known concurrency hazards documented and patched (#121)
- [ ] All HTTP body closure, file handle, and goroutine leak risks addressed (#122)
- [ ] `go test -race ./...` passes with zero races
- [ ] `golangci-lint run ./...` passes with zero new lints
- [ ] Each issue closed with a summary comment linking the PR

## Dependency Map

```
#121 (Concurrency) --> can start immediately
#122 (Memory Leaks) --> can start immediately (overlaps with #121)
#120 (Dead Code)    --> after #121 and #122 complete
```

## Checklist

### Issue #121 -- Concurrency Audit

- [x] Read all concurrency-critical files
- [x] Fix webhook/dispatcher.go goroutine cap (semaphore, max 20)
- [x] Confirm event/bus.go Stop() idempotency -- already correct
- [x] Confirm ratelimit.go cleanup goroutine -- already correct
- [x] Confirm scraper/executor.go -- sequential, no goroutines
- [x] Confirm handlers_image.go probeImageDimensions -- semaphore already present
- [x] PR opened (#136)
- [ ] CI passing
- [ ] PR merged
- [x] Issue #121 summary posted

### Issue #122 -- Memory Leak Audit

- [x] Replace manual read loop in musicbrainz.go with io.ReadAll(io.LimitReader)
- [x] Add body drain before early returns in musicbrainz.go
- [x] Add io.LimitReader + drain early returns in fanarttv.go
- [x] Add io.LimitReader + drain early returns in audiodb.go
- [x] Add io.LimitReader + drain early returns in discogs.go
- [x] Add io.LimitReader + drain early returns in lastfm.go
- [x] Add io.LimitReader + drain early return in wikidata.go
- [x] Add drain early returns in deezer.go (already had io.LimitReader)
- [x] Add drain early return in rule/fixers.go fetchImageURL
- [x] Confirm duckduckgo.go -- already clean
- [x] Confirm connection clients -- already drain on error paths
- [x] Confirm filesystem/atomic.go -- clean
- [x] PR opened (#137)
- [ ] CI passing
- [ ] PR merged
- [x] Issue #122 summary posted

### Issue #120 -- Dead Code Audit

- [x] Run golangci-lint with unused/unparam flags -- only one issue (deezer_test.go formatting)
- [x] Run go mod tidy -- no changes
- [x] Audit orphaned template/static assets -- all active
- [x] Fix deezer_test.go trailing blank line (gofmt)
- [x] PR opened (#138)
- [ ] CI passing
- [ ] PR merged
- [x] Issue #120 summary posted

### Cleanup

- [ ] Delete feature branches
- [ ] Close milestone 15 umbrella (if one exists)
- [ ] Delete docs/plans/m15-plan.md, commit to main

## UAT / Merge Order

1. PR for #121 (base: main) -- concurrency fixes
2. PR for #122 (base: main) -- memory leak fixes
3. PR for #120 (base: main) -- dead code removal

## Notes

### 2026-02-23: Audit findings

**#121 confirmed issues:**
- `webhook/dispatcher.go`: `HandleEvent()` spawns one goroutine per matching webhook
  with no semaphore. Under load with many webhooks, this is unbounded. Fix: add
  `make(chan struct{}, 20)` semaphore.
- `event/bus.go`: `Stop()` already has idempotent guard via `b.stopped` under
  `b.mu.Lock()`. No fix needed.
- `ratelimit.go`: Cleanup goroutine correctly exits on `ctx.Done()`. No fix needed.
- `scraper/executor.go`: All sequential, no goroutines. No fix needed.
- `handlers_image.go:probeImageDimensions`: Semaphore of 5 already present. Clean.

**#122 confirmed issues:**
- `musicbrainz.go:doRequest()`: Manual read loop with no size limit; early returns
  at non-OK statuses leave body undrained.
- `fanarttv.go:GetImages()`: `io.ReadAll` without `io.LimitReader`; early returns
  for 404 and non-OK statuses leave body undrained.
- `audiodb.go:fetchArtists()`: `io.ReadAll` without `io.LimitReader`; early return
  for non-OK status leaves body undrained.
- `discogs.go:doRequest()`: `io.ReadAll` without `io.LimitReader`; early returns
  for 404, 401/403, non-OK leave body undrained.
- `lastfm.go:doRequest()`: `io.ReadAll` without `io.LimitReader`; early returns
  for 401/403, non-OK leave body undrained.
- `wikidata.go:executeSPARQL()`: `io.ReadAll` without `io.LimitReader`; early return
  for non-OK status leaves body undrained.
- `deezer.go:doRequest()`: Has `io.LimitReader`. Early returns for 404, 429, other
  leave body undrained (drain added).
- `rule/fixers.go:fetchImageURL()`: `io.LimitReader` present, but early return at
  non-OK status leaves body undrained.
- All connection clients (emby, jellyfin, lidarr): Clean -- read body on error paths.
- `duckduckgo.go`: Clean -- `io.LimitReader` on all paths.
- `filesystem/atomic.go`: Clean -- file handles correctly closed.
