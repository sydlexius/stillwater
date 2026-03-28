package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestHandleLogout(t *testing.T) {
	t.Run("returns 200 with JSON body and HX-Redirect header", func(t *testing.T) {
		r := &Router{
			logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
			basePath: "/sw",
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		rec := httptest.NewRecorder()

		r.handleLogout(rec, req)

		resp := rec.Result()
		defer resp.Body.Close()

		// Verify HTTP 200 status.
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}

		// Verify Content-Type is JSON.
		ct := resp.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Fatalf("expected Content-Type application/json, got %q", ct)
		}

		// Verify the HX-Redirect header includes basePath + "/".
		redirect := resp.Header.Get("HX-Redirect")
		if redirect != "/sw/" {
			t.Fatalf("expected HX-Redirect %q, got %q", "/sw/", redirect)
		}

		// Verify JSON body contains {"status":"ok"}.
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode JSON body: %v", err)
		}
		if body["status"] != "ok" {
			t.Fatalf("expected status ok, got %q", body["status"])
		}
	})

	t.Run("clears session cookie", func(t *testing.T) {
		r := &Router{
			logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
			basePath: "",
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		rec := httptest.NewRecorder()

		r.handleLogout(rec, req)

		resp := rec.Result()
		defer resp.Body.Close()

		// Verify session cookie is cleared (MaxAge -1).
		var found bool
		for _, c := range resp.Cookies() {
			if c.Name == "session" {
				found = true
				if c.MaxAge != -1 {
					t.Fatalf("expected session cookie MaxAge -1, got %d", c.MaxAge)
				}
				if c.Value != "" {
					t.Fatalf("expected empty session cookie value, got %q", c.Value)
				}
			}
		}
		if !found {
			t.Fatal("expected session cookie in response")
		}

		// Verify HX-Redirect with empty basePath.
		redirect := resp.Header.Get("HX-Redirect")
		if redirect != "/" {
			t.Fatalf("expected HX-Redirect %q, got %q", "/", redirect)
		}
	})

	t.Run("normalizes basePath trailing slash", func(t *testing.T) {
		r := &Router{
			logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
			basePath: "/sw/",
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		rec := httptest.NewRecorder()

		r.handleLogout(rec, req)

		resp := rec.Result()
		defer resp.Body.Close()

		redirect := resp.Header.Get("HX-Redirect")
		if redirect != "/sw/" {
			t.Fatalf("expected HX-Redirect %q, got %q", "/sw/", redirect)
		}
	})
}
