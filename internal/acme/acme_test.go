package acme

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// challengeStore tests
// ---------------------------------------------------------------------------

func TestChallengeStore_ServeToken(t *testing.T) {
	store := newChallengeStore()
	store.set("mytoken", "mytoken.keyauth")

	handler := store.handler(http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/mytoken", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if body != "mytoken.keyauth" {
		t.Errorf("body = %q, want %q", body, "mytoken.keyauth")
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestChallengeStore_FallsThrough(t *testing.T) {
	store := newChallengeStore()
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	handler := store.handler(fallback)

	req := httptest.NewRequest(http.MethodGet, "/other/path", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d (fallback not invoked)", rr.Code, http.StatusTeapot)
	}
}

func TestChallengeStore_UnknownToken(t *testing.T) {
	store := newChallengeStore()
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	handler := store.handler(fallback)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/unknown", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Unknown token: fall through to next handler.
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (unknown token should fall through)", rr.Code, http.StatusNotFound)
	}
}

func TestChallengeStore_DeleteRemovesToken(t *testing.T) {
	store := newChallengeStore()
	store.set("tok", "tok.keyauth")
	store.delete("tok")

	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	handler := store.handler(fallback)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/tok", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("deleted token still served; status = %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// decodeEABMACKey tests
// ---------------------------------------------------------------------------

func TestDecodeEABMACKey_Base64URL(t *testing.T) {
	// "hello" as RawURLEncoding = "aGVsbG8"
	key, err := decodeEABMACKey("aGVsbG8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(key) != "hello" {
		t.Errorf("key = %q, want %q", string(key), "hello")
	}
}

func TestDecodeEABMACKey_StdBase64(t *testing.T) {
	// "hello" as StdEncoding = "aGVsbG8="
	key, err := decodeEABMACKey("aGVsbG8=")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(key) != "hello" {
		t.Errorf("key = %q, want %q", string(key), "hello")
	}
}

func TestDecodeEABMACKey_Invalid(t *testing.T) {
	_, err := decodeEABMACKey("not!!valid!!base64!!")
	if err == nil {
		t.Fatal("expected error for invalid Base64 input")
	}
}

// ---------------------------------------------------------------------------
// directoryURL tests
// ---------------------------------------------------------------------------

func TestDirectoryURL(t *testing.T) {
	tests := []struct {
		ca   string
		want string
	}{
		{"letsencrypt", DirectoryLetsEncrypt},
		{"", DirectoryLetsEncrypt},
		{"zerossl", DirectoryZeroSSL},
		{"https://custom.example.com/acme", "https://custom.example.com/acme"},
	}

	for _, tt := range tests {
		m := &Manager{cfg: Config{CA: tt.ca}}
		got := m.directoryURL()
		if got != tt.want {
			t.Errorf("directoryURL(%q) = %q, want %q", tt.ca, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// redirectToHTTPS tests
// ---------------------------------------------------------------------------

func TestRedirectToHTTPS(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://1.2.3.4/path?q=1", nil)
	req.Host = "1.2.3.4"
	rr := httptest.NewRecorder()
	redirectToHTTPS(rr, req)

	if rr.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusMovedPermanently)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://") {
		t.Errorf("Location = %q, does not start with https://", loc)
	}
}

// ---------------------------------------------------------------------------
// needsIPRenewal tests
// ---------------------------------------------------------------------------

func TestNeedsIPRenewal_NilCert(t *testing.T) {
	m := &Manager{}
	if !m.needsIPRenewal() {
		t.Error("needsIPRenewal() = false for nil cert; want true")
	}
}
