package rule

import (
	"database/sql"
	"log/slog"
	"slices"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// twoCandidates is the stored candidate list every test here parks a
// pending_choice row on. Two entries, because the whole point of the status is
// that a human has to pick between them.
func twoCandidates() []ImageCandidate {
	return []ImageCandidate{
		{URL: "https://example.test/thumb-a.jpg", Width: 1000, Height: 1000, Source: "fanart", ImageType: "thumb"},
		{URL: "https://example.test/thumb-b.jpg", Width: 500, Height: 500, Source: "fanart", ImageType: "thumb"},
	}
}

// storedCandidates reads the candidates column straight from the table so the
// assertion cannot be satisfied by a service-layer default. The returned slice
// is in stored JSON-array order, which is stable; nothing here depends on Go
// map iteration order.
func storedCandidates(t *testing.T, db *sql.DB, artistID, ruleID string) []ImageCandidate {
	t.Helper()
	var raw string
	err := db.QueryRowContext(t.Context(),
		`SELECT candidates FROM rule_violations WHERE artist_id = ? AND rule_id = ?`,
		artistID, ruleID).Scan(&raw)
	if err != nil {
		t.Fatalf("reading candidates for (%s, %s): %v", artistID, ruleID, err)
	}
	return unmarshalCandidates(raw)
}

// candidateURLs projects a candidate list down to the field the operator
// actually chooses between, for a readable diff on failure.
func candidateURLs(cs []ImageCandidate) []string {
	urls := make([]string, 0, len(cs))
	for _, c := range cs {
		urls = append(urls, c.URL)
	}
	return urls
}

// upsertPendingChoice parks a pending_choice violation with candidates on
// (ruleID, artist) and asserts the seed genuinely landed. A silently failed
// seed would make every test below pass having proven nothing.
func upsertPendingChoice(t *testing.T, db *sql.DB, svc *Service, a *artist.Artist, ruleID string, cs []ImageCandidate) {
	t.Helper()
	if err := svc.UpsertViolation(t.Context(), &RuleViolation{
		RuleID:     ruleID,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "seeded pending choice",
		Fixable:    true,
		Status:     ViolationStatusPendingChoice,
		Candidates: cs,
	}); err != nil {
		t.Fatalf("seeding pending_choice violation: %v", err)
	}
	if got := violationStatus(t, db, a.ID, ruleID); got != ViolationStatusPendingChoice {
		t.Fatalf("precondition: seeded row status = %q, want %q", got, ViolationStatusPendingChoice)
	}
	if got := storedCandidates(t, db, a.ID, ruleID); len(got) != len(cs) {
		t.Fatalf("precondition: seeded row has %d candidates, want %d", len(got), len(cs))
	}
}

// TestHealthSubscriber_PreservesPendingChoiceAndCandidates drives the real path
// from issue #2969: an ArtistUpdated event runs evaluateArtist, which writes a
// compensating fail row built from a PARTIAL RuleViolation (status open, no
// candidates). Before the guard that write downgraded the parked operator
// decision to open and replaced its candidate list with an empty array.
//
// Mutants this kills: removing either the pending_choice branch of the status
// CASE or the candidates CASE in UpsertViolation's ON CONFLICT clause.
func TestHealthSubscriber_PreservesPendingChoiceAndCandidates(t *testing.T) {
	db := setupSubscriberTestDB(t)
	ctx := t.Context()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rule defaults: %v", err)
	}
	engine := NewEngine(ruleSvc, db, nil, nil, slog.Default())

	a := &artist.Artist{Name: "Pending Choice Artist", SortName: "Pending Choice Artist", Path: "/music/pending-choice"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Precondition: the rule must be CONSIDERED and VIOLATED on this artist.
	// An unevaluated rule is left alone trivially, so without this the test
	// would pass against a subscriber that never wrote anything at all.
	res, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating artist: %v", err)
	}
	if !slices.Contains(res.RulesConsidered, RuleThumbExists) {
		t.Fatalf("precondition: %s not in RulesConsidered %v", RuleThumbExists, res.RulesConsidered)
	}
	var violated bool
	for i := range res.Violations {
		if res.Violations[i].RuleID == RuleThumbExists {
			violated = true
		}
	}
	if !violated {
		t.Fatalf("precondition: %s did not report a violation; the compensating fail write "+
			"under test only runs for a violated rule", RuleThumbExists)
	}

	want := twoCandidates()
	upsertPendingChoice(t, db, ruleSvc, a, RuleThumbExists, want)

	NewHealthSubscriber(engine, artistSvc, slog.Default()).evaluateArtist(ctx, a.ID)

	if got := violationStatus(t, db, a.ID, RuleThumbExists); got != ViolationStatusPendingChoice {
		t.Errorf("status after the subscriber's compensating fail write = %q, want %q: an "+
			"automated re-evaluation must not overrule a parked human decision (#1107)",
			got, ViolationStatusPendingChoice)
	}
	got := storedCandidates(t, db, a.ID, RuleThumbExists)
	if len(got) != len(want) {
		t.Fatalf("candidates after the compensating fail write = %v, want %v (the write carries "+
			"none, which must mean 'no data offered', not 'clear the list')",
			candidateURLs(got), candidateURLs(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestUpsertViolation_DecisionStateGuards is the positive control for the two
// guards. A guard that simply froze the row would pass the test above, so each
// case below asserts a transition that MUST still work, alongside the two that
// must not happen.
func TestUpsertViolation_DecisionStateGuards(t *testing.T) {
	seeded := twoCandidates()
	replacement := []ImageCandidate{
		{URL: "https://example.test/thumb-c.jpg", Width: 2000, Height: 2000, Source: "tadb", ImageType: "thumb"},
	}

	tests := []struct {
		name           string
		storedStatus   string
		incoming       *RuleViolation
		wantStatus     string
		wantCandidates []ImageCandidate
	}{
		{
			// The #2969 defect itself, at the SQL boundary.
			name:         "subscriber fail write cannot downgrade pending_choice",
			storedStatus: ViolationStatusPendingChoice,
			incoming: &RuleViolation{Status: ViolationStatusOpen,
				Message: "still failing"},
			wantStatus:     ViolationStatusPendingChoice,
			wantCandidates: seeded,
		},
		{
			// finalizeResolvedRows in fixer.go: a fix succeeded, so the choice
			// is moot. This must still land or a fixed row is stuck forever.
			name:           "a successful fix still resolves a pending_choice row",
			storedStatus:   ViolationStatusPendingChoice,
			incoming:       &RuleViolation{Status: ViolationStatusResolved, Message: "fixed"},
			wantStatus:     ViolationStatusResolved,
			wantCandidates: seeded,
		},
		{
			// Re-discovery found a different candidate set. "Do not overwrite
			// with nothing" is not "never overwrite".
			name:         "fresh candidates still replace the stored list",
			storedStatus: ViolationStatusPendingChoice,
			incoming: &RuleViolation{Status: ViolationStatusPendingChoice,
				Message: "re-discovered", Candidates: replacement},
			wantStatus:     ViolationStatusPendingChoice,
			wantCandidates: replacement,
		},
		{
			// The guard is scoped to pending_choice: an ordinary open row keeps
			// taking every incoming status, including resolved and open.
			name:           "an open row still takes an incoming open write",
			storedStatus:   ViolationStatusOpen,
			incoming:       &RuleViolation{Status: ViolationStatusOpen, Message: "still failing"},
			wantStatus:     ViolationStatusOpen,
			wantCandidates: seeded,
		},
		{
			// #1107's existing guard, re-asserted because the status CASE was
			// restructured: dismissed stays terminal and now keeps its
			// candidates too, which it did not before.
			name:           "dismissed stays terminal and keeps its candidates",
			storedStatus:   ViolationStatusDismissed,
			incoming:       &RuleViolation{Status: ViolationStatusOpen, Message: "still failing"},
			wantStatus:     ViolationStatusDismissed,
			wantCandidates: seeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := setupSubscriberTestDB(t)
			ctx := t.Context()
			artistSvc := artist.NewService(db)
			ruleSvc := NewService(db)
			a := &artist.Artist{Name: "Guard Artist", SortName: "Guard Artist", Path: "/music/guard"}
			if err := artistSvc.Create(ctx, a); err != nil {
				t.Fatalf("creating artist: %v", err)
			}

			// Every case seeds through pending_choice so the stored row always
			// starts with a non-empty candidate list; the non-pending cases then
			// move it to their starting status the way production does.
			upsertPendingChoice(t, db, ruleSvc, a, RuleThumbExists, seeded)
			switch tc.storedStatus {
			case ViolationStatusPendingChoice:
			case ViolationStatusOpen:
				setViolationStatusDirect(t, db, a.ID, RuleThumbExists, ViolationStatusOpen)
			case ViolationStatusDismissed:
				setViolationStatusDirect(t, db, a.ID, RuleThumbExists, ViolationStatusDismissed)
			default:
				t.Fatalf("unhandled stored status %q", tc.storedStatus)
			}
			if got := violationStatus(t, db, a.ID, RuleThumbExists); got != tc.storedStatus {
				t.Fatalf("precondition: stored status = %q, want %q", got, tc.storedStatus)
			}
			if got := storedCandidates(t, db, a.ID, RuleThumbExists); len(got) != len(seeded) {
				t.Fatalf("precondition: stored candidates = %v, want %d entries",
					candidateURLs(got), len(seeded))
			}

			tc.incoming.RuleID = RuleThumbExists
			tc.incoming.ArtistID = a.ID
			tc.incoming.ArtistName = a.Name
			tc.incoming.Severity = "error"
			tc.incoming.Fixable = true
			if err := ruleSvc.UpsertViolation(ctx, tc.incoming); err != nil {
				t.Fatalf("upserting the incoming violation: %v", err)
			}

			if got := violationStatus(t, db, a.ID, RuleThumbExists); got != tc.wantStatus {
				t.Errorf("status = %q, want %q", got, tc.wantStatus)
			}
			got := storedCandidates(t, db, a.ID, RuleThumbExists)
			if len(got) != len(tc.wantCandidates) {
				t.Fatalf("candidates = %v, want %v", candidateURLs(got), candidateURLs(tc.wantCandidates))
			}
			for i := range tc.wantCandidates {
				if got[i] != tc.wantCandidates[i] {
					t.Errorf("candidate %d = %+v, want %+v", i, got[i], tc.wantCandidates[i])
				}
			}
		})
	}
}

// setViolationStatusDirect moves a seeded row to a starting status with a plain
// UPDATE, mirroring DismissViolation and friends -- those production writers do
// NOT go through the ON CONFLICT clause, so seeding this way keeps the fixture
// honest about how the row got into that state.
func setViolationStatusDirect(t *testing.T, db *sql.DB, artistID, ruleID, status string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		`UPDATE rule_violations SET status = ? WHERE artist_id = ? AND rule_id = ?`,
		status, artistID, ruleID); err != nil {
		t.Fatalf("setting stored status to %q: %v", status, err)
	}
}
