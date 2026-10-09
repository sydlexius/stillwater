package publishtest

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// img returns a small distinct payload per n. The fake never decodes images,
// so any distinct bytes are a faithful fixture here.
func img(n byte) []byte { return bytes.Repeat([]byte{n}, 8) }

func images() [][]byte { return [][]byte{img(1), img(2), img(3), img(4)} }

// newHandle serves p over HTTP and returns a Handle built on the REAL platform
// client for connType.
func newHandle(t *testing.T, connType string, p *Peer) Handle {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return NewHandle(connType, srv.URL, "k", "u1", "item1")
}

func bothTypes() []string { return []string{connection.TypeEmby, connection.TypeJellyfin} }

func wantState(t *testing.T, p *Peer, want ...[]byte) {
	t.Helper()
	got, _ := p.State()
	if len(got) != len(want) {
		t.Fatalf("peer holds %d backdrops, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("slot %d holds the wrong image", i)
		}
	}
}

// TestAssertPeerSemantics_Fake runs the shared contract against the fake for
// both peer kinds. A later slice runs the SAME function against a live server.
func TestAssertPeerSemantics_Fake(t *testing.T) {
	for _, typ := range bothTypes() {
		t.Run(typ, func(t *testing.T) {
			AssertPeerSemantics(t, newHandle(t, typ, NewPeer(typ, nil)), images())
		})
	}
}

// TestPeer_ReplaceVersusAppend pins the one behavioral difference between the
// kinds directly, so a break in the fake is named here and not only in the
// contract.
func TestPeer_ReplaceVersusAppend(t *testing.T) {
	a, b, c := img(1), img(2), img(3)
	emby := NewPeer(connection.TypeEmby, [][]byte{a, b})
	jelly := NewPeer(connection.TypeJellyfin, [][]byte{a, b})
	for _, tc := range []struct {
		typ  string
		p    *Peer
		want [][]byte
	}{
		{connection.TypeEmby, emby, [][]byte{c, b}},
		{connection.TypeJellyfin, jelly, [][]byte{a, b, c}},
	} {
		if err := newHandle(t, tc.typ, tc.p).Upload(0, c); err != nil {
			t.Fatalf("%s upload: %v", tc.typ, err)
		}
		wantState(t, tc.p, tc.want...)
	}
}

// TestPeer_InjectedFaultLeavesStateUntouched: for each call kind, a faulted
// call errors, changes nothing (data AND write counters), is logged as a
// failure, and the NEXT call succeeds because the fault was spent.
func TestPeer_InjectedFaultLeavesStateUntouched(t *testing.T) {
	for _, typ := range bothTypes() {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			t.Run(typ+"/"+method, func(t *testing.T) {
				p := NewPeer(typ, [][]byte{img(1), img(2)})
				h := newHandle(t, typ, p)
				p.InjectFault(method, AnyIndex, 1)
				var err error
				switch method {
				case http.MethodPost:
					err = h.Upload(2, img(9))
				case http.MethodGet:
					_, err = h.Download(0)
				case http.MethodDelete:
					err = h.Delete(0)
				}
				if err == nil {
					t.Fatal("faulted call succeeded, want an error")
				}
				wantState(t, p, img(1), img(2))
				if _, writes := p.State(); writes != 0 || p.DeleteCount() != 0 {
					t.Errorf("faulted call counted as a write (writes %d, deletes %d)", writes, p.DeleteCount())
				}
				reqs := p.Requests()
				if len(reqs) != 1 || reqs[0].Status != http.StatusInternalServerError || reqs[0].Mutated {
					t.Fatalf("request log = %+v, want one failed, unmutated 500", reqs)
				}
				// The fault is spent: the same call now works.
				switch method {
				case http.MethodPost:
					err = h.Upload(2, img(9))
				case http.MethodGet:
					_, err = h.Download(0)
				case http.MethodDelete:
					err = h.Delete(0)
				}
				if err != nil {
					t.Fatalf("retry after the fault was spent: %v", err)
				}
			})
		}
	}
}

// TestPeer_FaultIsIndexScoped: a fault pinned to one index leaves the others
// alone.
func TestPeer_FaultIsIndexScoped(t *testing.T) {
	p := NewPeer(connection.TypeEmby, [][]byte{img(1), img(2)})
	h := newHandle(t, connection.TypeEmby, p)
	p.InjectFault(http.MethodGet, 1, 1)
	if _, err := h.Download(0); err != nil {
		t.Fatalf("download at an un-faulted index: %v", err)
	}
	if _, err := h.Download(1); err == nil {
		t.Fatal("download at the faulted index succeeded")
	}
}

// TestPeer_StaleCountLagsRealList: the reported count trails the real list,
// but by-index reads still see the real data, and turning the lag off heals it.
func TestPeer_StaleCountLagsRealList(t *testing.T) {
	p := NewPeer(connection.TypeEmby, [][]byte{img(1), img(2), img(3)})
	h := newHandle(t, connection.TypeEmby, p)
	p.SetStaleCount(1)
	if n, err := h.Len(); err != nil || n != 2 {
		t.Fatalf("stale count = %d (err %v), want 2 while 3 are held", n, err)
	}
	if got, err := h.Download(2); err != nil || !bytes.Equal(got, img(3)) {
		t.Fatalf("by-index read of the lagging slot: %v", err)
	}
	p.SetStaleCount(5) // lag larger than the list clamps at 0, never negative
	if n, err := h.Len(); err != nil || n != 0 {
		t.Fatalf("over-large lag reported %d (err %v), want 0", n, err)
	}
	p.SetStaleCount(-2) // a negative lag must not over-report
	if n, err := h.Len(); err != nil || n != 3 {
		t.Fatalf("negative lag reported %d (err %v), want 3", n, err)
	}
	p.SetStaleCount(0)
	if n, _ := h.Len(); n != 3 {
		t.Fatalf("count after the lag ended = %d, want 3", n)
	}
}

// TestPeer_OperatorActionsChangeStateSilently: operator add/delete change the
// list (with the same shift rules) but are invisible to the request log and
// write counters, because Stillwater made no request.
func TestPeer_OperatorActionsChangeStateSilently(t *testing.T) {
	p := NewPeer(connection.TypeEmby, [][]byte{img(1), img(2)})
	p.OperatorAdd(img(3))
	wantState(t, p, img(1), img(2), img(3))
	if !p.OperatorDelete(0) {
		t.Fatal("OperatorDelete(0) reported no such slot")
	}
	wantState(t, p, img(2), img(3)) // higher slots shifted down
	if p.OperatorDelete(5) {
		t.Fatal("OperatorDelete past the end reported success")
	}
	if _, writes := p.State(); writes != 0 || len(p.Requests()) != 0 {
		t.Errorf("operator actions leaked into counters/log (writes %d, log %d)", writes, len(p.Requests()))
	}
}

// TestPeer_RequestLog: every Stillwater request is recorded in order with its
// index, status and whether it mutated; the count read is index -1.
func TestPeer_RequestLog(t *testing.T) {
	p := NewPeer(connection.TypeEmby, [][]byte{img(1)})
	h := newHandle(t, connection.TypeEmby, p)
	_, _ = h.Len()
	_ = h.Upload(1, img(2))
	_, _ = h.Download(7) // missing slot: 404, no mutation
	_ = h.Delete(0)
	want := []Request{
		{http.MethodGet, DetailIndex, http.StatusOK, false, "/Users/u1/Items/item1"},
		{http.MethodPost, 1, http.StatusNoContent, true, "/Items/item1/Images/Backdrop/1"},
		{http.MethodGet, 7, http.StatusNotFound, false, "/Items/item1/Images/Backdrop/7"},
		{http.MethodDelete, 0, http.StatusNoContent, true, "/Items/item1/Images/Backdrop/0"},
	}
	got := p.Requests()
	if len(got) != len(want) {
		t.Fatalf("log = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("log[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if p.RequestCount(http.MethodDelete) != 1 || p.RequestCount(http.MethodPost) != 1 {
		t.Errorf("RequestCount wrong: DELETE %d POST %d", p.RequestCount(http.MethodDelete), p.RequestCount(http.MethodPost))
	}
}

// TestPeer_ConcurrentUse is for -race: parallel Stillwater requests plus
// operator actions must not race on the peer's state.
func TestPeer_ConcurrentUse(t *testing.T) {
	p := NewPeer(connection.TypeEmby, nil)
	h := newHandle(t, connection.TypeEmby, p)
	var wg sync.WaitGroup
	// Three kinds of goroutine, so every locked method has a concurrent
	// counterpart: HTTP writers, direct mutators, and readers.
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = h.Upload(0, img(1))
				_, _ = h.Len()
				_ = h.Delete(0)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				p.OperatorAdd(img(2))
				p.OperatorDelete(0)
				p.SetStaleCount(j % 2)
				p.InjectFault(http.MethodGet, AnyIndex, 0)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				_ = p.Requests()
				_, _ = p.State()
				_ = p.DeleteCount()
			}
		}()
	}
	wg.Wait()
}

// TestPeer_EmbyUploadPastLengthAppendsAnd500s pins the measured Emby 4.9.5.0
// quirk (#3126): an upload at idx > len appends AND answers 500; idx == len is
// a clean 204; Jellyfin appends cleanly whatever the index.
func TestPeer_EmbyUploadPastLengthAppendsAnd500s(t *testing.T) {
	for _, tc := range []struct {
		typ string
		idx int
		err bool
	}{
		{connection.TypeEmby, 1, false},
		{connection.TypeEmby, 3, true},
		{connection.TypeJellyfin, 3, false},
	} {
		p := NewPeer(tc.typ, [][]byte{img(1)})
		err := newHandle(t, tc.typ, p).Upload(tc.idx, img(9))
		if (err != nil) != tc.err {
			t.Errorf("%s idx %d: err = %v, want error=%v", tc.typ, tc.idx, err, tc.err)
		}
		wantState(t, p, img(1), img(9))
		if r := p.Requests(); len(r) != 1 || !r[0].Mutated {
			t.Errorf("%s idx %d: log = %+v, want one Mutated request", tc.typ, tc.idx, r)
		}
	}
}

// TestPeer_UnmatchedRequestsAreLogged: a path the fake does not model (the
// UNINDEXED backdrop POST/DELETE production sends) is logged, answers 404 and
// changes nothing.
func TestPeer_UnmatchedRequestsAreLogged(t *testing.T) {
	p := NewPeer(connection.TypeEmby, [][]byte{img(1)})
	srv := httptest.NewServer(p)
	defer srv.Close()
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		req, _ := http.NewRequest(m, srv.URL+"/Items/item1/Images/Backdrop", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	wantState(t, p, img(1))
	if p.RequestCount(http.MethodPost) != 1 || p.RequestCount(http.MethodDelete) != 1 {
		t.Fatalf("unindexed requests not logged: %+v", p.Requests())
	}
	if r := p.Requests()[0]; r.Index != UnmatchedIndex || r.Status != http.StatusNotFound || r.Mutated {
		t.Errorf("unmatched entry = %+v", r)
	}
}

// TestPeer_EdgeSemantics: operator deletes are not counted as deletes, an
// out-of-range DELETE is a logged no-op 404, and a fault is scoped to its method.
func TestPeer_EdgeSemantics(t *testing.T) {
	p := NewPeer(connection.TypeEmby, [][]byte{img(1), img(2)})
	h := newHandle(t, connection.TypeEmby, p)
	p.OperatorDelete(0)
	if p.DeleteCount() != 0 {
		t.Errorf("OperatorDelete counted as %d deletes, want 0", p.DeleteCount())
	}
	if err := h.Delete(5); err == nil {
		t.Error("out-of-range DELETE succeeded")
	}
	if r := p.Requests(); len(r) != 1 || r[0].Status != http.StatusNotFound || r[0].Mutated {
		t.Errorf("out-of-range DELETE log = %+v, want one 404, Mutated=false", r)
	}
	wantState(t, p, img(2))
	p.InjectFault(http.MethodPost, 0, 1)
	if err := h.Delete(0); err != nil {
		t.Errorf("a POST fault fired on a DELETE at the same index: %v", err)
	}
}

// TestAssertPeerSemantics_Guards: the contract refuses a non-empty peer (it
// protects a live item's artwork) and non-distinct images. A stub TB records
// Fatalf and aborts the contract with a panic the test recovers.
func TestAssertPeerSemantics_Guards(t *testing.T) {
	run := func(h Handle, imgs [][]byte) (msg string) {
		stub := &fatalStub{}
		defer func() {
			if r := recover(); r != nil {
				msg = stub.msg
			}
		}()
		AssertPeerSemantics(stub, h, imgs)
		return ""
	}
	if m := run(newHandle(t, connection.TypeEmby, NewPeer(connection.TypeEmby, [][]byte{img(1)})), images()); !strings.Contains(m, "must start with 0") {
		t.Errorf("non-empty peer: got %q, want the precondition refusal", m)
	}
	dup := [][]byte{img(1), img(2), img(1), img(4)}
	if m := run(newHandle(t, connection.TypeEmby, NewPeer(connection.TypeEmby, nil)), dup); !strings.Contains(m, "byte-identical") {
		t.Errorf("duplicate images: got %q, want the distinctness refusal", m)
	}
}

// fatalStub is a testing.TB whose Fatalf records the message and panics, the
// way a real Fatalf stops the goroutine.
type fatalStub struct {
	testing.TB
	msg string
}

func (s *fatalStub) Helper() {}
func (s *fatalStub) Fatalf(f string, a ...any) {
	s.msg = fmt.Sprintf(f, a...)
	panic("fatal")
}
