package publishtest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
)

// Handle is the small surface AssertPeerSemantics needs from a peer. It takes
// an interface, not *Peer, so the SAME contract can later run against a live
// Emby or Jellyfin server (under the `integration` build tag) as well as the
// fake: if the fake and a real server disagree, that contract run is where it
// shows.
type Handle interface {
	// ConnType is connection.TypeEmby or connection.TypeJellyfin. It decides
	// whether an upload below the length is expected to replace or append.
	ConnType() string
	Upload(index int, data []byte) error
	Download(index int) ([]byte, error)
	Delete(index int) error
	// Len is the peer's reported backdrop count. A live implementation must
	// return a SETTLED count: check() reads it right after each write, and a
	// real Emby's count lags a write briefly (fanart_indexed_upload_verify.go).
	Len() (int, error)
}

// backdropClient is the part of the real emby and jellyfin clients the
// adapter uses; both satisfy it.
type backdropClient interface {
	connection.IndexedImageUploader
	connection.IndexedImageDeleter
	connection.BackdropReader
}

// clientHandle adapts a REAL platform client (pointed at the fake or, later,
// at a live server) to Handle.
type clientHandle struct {
	connType string
	c        backdropClient
	itemID   string
}

// NewHandle builds a Handle over the real platform client for connType.
// baseURL is the fake's httptest URL or a live server; itemID is the artist
// item whose backdrop list is exercised.
func NewHandle(connType, baseURL, apiKey, userID, itemID string) Handle {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var c backdropClient
	if connType == connection.TypeEmby {
		c = emby.New(baseURL, apiKey, userID, logger)
	} else {
		c = jellyfin.New(baseURL, apiKey, userID, logger)
	}
	return &clientHandle{connType: connType, c: c, itemID: itemID}
}

func (h *clientHandle) ConnType() string { return h.connType }

func (h *clientHandle) Upload(index int, data []byte) error {
	return h.c.UploadImageAtIndex(context.Background(), h.itemID, "fanart", index, data, "image/jpeg")
}

func (h *clientHandle) Download(index int) ([]byte, error) {
	b, _, err := h.c.GetArtistBackdrop(context.Background(), h.itemID, index)
	return b, err
}

func (h *clientHandle) Delete(index int) error {
	return h.c.DeleteImageAtIndex(context.Background(), h.itemID, "fanart", index)
}

func (h *clientHandle) Len() (int, error) {
	st, err := h.c.GetArtistDetail(context.Background(), h.itemID)
	if err != nil {
		return 0, err
	}
	return st.BackdropCount, nil
}

// AssertPeerSemantics checks the write semantics every #3175 slice relies on,
// against any Handle. It keeps its OWN expected list (`want`), updated by the
// rules below, and after every step reads the peer back slot by slot and
// compares bytes:
//
//   - an upload at index == length appends;
//   - an upload at an index BELOW the length replaces that slot (Emby) or
//     appends (Jellyfin), per connection.SupportsIndexedBackdropReplace;
//   - a delete removes the slot and shifts every higher slot down by one;
//   - readback is byte-identical to what was uploaded.
//
// imgs needs at least 4 distinct images. The peer must start with NO
// backdrops (checked, so a dirty live item fails loudly rather than
// producing a misleading pass); it is emptied again before returning.
func AssertPeerSemantics(t testing.TB, h Handle, imgs [][]byte) {
	t.Helper()
	if len(imgs) < 4 {
		t.Fatalf("AssertPeerSemantics needs >= 4 distinct images, got %d", len(imgs))
	}
	requireDistinct(t, imgs)
	if n, err := h.Len(); err != nil || n != 0 {
		t.Fatalf("precondition: peer must start with 0 backdrops, got %d (err %v)", n, err)
	}
	replaces := connection.SupportsIndexedBackdropReplace(h.ConnType())
	var want [][]byte

	check := func(step string) {
		t.Helper()
		n, err := h.Len()
		if err != nil || n != len(want) {
			t.Fatalf("%s: peer reports %d backdrops (err %v), want %d", step, n, err, len(want))
		}
		for i, w := range want {
			got, err := h.Download(i)
			if err != nil {
				t.Fatalf("%s: reading slot %d: %v", step, i, err)
			}
			if !bytes.Equal(got, w) {
				t.Fatalf("%s: slot %d is not byte-identical to what the model expects", step, i)
			}
		}
	}
	upload := func(step string, idx int, b []byte) {
		t.Helper()
		if err := h.Upload(idx, b); err != nil {
			t.Fatalf("%s: upload at %d: %v", step, idx, err)
		}
		if replaces && idx < len(want) {
			want[idx] = b // Emby: replace in place
		} else {
			want = append(want, b) // at the length, or Jellyfin below it: append
		}
		check(step)
	}

	// Uploads at the length append (every peer).
	upload("append at length 0", 0, imgs[0])
	upload("append at length 1", 1, imgs[1])
	upload("append at length 2", 2, imgs[2])
	// An upload below the length: replace (Emby) or append (Jellyfin).
	upload("upload below length", 1, imgs[3])

	// Upload at the LAST slot (len-1), the #3175 tail boundary: Emby replaces
	// it, Jellyfin appends.
	upload("upload at last slot", len(want)-1, imgs[0])

	// Delete the first slot: every higher slot shifts down by one.
	if err := h.Delete(0); err != nil {
		t.Fatalf("delete slot 0: %v", err)
	}
	want = want[1:]
	check("delete first slot")

	// Delete the last slot: nothing shifts, the list just gets shorter.
	if err := h.Delete(len(want) - 1); err != nil {
		t.Fatalf("delete last slot: %v", err)
	}
	want = want[:len(want)-1]
	check("delete last slot")

	// Leave the peer empty (high index first so no shift is in play).
	for len(want) > 0 {
		if err := h.Delete(len(want) - 1); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
		want = want[:len(want)-1]
	}
	check("emptied")
}

// requireDistinct fails the test on any byte-identical pair in imgs.
func requireDistinct(t testing.TB, imgs [][]byte) {
	t.Helper()
	for i := range imgs {
		for j := i + 1; j < len(imgs); j++ {
			if bytes.Equal(imgs[i], imgs[j]) {
				t.Fatalf("images %d and %d are byte-identical; the contract needs distinct images", i, j)
			}
		}
	}
}
