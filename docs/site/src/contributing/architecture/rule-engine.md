---
description: How the Stillwater rule engine detects violations, dispatches fixes, and persists results.
---

# Rule engine

The rule engine evaluates a configurable set of rules against every artist in
the library and records violations. Each rule has three modes: disabled,
manual, and auto. In auto mode the engine fixes violations without user
intervention; in manual mode it presents candidate fixes and waits for user
approval.

## Fix-all dispatch flow

The diagram below shows the path taken when a user (or the scheduler) triggers
a full rule run. The orchestration entry point is
`internal/rule.Service.RunAllScoped`.

Only one full rule run executes at a time. A second start attempt while a run
is in flight receives a 409 Conflict. An in-memory, mutex-protected progress
tracker coordinates the single active run and surfaces live progress to the UI
over SSE.

```mermaid
flowchart TD
    Trigger["Run Rules triggered\n(HTTP handler or scheduler)"]
    Scope{"Scope?"}
    Incremental["Collect dirty artists only\n(dirty_subscriber tracks changes)"]
    All["Collect all eligible artists\n(paginated, page size 200)"]
    PassCtx["Build shared provider cache\nfor the whole pass (LRU, 500 entries)"]
    EvalLoop["Evaluate each artist\nagainst every enabled rule"]
    CheckEnabled{"Rule enabled?"}
    SkipRule["Skip rule"]
    RunChecker["Run checker function\n(returns nil or Violation)"]
    NoViolation["Record pass\nincrement RulesPassed"]
    AutoMode{"Automation mode?"}
    ManualMode["Store violation for\nuser review"]
    FixAttempt["Attempt automatic fix"]
    ConflictGate{"Conflict gate\nallow write?"}
    Blocked["Record blocked\nlog warning"]
    PersistFix["Update artist in DB\nwrite NFO / image to disk"]
    PublishFix["Publish metadata and images\nto connected platforms"]
    MarkDirty["Mark artist dirty\nfor next incremental run"]
    StoreResult["Store EvaluationResult\n(violation rows)"]
    BusEvent["Publish operation.progress\nand bulk.completed events"]

    Trigger --> Scope
    Scope -->|incremental| Incremental
    Scope -->|full| All
    Incremental --> PassCtx
    All --> PassCtx
    PassCtx --> EvalLoop
    EvalLoop --> CheckEnabled
    CheckEnabled -->|no| SkipRule
    CheckEnabled -->|yes| RunChecker
    RunChecker -->|pass| NoViolation
    RunChecker -->|violation| AutoMode
    AutoMode -->|manual| ManualMode
    AutoMode -->|auto| FixAttempt
    FixAttempt --> ConflictGate
    ConflictGate -->|blocked| Blocked
    ConflictGate -->|allowed| PersistFix
    PersistFix --> PublishFix
    PersistFix --> MarkDirty
    NoViolation --> StoreResult
    ManualMode --> StoreResult
    Blocked --> StoreResult
    MarkDirty --> StoreResult
    PublishFix --> StoreResult
    StoreResult --> BusEvent
```

## Deferred-resolved rows and the persistence chain

Each evaluation run produces an `EvaluationResult` per artist (defined in
`internal/rule/model.go`). The service writes violation rows to the database
via `internal/rule.Service.StoreViolations`, keyed by `(artist_id, rule_id)`.
A previous violation row with `fixed=true` or `dismissed=true` is considered
resolved; a new row for the same pair supersedes it.

The `FixResult` struct (returned by each fixer) has three significant states:

| State | Meaning |
|---|---|
| `Fixed: true` | Fixer successfully applied the change; the violation is cleared |
| `Dismissed: true` | Violation is valid but no fix is possible (e.g. artist has no MBID); recorded so the user is not re-notified |
| Neither | Fixer attempted a fix but failed; violation stays open for retry |

After a successful fix, the fixer or the pipeline calls
`Publisher.PublishMetadata` or `Publisher.SyncImageToPlatforms` (both in
`internal/publish`) to propagate changes to connected Emby and Jellyfin
instances.

## Pass-level provider cache

During a run-all pass, many artists may share the same MusicBrainz ID or
need the same provider payload. Fetching it repeatedly for each artist wastes
network round-trips. `PassContext` (in `internal/rule/pass_context.go`) holds
an LRU cache (default 500 entries) that is shared across all artist
evaluations in a single `RunAllScoped` call. When a fixer writes changes for
an artist it must call `PassContext.Invalidate` for that artist so subsequent
re-evaluations see fresh data.

## Where to look

| Topic | File |
|---|---|
| Engine wiring and `Evaluate` | `internal/rule/engine.go` |
| `RunAllScoped`, `RunScope`, `FixResult` | `internal/rule/fixer.go` |
| `Rule`, `Violation`, `RuleConfig` model types | `internal/rule/model.go` |
| Checker registration and built-in checkers | `internal/rule/checkers.go` |
| Individual checkers | `internal/rule/checkers_*.go` |
| Fixer implementations | `internal/rule/fixers.go`, `internal/rule/fixers_*.go` |
| Bulk executor (fix-all job lifecycle) | `internal/rule/bulk_executor.go` |
| Pass-level provider cache | `internal/rule/pass_context.go` |
| Violation persistence | `internal/rule/sqlite_rule_results.go` |
| Dirty-artist tracking | `internal/rule/dirty_subscriber.go` |

See also [Architecture decisions](../architecture-decisions.md) for the ADRs
that shaped the rule engine's incremental-evaluation and automation-mode design.
