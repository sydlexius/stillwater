package api

import (
	"context"
	"testing"
)

// TestRouterRegistration verifies that all route patterns registered in
// Handler() are compatible with each other. Go 1.22+ panics when two
// patterns overlap ambiguously (e.g. "/{id}/dismiss" vs "/undo/{undoId}").
// This test catches such conflicts at CI time instead of at startup.
func TestRouterRegistration(t *testing.T) {
	r := testRouterForOnboarding(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("route registration panicked: %v", v)
		}
	}()

	_ = r.Handler(ctx)
}
