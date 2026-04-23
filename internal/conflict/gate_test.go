package conflict

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

func TestGateAllowImageWriteClean(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: connection.TypeEmby, Enabled: true}}
	client := &fakeClient{}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)
	g := NewGate(d)

	if err := g.AllowImageWrite(context.Background()); err != nil {
		t.Errorf("clean ledger should allow image writes, got %v", err)
	}
}

func TestGateBlocksOnImageConflict(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: connection.TypeEmby, Enabled: true}}
	client := &fakeClient{image: true}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)
	g := NewGate(d)

	err := g.AllowImageWrite(context.Background())
	be, ok := AsBlocked(err)
	if !ok {
		t.Fatalf("want BlockedError, got %T: %v", err, err)
	}
	if be.Axis != AxisImage {
		t.Errorf("want axis=image, got %s", be.Axis)
	}
}

func TestGateBlocksOnNFOConflict(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: connection.TypeEmby, Enabled: true}}
	client := &fakeClient{nfo: true}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)
	g := NewGate(d)

	err := g.AllowNFOWrite(context.Background())
	be, ok := AsBlocked(err)
	if !ok {
		t.Fatalf("want BlockedError, got %T: %v", err, err)
	}
	if be.Axis != AxisNFO {
		t.Errorf("want axis=nfo, got %s", be.Axis)
	}
}

func TestGateRoundTripBlocksBothAxes(t *testing.T) {
	conns := []connection.Connection{
		{ID: "a", Type: connection.TypeEmby, Enabled: true},
		{ID: "b", Type: connection.TypeJellyfin, Enabled: true},
	}
	clients := map[string]peerClient{"a": &fakeClient{}, "b": &fakeClient{}}
	paths := map[string]pathProvider{
		"a": &fakePaths{paths: []string{"/music"}},
		"b": &fakePaths{paths: []string{"/music"}},
	}
	d, _ := buildDetector(t, conns, clients, paths)
	g := NewGate(d)

	for _, fn := range []func(context.Context) error{g.AllowImageWrite, g.AllowNFOWrite} {
		err := fn(context.Background())
		be, ok := AsBlocked(err)
		if !ok {
			t.Fatalf("round-trip should block, got %v", err)
		}
		if be.Axis != AxisRoundTrip {
			t.Errorf("want axis=round_trip, got %s", be.Axis)
		}
	}
}

func TestGateManagedConnectionAllowsWrites(t *testing.T) {
	conns := []connection.Connection{
		{ID: "a", Type: connection.TypeEmby, Enabled: true, FeatureManageServerFiles: true},
	}
	clients := map[string]peerClient{"a": &fakeClient{image: true, nfo: true}}
	d, _ := buildDetector(t, conns, clients, nil)
	g := NewGate(d)

	if err := g.AllowImageWrite(context.Background()); err != nil {
		t.Errorf("managed connection should not gate image writes: %v", err)
	}
	if err := g.AllowNFOWrite(context.Background()); err != nil {
		t.Errorf("managed connection should not gate NFO writes: %v", err)
	}
}
