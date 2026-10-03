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
