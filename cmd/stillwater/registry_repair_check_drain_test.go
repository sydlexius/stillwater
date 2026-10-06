package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api"
	"github.com/sydlexius/stillwater/internal/maintenance"
)

// startRegistryRepairCheck must track the detector goroutine: the done channel
// stays open while the loop runs and closes once ctx is canceled, and a drain
// with an already-expired ctx reports the timeout while the loop is running.
func TestRegistryRepairCheckDrain_WaitsForLoop(t *testing.T) {
	a := &Application{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.drainRegistryRepairCheck(context.Background()); err != nil {
		t.Fatalf("drain of a never-started loop = %v, want nil", err)
	}
	a.maintenanceService = maintenance.NewService(nil, "", "", a.logger)
	a.registryRepairCache = &maintenance.RegistryRepairCache{}
	a.router = &api.Router{} // the claim is only bound, never called: ctx is canceled during the startup delay

	ctx, cancel := context.WithCancel(context.Background())
	a.startRegistryRepairCheck(ctx) // default 2m startup delay: the loop parks
	if a.registryRepairCheckDone == nil {
		t.Fatal("startRegistryRepairCheck did not record a done channel")
	}
	expired, expire := context.WithCancel(context.Background())
	expire()
	if err := a.drainRegistryRepairCheck(expired); err == nil {
		t.Fatal("drain returned nil while the detector loop was still running")
	}
	cancel()
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	if err := a.drainRegistryRepairCheck(dctx); err != nil {
		t.Fatalf("drain after cancel = %v, want the loop to exit", err)
	}
}

// The cadence seam must honor a positive duration and fall back to the
// production defaults (0) for an unset, malformed, or non-positive value, so a
// typo can never start back-to-back library scans.
func TestRegistryRepairCheckEvery(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 0},
		{"3s", 3 * time.Second},
		{"1s", time.Second},
		{"2s", 2 * time.Second},
		{"24h", 24 * time.Hour},
		{"1ms", 0},
		{"999ms", 0},
		{"999999h", 0},
		{"soon", 0},
		{"0s", 0},
		{"-5s", 0},
	}
	for _, tc := range cases {
		if got := registryRepairCheckEvery(logger, tc.raw); got != tc.want {
			t.Errorf("registryRepairCheckEvery(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// A release build must refuse the seam, exactly like SW_FORCE_PROVIDER_ERROR.
func TestRefuseRegistryRepairSeam(t *testing.T) {
	if err := refuseRegistryRepairSeam("2s", true); err == nil {
		t.Error("release build with the seam set must be refused")
	}
	if err := refuseRegistryRepairSeam("", true); err != nil {
		t.Errorf("release build with the seam unset = %v, want nil", err)
	}
	if err := refuseRegistryRepairSeam("2s", false); err != nil {
		t.Errorf("non-release build with the seam set = %v, want nil", err)
	}
}
