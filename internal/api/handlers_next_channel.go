package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// checkNextChannel writes 404 and returns false when the resolved UX channel is
// not "next". Callers must return immediately when this returns false.
//
// In stable mode (SW_UX=stable) the middleware.UX gate already 404s any /next/*
// request before any handler runs, so this call is dead code in that mode. In
// next/dual mode it guards the edge case where an explicit
// X-Stillwater-UX: stable header opts a per-request sub-request back to the
// stable channel -- those requests reach the handler but must not render next/
// content (decision 12, docs/architecture-decisions.md).
//
// All handleNext* page handlers call this as their first guard so the policy is
// enforced consistently regardless of which handler is reached.
func checkNextChannel(w http.ResponseWriter, req *http.Request) bool {
	if middleware.UXChannelFromContext(req.Context()) != middleware.UXNext {
		http.NotFound(w, req)
		return false
	}
	return true
}
