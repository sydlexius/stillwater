package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/sydlexius/stillwater/internal/config"
)

// discardLogger returns a slog.Logger that discards every record. Tests do
// not need to inspect the listener's structured logs, only its behavior.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freePort returns an OS-allocated TCP port. The caller binds it
// immediately; reuse is acceptable because RunListeners closes the listener
// before the test ends.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// generateSelfSignedCert produces a short-lived self-signed cert/key pair
// at writeDir/cert.pem and writeDir/key.pem. The tests load it through the
// same code path that production uses (paths-on-disk), so the helper avoids
// drift between test and production cert handling.
func generateSelfSignedCert(t *testing.T, writeDir string) (certPath, keyPath string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "stillwater-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPath = filepath.Join(writeDir, "cert.pem")
	keyPath = filepath.Join(writeDir, "key.pem")
	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		certFile.Close()
		t.Fatalf("encode cert: %v", err)
	}
	certFile.Close()

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyFile, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		keyFile.Close()
		t.Fatalf("encode key: %v", err)
	}
	keyFile.Close()
	return certPath, keyPath
}

// pollUntilServing repeatedly TCP-dials addr until it accepts a connection.
// The listener helper is asynchronous, so the test cannot assume the socket
// is ready immediately after RunListeners returns.
func pollUntilServing(t *testing.T, addr string) {
	t.Helper()
	deadline := 5 * time.Second
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond) // poll interval inside pollUntilServing
	}
	t.Fatalf("server at %s never became ready within %s", addr, deadline)
}

// TestRunListeners_PlainHTTPStartAndShutdown asserts the helper starts a
// plain-HTTP listener, serves at least one request, and exits cleanly when
// the parent context is canceled.
func TestRunListeners_PlainHTTPStartAndShutdown(t *testing.T) {
	// Not t.Parallel: freePort closes its socket before RunListeners rebinds,
	// so two parallel tests within this package could end up racing for the
	// same OS-allocated port. Serializing the real-socket tests closes that
	// window without changing RunListeners' public API.
	port := freePort(t)
	cfg := &config.Config{
		Server: config.ServerConfig{Port: port},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunListeners(ctx, cfg, handler, discardLogger()) }()

	addr := "127.0.0.1:" + strconv.Itoa(port)
	pollUntilServing(t, addr)

	resp, err := http.Get("http://" + addr + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d; want %d", resp.StatusCode, http.StatusNoContent)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunListeners returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunListeners did not exit within 5s of cancel")
	}
}

// TestRunListeners_TLSStartAndShutdown asserts the helper serves HTTPS when
// cert and key are configured and exits cleanly on context cancel.
func TestRunListeners_TLSStartAndShutdown(t *testing.T) {
	// Not t.Parallel: see TestRunListeners_PlainHTTPStartAndShutdown.
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	port := freePort(t)
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port: port,
			TLS: config.TLSConfig{
				CertFile: certPath,
				KeyFile:  keyPath,
			},
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunListeners(ctx, cfg, handler, discardLogger()) }()

	addr := "127.0.0.1:" + strconv.Itoa(port)
	pollUntilServing(t, addr)

	// Skip cert verification: the test cert is self-signed.
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTPS GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatal("response was not TLS")
	}
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS version = %x; want >= TLS 1.2", resp.TLS.Version)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunListeners returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunListeners did not exit within 5s of cancel")
	}
}

// TestRunListeners_TLSCollapseToServerPort asserts that when TLS is
// configured but TLS.Port is unset, HTTPS binds Server.Port (the collapse
// semantics documented in the M47 plan).
func TestRunListeners_TLSCollapseToServerPort(t *testing.T) {
	// Not t.Parallel: see TestRunListeners_PlainHTTPStartAndShutdown.
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	serverPort := freePort(t)
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port: serverPort,
			TLS: config.TLSConfig{
				CertFile: certPath,
				KeyFile:  keyPath,
				// Port intentionally 0 -- collapse to Server.Port.
			},
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunListeners(ctx, cfg, handler, discardLogger()) }()

	addr := "127.0.0.1:" + strconv.Itoa(serverPort)
	pollUntilServing(t, addr)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("HTTPS GET on Server.Port: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunListeners returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunListeners did not exit within 5s of cancel")
	}
}

// TestRunListeners_TLSSplitPort asserts that when TLS.Port is set, HTTPS
// binds the TLS port and Server.Port has no listener. Without this assertion
// the collapse-port and split-port branches are observationally identical in
// the test suite (both serve HTTPS), letting a regression that flips the
// branch slip past every other test.
func TestRunListeners_TLSSplitPort(t *testing.T) {
	// Not t.Parallel: see TestRunListeners_PlainHTTPStartAndShutdown.
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	serverPort := freePort(t)
	tlsPort := freePort(t)
	if serverPort == tlsPort {
		t.Skip("freePort returned identical ports; rerun")
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port: serverPort,
			TLS: config.TLSConfig{
				CertFile: certPath,
				KeyFile:  keyPath,
				Port:     tlsPort,
			},
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunListeners(ctx, cfg, handler, discardLogger()) }()

	tlsAddr := "127.0.0.1:" + strconv.Itoa(tlsPort)
	pollUntilServing(t, tlsAddr)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("https://" + tlsAddr + "/")
	if err != nil {
		t.Fatalf("HTTPS GET on TLS.Port: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}

	// Server.Port must have no listener: a TCP dial should fail. Use a
	// short deadline so a stuck dial does not hang the test.
	dialer := &net.Dialer{Timeout: 250 * time.Millisecond}
	conn, err := dialer.Dial("tcp", "127.0.0.1:"+strconv.Itoa(serverPort))
	if err == nil {
		conn.Close()
		t.Errorf("Server.Port :%d unexpectedly accepted a connection; split-port mode should bind only TLS.Port", serverPort)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunListeners returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunListeners did not exit within 5s of cancel")
	}
}

// freeUDPPort returns a port number that is free on BOTH TCP and UDP. The
// HTTP/3 listener test reuses one port for HTTPS (TCP) and QUIC (UDP), so a
// UDP-only probe could hand back a number already bound by another TCP
// listener and the HTTPS bind would race-fail before QUIC is exercised.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 100; i++ {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen(tcp): %v", err)
		}
		port := tcp.Addr().(*net.TCPAddr).Port

		udp, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			_ = udp.Close()
			_ = tcp.Close()
			return port
		}
		_ = tcp.Close()
	}
	t.Fatal("failed to reserve a port free on both TCP and UDP")
	return 0
}

// TestEffectiveHTTP3Port covers the resolver that picks the UDP port for the
// HTTP/3 listener. Order: HTTP3.Port > TLS.Port > Server.Port. Disabled and
// TLS-not-configured both resolve to 0.
func TestEffectiveHTTP3Port(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *config.Config
		want int
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: 0,
		},
		{
			name: "disabled",
			cfg: &config.Config{
				Server: config.ServerConfig{Port: 1973, TLS: config.TLSConfig{CertFile: "c", KeyFile: "k"}},
			},
			want: 0,
		},
		{
			name: "enabled but no TLS",
			cfg: &config.Config{
				Server: config.ServerConfig{Port: 1973, HTTP3: config.HTTP3Config{Enabled: true}},
			},
			want: 0,
		},
		{
			name: "explicit HTTP3 port wins",
			cfg: &config.Config{
				Server: config.ServerConfig{
					Port:  1973,
					TLS:   config.TLSConfig{CertFile: "c", KeyFile: "k", Port: 443},
					HTTP3: config.HTTP3Config{Enabled: true, Port: 8443},
				},
			},
			want: 8443,
		},
		{
			name: "fall back to TLS port",
			cfg: &config.Config{
				Server: config.ServerConfig{
					Port:  1973,
					TLS:   config.TLSConfig{CertFile: "c", KeyFile: "k", Port: 443},
					HTTP3: config.HTTP3Config{Enabled: true},
				},
			},
			want: 443,
		},
		{
			name: "fall back to Server.Port (collapse)",
			cfg: &config.Config{
				Server: config.ServerConfig{
					Port:  1973,
					TLS:   config.TLSConfig{CertFile: "c", KeyFile: "k"},
					HTTP3: config.HTTP3Config{Enabled: true},
				},
			},
			want: 1973,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveHTTP3Port(tt.cfg); got != tt.want {
				t.Errorf("EffectiveHTTP3Port = %d; want %d", got, tt.want)
			}
		})
	}
}

// TestRunListeners_HTTP3RoundTrip starts the listener helper with HTTP/3
// enabled, dials with a quic-go HTTP/3 client, and verifies a successful
// response. Sharing one port across TCP (HTTPS) and UDP (QUIC) mirrors the
// production "advertise via Alt-Svc" topology.
func TestRunListeners_HTTP3RoundTrip(t *testing.T) {
	// Not t.Parallel: the freePort/freeUDPPort dance closes its sockets
	// before RunListeners rebinds, so we serialize real-socket tests.
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	port := freeUDPPort(t)
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port: port,
			TLS: config.TLSConfig{
				CertFile: certPath,
				KeyFile:  keyPath,
			},
			HTTP3: config.HTTP3Config{Enabled: true},
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("h3-ok"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunListeners(ctx, cfg, handler, discardLogger()) }()

	// QUIC has no TCP-style accept-on-ready signal. Give the UDP listener a
	// brief grace period to enter its accept loop, then issue the request
	// (which has its own timeout/retry loop below). Cannot be replaced with a
	// deterministic poll because there is no observable ready-state to probe.
	time.Sleep(100 * time.Millisecond)

	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		},
	}
	defer tr.Close()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	addr := "127.0.0.1:" + strconv.Itoa(port)
	// Retry briefly: the http3.Server.ListenAndServe goroutine may not yet
	// have its UDP socket bound the first time we dial.
	var resp *http.Response
	var lastErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.Get("https://" + addr + "/")
		if err == nil {
			resp = r
			break
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond) // poll interval inside HTTP/3 retry loop
	}
	if resp == nil {
		cancel()
		// Bound the wait on shutdown: if RunListeners hangs on close (a real
		// regression CR has flagged before), we want a fast-fail with both
		// errors visible rather than blocking until the global go test
		// timeout fires.
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("HTTP/3 GET never succeeded: %v (RunListeners error: %v)", lastErr, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("HTTP/3 GET never succeeded: %v; RunListeners did not exit within 5s of cancel", lastErr)
		}
		t.Fatalf("HTTP/3 GET never succeeded: %v", lastErr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if resp.ProtoMajor != 3 {
		t.Errorf("response proto = %s (major=%d); want HTTP/3", resp.Proto, resp.ProtoMajor)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(resp.Body): %v", err)
	}
	if string(body) != "h3-ok" {
		t.Errorf("body = %q; want h3-ok", string(body))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunListeners returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunListeners did not exit within 5s of cancel")
	}
}

// TestRunListeners_NilArgs covers the defensive nil checks at the top of
// RunListeners. Each branch returns an error rather than panicking so a
// caller bug does not crash the whole binary.
func TestRunListeners_NilArgs(t *testing.T) {
	t.Parallel()
	logger := discardLogger()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	cfg := &config.Config{Server: config.ServerConfig{Port: 1}}
	if err := RunListeners(context.Background(), nil, handler, logger); err == nil {
		t.Error("nil cfg: want error, got nil")
	}
	if err := RunListeners(context.Background(), cfg, nil, logger); err == nil {
		t.Error("nil handler: want error, got nil")
	}
	if err := RunListeners(context.Background(), cfg, handler, nil); err == nil {
		t.Error("nil logger: want error, got nil")
	}
}

// TestRedirectHandler covers the redirect target construction across the host
// shapes the handler must support: bare hostname, hostname with explicit port,
// IPv4 literal, and bracketed IPv6 literal. Each row asserts that the 301
// status fires and the Location header points to the right HTTPS URL.
func TestRedirectHandler(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		httpsPort  int
		hostHeader string
		requestURI string
		wantLoc    string
	}{
		{
			name:       "bare host, default https port omits :443",
			httpsPort:  443,
			hostHeader: "example.com",
			requestURI: "/artists",
			wantLoc:    "https://example.com/artists",
		},
		{
			name:       "host with explicit :80 stripped, redirect to non-default port",
			httpsPort:  1973,
			hostHeader: "example.com:80",
			requestURI: "/",
			wantLoc:    "https://example.com:1973/",
		},
		{
			name:       "preserves query string",
			httpsPort:  443,
			hostHeader: "example.com",
			requestURI: "/search?q=test&page=2",
			wantLoc:    "https://example.com/search?q=test&page=2",
		},
		{
			name:       "IPv4 literal with port",
			httpsPort:  443,
			hostHeader: "127.0.0.1:80",
			requestURI: "/healthz",
			wantLoc:    "https://127.0.0.1/healthz",
		},
		{
			name:       "IPv6 literal with port preserves brackets",
			httpsPort:  443,
			hostHeader: "[::1]:80",
			requestURI: "/",
			wantLoc:    "https://[::1]/",
		},
		{
			name:       "non-default https port appears in target",
			httpsPort:  8443,
			hostHeader: "stillwater.local",
			requestURI: "/api/v1/health",
			wantLoc:    "https://stillwater.local:8443/api/v1/health",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := redirectHandler(tc.httpsPort)
			req := &http.Request{
				Method:     http.MethodGet,
				Host:       tc.hostHeader,
				RequestURI: tc.requestURI,
				URL:        mustParseURL(t, tc.requestURI),
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMovedPermanently {
				t.Errorf("status = %d; want 301", rec.Code)
			}
			gotLoc := rec.Header().Get("Location")
			if gotLoc != tc.wantLoc {
				t.Errorf("Location = %q; want %q", gotLoc, tc.wantLoc)
			}
		})
	}
}

// TestRunListeners_RedirectIntegration asserts the redirect listener actually
// runs alongside the HTTPS listener and returns 301 with the right Location
// over a real socket. Without this, the unit test above passes even if the
// listener never registers (buildEntries gating bug).
func TestRunListeners_RedirectIntegration(t *testing.T) {
	// Not t.Parallel: see TestRunListeners_PlainHTTPStartAndShutdown.
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	tlsPort := freePort(t)
	redirectPort := freePort(t)
	if tlsPort == redirectPort {
		t.Skip("freePort returned identical ports; rerun")
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port: freePort(t),
			TLS: config.TLSConfig{
				CertFile: certPath,
				KeyFile:  keyPath,
				Port:     tlsPort,
			},
			HTTPRedirect: config.HTTPRedirectConfig{Port: redirectPort},
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunListeners(ctx, cfg, handler, discardLogger()) }()

	redirectAddr := "127.0.0.1:" + strconv.Itoa(redirectPort)
	tlsAddr := "127.0.0.1:" + strconv.Itoa(tlsPort)
	pollUntilServing(t, redirectAddr)
	pollUntilServing(t, tlsAddr)

	// Verify the HTTPS sibling is actually serving. pollUntilServing only
	// confirms the TCP socket is bound; without a real HTTPS round-trip a
	// regression that stopped registering the TLS listener would still let
	// the redirect-Location assertion pass (the URL is computed, not chased).
	httpsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	tlsResp, err := httpsClient.Get("https://" + tlsAddr + "/")
	if err != nil {
		t.Fatalf("HTTPS GET on TLS listener: %v", err)
	}
	tlsResp.Body.Close()
	if tlsResp.StatusCode != http.StatusOK {
		t.Fatalf("TLS listener status = %d; want 200", tlsResp.StatusCode)
	}

	// Use a client that does NOT follow redirects; we want to inspect the
	// Location header directly, not chase it into the TLS listener.
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get("http://" + redirectAddr + "/foo?bar=baz")
	if err != nil {
		t.Fatalf("HTTP GET on redirect listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("status = %d; want 301", resp.StatusCode)
	}
	gotLoc := resp.Header.Get("Location")
	wantLoc := "https://127.0.0.1:" + strconv.Itoa(tlsPort) + "/foo?bar=baz"
	if gotLoc != wantLoc {
		t.Errorf("Location = %q; want %q", gotLoc, wantLoc)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunListeners returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunListeners did not exit within 5s of cancel")
	}
}

// TestRunListeners_RedirectSkippedWithoutTLS asserts the redirect listener is
// NOT registered when TLS is not configured, even if HTTPRedirect.Port is set.
// (The config validator rejects this combination separately; this guard is
// inside buildEntries as belt-and-suspenders.)
func TestRunListeners_RedirectSkippedWithoutTLS(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:         9999,
			HTTPRedirect: config.HTTPRedirectConfig{Port: 8080},
		},
	}
	handler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	entries, err := buildEntries(cfg, handler, discardLogger())
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("buildEntries returned %d entries; want 1 (redirect must be skipped without TLS)", len(entries))
	}
	for _, e := range entries {
		if e.name == "http-redirect" {
			t.Errorf("redirect listener registered without TLS configured")
		}
	}
}

// TestBuildEntries_NoACMEReturnsSinglePrimary asserts the default
// (non-ACME) configuration registers one listener -- the primary HTTP or
// HTTPS server. Pinning the count keeps the listener layer's contract with
// future PRs (#929 redirect, #932 HTTP/3) explicit.
func TestBuildEntries_NoACMEReturnsSinglePrimary(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: config.ServerConfig{Port: 1973}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	entries, err := buildEntries(cfg, handler, discardLogger())
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("len(entries) = %d; want %d", got, want)
	}
	if entries[0].name != "http" {
		t.Errorf("primary listener name = %q; want %q", entries[0].name, "http")
	}
}

// TestBuildEntries_ACMERegistersChallengeListener asserts that turning on
// ACME registers TWO listeners: the HTTPS primary (https-acme) plus the
// dedicated plain-HTTP HTTP-01 challenge listener (acme-challenge). Without
// the second listener the certificate authority cannot fetch challenge
// tokens and renewals silently fail.
func TestBuildEntries_ACMERegistersChallengeListener(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := &config.Config{
		Server:   config.ServerConfig{Port: 1973},
		Database: config.DatabaseConfig{Path: filepath.Join(tmp, "stillwater.db")},
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	entries, err := buildEntries(cfg, handler, discardLogger())
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	if got, want := len(entries), 2; got != want {
		t.Fatalf("len(entries) = %d; want %d", got, want)
	}
	hasPrimary := false
	hasChallenge := false
	for _, e := range entries {
		if e.name == "https-acme" {
			hasPrimary = true
		}
		if e.name == "acme-challenge" {
			hasChallenge = true
		}
	}
	if !hasPrimary {
		t.Errorf("missing https-acme entry; got %+v", entries)
	}
	if !hasChallenge {
		t.Errorf("missing acme-challenge entry; got %+v", entries)
	}
}

// TestBuildEntries_ACMEChallengeReusesRedirectPort asserts the challenge
// listener binds SW_HTTP_REDIRECT_PORT when the operator set it, rather
// than double-binding the default port 80 alongside a future #929
// redirect listener. The challenge handler's autocert.HTTPHandler(nil)
// already 301s non-challenge requests to HTTPS, so the redirect listener's
// behavior is subsumed when ACME is on.
func TestBuildEntries_ACMEChallengeReusesRedirectPort(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:         1973,
			HTTPRedirect: config.HTTPRedirectConfig{Port: 8080},
		},
		Database: config.DatabaseConfig{Path: filepath.Join(tmp, "stillwater.db")},
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	entries, err := buildEntries(cfg, handler, discardLogger())
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	var challengeAddr string
	for _, e := range entries {
		if e.name == "acme-challenge" {
			challengeAddr = e.addr
		}
	}
	if challengeAddr != ":8080" {
		t.Errorf("challenge listener addr = %q; want %q (re-using HTTPRedirect.Port)", challengeAddr, ":8080")
	}
}

// TestRunListeners_BindFailureSurfacesError asserts a non-graceful start
// error (port already in use) propagates as the function's return value.
// The test binds the wildcard ":<port>" upfront so RunListeners' wildcard
// bind on the same port collides; binding two sockets on the same wildcard
// fails on every supported platform whereas wildcard-vs-loopback binding
// races silently on macOS.
func TestRunListeners_BindFailureSurfacesError(t *testing.T) {
	t.Parallel()
	hold, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer hold.Close()
	port := hold.Addr().(*net.TCPAddr).Port
	cfg := &config.Config{Server: config.ServerConfig{Port: port}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = RunListeners(ctx, cfg, handler, discardLogger())
	if err == nil {
		t.Fatal("RunListeners returned nil; want bind error")
	}
}

// TestRedirectHandler_RejectsBadInputs covers the defensive guards: a
// non-origin-form RequestURI (CONNECT/OPTIONS shapes) and a malformed Host
// header must return 400, not splice into a malformed Location.
func TestRedirectHandler_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		host       string
		requestURI string
	}{
		{name: "RequestURI without leading slash (CONNECT-like)", host: "example.com", requestURI: "example.com:443"},
		{name: "RequestURI = *", host: "example.com", requestURI: "*"},
		{name: "Host with whitespace", host: "evil .com", requestURI: "/foo"},
		{name: "Host with slash", host: "evil/com", requestURI: "/foo"},
		{name: "Empty Host", host: "", requestURI: "/foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := redirectHandler(443)
			req := &http.Request{Method: http.MethodGet, Host: tc.host, RequestURI: tc.requestURI}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400 (Location = %q)", rec.Code, rec.Header().Get("Location"))
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("Location = %q; want empty", loc)
			}
		})
	}
}

// TestRunListeners_RedirectShutdownPropagation verifies that canceling the
// parent context tears down BOTH the HTTPS and the redirect listener -- not
// just the one we happen to dial in TestRunListeners_RedirectIntegration.
// Without this, a future regression where buildRedirectListener wires a no-op
// shutdown would pass the integration test but leave the redirect socket
// listening forever.
func TestRunListeners_RedirectShutdownPropagation(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	tlsPort := freePort(t)
	redirectPort := freePort(t)
	if tlsPort == redirectPort {
		t.Skip("freePort returned identical ports; rerun")
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:         freePort(t),
			TLS:          config.TLSConfig{CertFile: certPath, KeyFile: keyPath, Port: tlsPort},
			HTTPRedirect: config.HTTPRedirectConfig{Port: redirectPort},
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunListeners(ctx, cfg, handler, discardLogger()) }()
	pollUntilServing(t, "127.0.0.1:"+strconv.Itoa(redirectPort))

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunListeners did not exit within 5s of cancel")
	}

	// Re-dial both ports; both should refuse within ~250ms.
	for _, p := range []int{redirectPort, tlsPort} {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(p), 250*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Errorf("port %d still accepting connections after shutdown", p)
		}
	}
}

// TestRunListeners_RedirectBindFailureCancelsHTTPS asserts the errgroup
// contract: if the redirect listener cannot bind (port already held), the
// HTTPS sibling is also torn down so the operator sees a single fatal error
// instead of a half-running daemon.
func TestRunListeners_RedirectBindFailureCancelsHTTPS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	hold, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer hold.Close()
	redirectPort := hold.Addr().(*net.TCPAddr).Port
	tlsPort := freePort(t)
	if tlsPort == redirectPort {
		t.Skip("freePort collision with held port; rerun")
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:         freePort(t),
			TLS:          config.TLSConfig{CertFile: certPath, KeyFile: keyPath, Port: tlsPort},
			HTTPRedirect: config.HTTPRedirectConfig{Port: redirectPort},
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = RunListeners(ctx, cfg, handler, discardLogger())
	if err == nil {
		t.Fatal("RunListeners returned nil; want bind error from redirect listener")
	}
	// HTTPS sibling must also be down.
	conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(tlsPort), 250*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		t.Errorf("HTTPS port %d still accepting connections after sibling bind failure", tlsPort)
	}
}

// mustParseURL is a tiny helper so the redirect-handler table tests can
// populate http.Request.URL without bubbling parse errors through every row.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		t.Fatalf("url.ParseRequestURI(%q): %v", raw, err)
	}
	return u
}
