// Package publishtest holds a stateful fake of an Emby or Jellyfin server's
// backdrop list, plus a contract function that pins the peer semantics the
// publisher depends on (#3175).
//
// It is test support: only _test.go files may import it, so it never links
// into the production binary (checked with
// `go list -deps ./cmd/stillwater | grep publishtest`, which must print
// nothing). It lives in a normal (non-_test) package only so that tests in
// OTHER packages can import it, the same convention as logging/logtest.
package publishtest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"

	"github.com/sydlexius/stillwater/internal/connection"
)

// AnyIndex makes an injected fault match every index.
const AnyIndex = -1

// Request is one entry of the request log: what Stillwater asked the peer
// to do and what the peer answered. Operator actions (OperatorAdd,
// OperatorDelete) are NOT logged here, because they model a person acting in
// the platform UI, not a request from Stillwater.
type Request struct {
	Method  string // GET, POST or DELETE
	Index   int    // backdrop slot, or -1 for the item-detail GET (count read)
	Status  int    // HTTP status the peer answered
	Mutated bool   // true only when the request changed the peer's state
}

// fault makes the next `remaining` matching calls fail with HTTP 500 and
// leave the peer's state untouched.
type fault struct {
	method    string
	index     int
	remaining int
}

// Peer is an httptest Emby/Jellyfin that models ONE item's backdrop LIST, not
// just the calls it receives (#3144). The real emby/jellyfin clients talk to
// it over HTTP, so the prune, the reconciler's state read, and both full-set
// push shapes run their production code against it.
//
// Write semantics are the measured ones:
//   - Emby: POST to index i < len REPLACES slot i; any other index appends
//     (#3125, #3145 measurements on 4.9.5.0).
//   - Jellyfin: every POST appends, whatever the index (#3135).
//   - Both: DELETE at i removes slot i and renumbers the rest down.
//
// Which of the two a Peer behaves as comes from
// connection.SupportsIndexedBackdropReplace, the same predicate production
// uses, so the fake and the publisher cannot disagree about it.
//
// All methods are safe for concurrent use.
type Peer struct {
	mu        sync.Mutex
	appendAll bool
	data      [][]byte
	writes    int // successful POST and DELETE, so a pass that touches nothing is provable
	deletes   int // successful DELETE alone, so a path that must never delete is provable (#3147)
	staleLag  int // how many slots the reported count trails the real list
	faults    []fault
	log       []Request
}

// NewPeer builds a peer that behaves as connType (connection.TypeEmby or
// connection.TypeJellyfin) holding a copy of seed.
func NewPeer(connType string, seed [][]byte) *Peer {
	p := &Peer{appendAll: !connection.SupportsIndexedBackdropReplace(connType)}
	for _, b := range seed {
		p.data = append(p.data, bytes.Clone(b))
	}
	return p
}

var (
	peerDetailPath   = regexp.MustCompile(`^/Users/[^/]+/Items/[^/]+$`)
	peerBackdropPath = regexp.MustCompile(`^/Items/[^/]+/Images/Backdrop/(\d+)$`)
)

// consumeFault reports whether a matching injected fault fires for this call,
// and uses one of its remaining charges. Callers hold p.mu.
func (p *Peer) consumeFault(method string, idx int) bool {
	for i := range p.faults {
		f := &p.faults[i]
		if f.remaining > 0 && f.method == method && (f.index == AnyIndex || f.index == idx) {
			f.remaining--
			return true
		}
	}
	return false
}

// ServeHTTP implements the peer's three endpoints: item detail (count),
// backdrop-by-index GET, backdrop-by-index POST and DELETE.
func (p *Peer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method == http.MethodGet && peerDetailPath.MatchString(r.URL.Path) {
		// The reported tag count may trail the real list (SetStaleCount).
		n := max(len(p.data)-p.staleLag, 0)
		tags := make([]string, n)
		for i := range tags {
			tags[i] = "t" + strconv.Itoa(i)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"Name": "peer item", "BackdropImageTags": tags})
		p.log = append(p.log, Request{Method: http.MethodGet, Index: -1, Status: http.StatusOK})
		return
	}
	m := peerBackdropPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	idx, _ := strconv.Atoi(m[1])
	status, mutated := p.handleBackdrop(w, r, idx)
	p.log = append(p.log, Request{Method: r.Method, Index: idx, Status: status, Mutated: mutated})
}

// handleBackdrop serves one by-index request and returns the status it wrote
// and whether it changed state. An injected fault is checked FIRST and returns
// before any state change, which is what makes "a failed call mutates
// nothing" true by construction.
func (p *Peer) handleBackdrop(w http.ResponseWriter, r *http.Request, idx int) (status int, mutated bool) {
	if p.consumeFault(r.Method, idx) {
		http.Error(w, "injected fault", http.StatusInternalServerError)
		return http.StatusInternalServerError, false
	}
	switch r.Method {
	case http.MethodGet:
		if idx >= len(p.data) {
			http.NotFound(w, r)
			return http.StatusNotFound, false
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(p.data[idx])
		return http.StatusOK, false
	case http.MethodPost:
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading upload body: "+err.Error(), http.StatusInternalServerError)
			return http.StatusInternalServerError, false
		}
		b, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return http.StatusBadRequest, false
		}
		p.writes++
		if !p.appendAll && idx < len(p.data) {
			p.data[idx] = b
		} else {
			p.data = append(p.data, b)
		}
		w.WriteHeader(http.StatusNoContent)
		return http.StatusNoContent, true
	case http.MethodDelete:
		if idx >= len(p.data) {
			http.NotFound(w, r)
			return http.StatusNotFound, false
		}
		p.writes++
		p.deletes++
		p.data = append(p.data[:idx], p.data[idx+1:]...)
		w.WriteHeader(http.StatusNoContent)
		return http.StatusNoContent, true
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return http.StatusMethodNotAllowed, false
	}
}

// State returns a copy of the peer's backdrop list and the number of
// successful writes (POST and DELETE) Stillwater has made.
func (p *Peer) State() (data [][]byte, writes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.data))
	for i, b := range p.data {
		out[i] = bytes.Clone(b)
	}
	return out, p.writes
}

// DeleteCount is the number of successful DELETE requests.
func (p *Peer) DeleteCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deletes
}

// InjectFault makes the next `times` calls of method (GET by index, POST or
// DELETE) at index (or AnyIndex) answer HTTP 500 WITHOUT changing state. The
// detail GET that reads the count is never faulted.
func (p *Peer) InjectFault(method string, index, times int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults = append(p.faults, fault{method: method, index: index, remaining: times})
}

// SetStaleCount makes the reported backdrop count trail the real list by lag
// slots, the way a real peer's tag list briefly does. 0 turns it off.
func (p *Peer) SetStaleCount(lag int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.staleLag = lag
}

// OperatorAdd appends an image the way an operator uploading in the platform
// UI would: the peer's state changes, but Stillwater made no request, so the
// request log and write counters are untouched.
func (p *Peer) OperatorAdd(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data = append(p.data, bytes.Clone(b))
}

// OperatorDelete removes slot index (higher slots shift down) the way an
// operator deleting in the platform UI would. It reports false when the slot
// does not exist. Not logged or counted, like OperatorAdd.
func (p *Peer) OperatorDelete(index int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.data) {
		return false
	}
	p.data = append(p.data[:index], p.data[index+1:]...)
	return true
}

// Requests returns a copy of the request log in arrival order.
func (p *Peer) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.log...)
}

// RequestCount counts logged requests of method.
func (p *Peer) RequestCount(method string) int {
	n := 0
	for _, r := range p.Requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}
