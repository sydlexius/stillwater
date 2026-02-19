# M7: Integrations (v0.7.0)

## Goal

Connect Stillwater to external services: Emby, Jellyfin, and Lidarr for library sync and scan triggers. Implement webhook notifications for event dispatching.

## Prerequisites

- M2 complete (artist model)
- M5 complete (rule engine events to notify about)

## Issues

| # | Title | Mode | Model |
|---|-------|------|-------|
| 24 | Emby integration: connection and library scan trigger | plan | sonnet |
| 25 | Jellyfin integration: connection and library scan trigger | direct | sonnet |
| 26 | Lidarr integration: artist sync and conflict detection | plan | sonnet |
| 27 | Webhook notifications (Discord, Gotify, generic HTTP) | plan | sonnet |
| 28 | Artist alias management | direct | sonnet |

## Implementation Order

### Step 1: Connection Management Foundation

**Package:** `internal/connection/`

Before implementing specific integrations, establish the connection management layer.

1. Define connection model (using existing `connections` table):

```go
type Connection struct {
    ID        string
    Name      string
    Type      string    // "emby", "jellyfin", "lidarr"
    URL       string
    APIKey    string    // encrypted at rest
    Enabled   bool
    Status    string    // "unknown", "healthy", "unhealthy"
    LastCheck time.Time
    Config    map[string]string  // type-specific config (e.g., library ID)
}
```

2. Connection repository (CRUD):
   - API keys encrypted/decrypted via encryption package
   - `internal/database/connection_repo.go`

3. Connection API endpoints (wire existing placeholders in router):
   - `GET /api/v1/connections` -- list all connections
   - `POST /api/v1/connections` -- create new connection
   - `PUT /api/v1/connections/{id}` -- update connection
   - `DELETE /api/v1/connections/{id}` -- delete connection
   - `POST /api/v1/connections/{id}/test` -- test connectivity

4. Connection management UI:
   - List page with status indicators
   - Add/edit modal with type selector
   - Test button with live feedback
   - API key input (masked after save)

### Step 2: Emby Integration (#24)

**Package:** `internal/connection/emby/`

1. Emby client:
   - `TestConnection(ctx) error` -- verify URL + API key
   - `GetArtists(ctx, libraryID) ([]ArtistInfo, error)` -- pull artist list
   - `TriggerLibraryScan(ctx) error` -- POST /Library/Refresh
   - `TriggerArtistRefresh(ctx, artistID) error` -- refresh specific artist

2. Authentication: API key in `X-Emby-Token` header

3. Library selection: allow user to pick which music library to sync from

4. Auto-trigger: optionally trigger Emby scan after metadata/image changes (configurable in settings)

### Step 3: Jellyfin Integration (#25)

**Package:** `internal/connection/jellyfin/`

1. Jellyfin client (very similar to Emby, shared base where possible):
   - Same operations: test, get artists, trigger scan, trigger refresh
   - Authentication: API key in `Authorization: MediaBrowser Token="..."` header
   - Endpoint differences from Emby

2. Reuse connection management UI (type selector distinguishes Emby vs Jellyfin)

### Step 4: Lidarr Integration (#26)

**Package:** `internal/connection/lidarr/`

1. Lidarr client:
   - `TestConnection(ctx) error`
   - `GetArtists(ctx) ([]LidarrArtist, error)` -- pull artist list with paths and MBIDs
   - `GetMetadataProfiles(ctx) ([]MetadataProfile, error)` -- check what Lidarr writes
   - `TriggerArtistRefresh(ctx, artistID) error`

2. Conflict detection:
   - Check if Lidarr's metadata profile includes NFO writing
   - If so, warn the user that both Stillwater and Lidarr may write to the same artist.nfo
   - Display warning banner in UI when conflict detected
   - Suggest disabling NFO writing in one of the two tools

3. Artist sync:
   - Match Lidarr artists to Stillwater artists by MBID or path
   - Import MBIDs from Lidarr for artists missing them
   - Show sync status in artist list

### Step 5: Webhook Notifications (#27)

**Package:** `internal/notification/`

1. Define webhook model (using existing `webhooks` table):

```go
type Webhook struct {
    ID      string
    Name    string
    Type    string   // "discord", "slack", "gotify", "http"
    URL     string
    Events  []string // which events trigger this webhook
    Enabled bool
}

type Event struct {
    Type      string
    Timestamp time.Time
    Data      map[string]any
}
```

2. Event types:
   - `artist.new` -- new artist discovered by scanner
   - `metadata.fixed` -- metadata auto-fixed by rule engine
   - `review.needed` -- manual review required (ambiguous match)
   - `rule.violation` -- rule violation detected
   - `bulk.completed` -- bulk operation finished
   - `scan.completed` -- library scan finished

3. Dispatcher adapters:
   - Discord: format as embed, POST to webhook URL
   - Slack: format as Block Kit message
   - Gotify: POST to Gotify server API
   - Generic HTTP: POST JSON payload to URL

4. Event bus:
   - Publish events from rule engine, scanner, bulk operations
   - Dispatcher consumes events and sends to configured webhooks
   - Async dispatch (do not block main operations)
   - Retry on failure (configurable retry count)

5. Webhook management UI:
   - CRUD for webhooks
   - Event type checkboxes per webhook
   - Test button (sends sample event)

6. API endpoints:
   - `GET /api/v1/webhooks`
   - `POST /api/v1/webhooks`
   - `PUT /api/v1/webhooks/{id}`
   - `DELETE /api/v1/webhooks/{id}`
   - `POST /api/v1/webhooks/{id}/test`

### Step 6: Artist Alias Management (#28)

**Package:** `internal/artist/`

1. Alias CRUD (using existing `artist_aliases` table):
   - Add/remove aliases for an artist
   - Import aliases from MusicBrainz

2. Multi-alias search:
   - When searching providers, search all known aliases
   - Merge results and deduplicate

3. Merge suggestions:
   - When scanner finds directories that might be the same artist
   - Compare by name similarity, MBID, aliases
   - UI prompt to merge or dismiss

4. Artist detail UI:
   - Show aliases below primary name
   - Add/remove alias buttons

## Key Design Decisions

- **Emby and Jellyfin share similar APIs:** Abstract common operations where possible, but keep separate packages since the APIs diverge in authentication and some endpoints.
- **Lidarr conflict detection is advisory:** Stillwater warns but does not prevent conflicting configurations. The user decides which tool manages NFO files.
- **Webhook dispatch is fire-and-forget with retry:** Webhook failures do not affect core operations. Failed deliveries are retried up to 3 times with exponential backoff.
- **Event bus is in-process:** No external message queue needed. A simple Go channel-based event bus is sufficient for the expected event volume.

## Verification

- [ ] Emby connection test works with valid credentials
- [ ] Jellyfin connection test works
- [ ] Lidarr artist sync imports MBIDs correctly
- [ ] Lidarr conflict detection warns when NFO writing is enabled
- [ ] Library scan triggers fire after metadata/image changes
- [ ] Webhook notifications delivered to Discord, Gotify
- [ ] Test webhook button sends sample event
- [ ] Artist aliases display and search correctly
- [ ] Merge suggestions appear for duplicate artists
- [ ] `make test` and `make lint` pass
- [ ] Bruno collection updated with connection and webhook endpoints
