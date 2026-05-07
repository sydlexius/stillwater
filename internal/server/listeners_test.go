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
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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
	deadline := 2 * time.Second
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready within %s", addr, deadline)
}

// TestRunListeners_PlainHTTPStartAndShutdown asserts the helper starts a
// plain-HTTP listener, serves at least one request, and exits cleanly when
// the parent context is canceled.
func TestRunListeners_PlainHTTPStartAndShutdown(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert is self-signed
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
	t.Parallel()
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
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert is self-signed
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
	<-done
}

// TestRunListeners_TLSSplitPort asserts that when TLS.Port is set, HTTPS
// binds the TLS port and Server.Port has no listener. Without this assertion
// the collapse-port and split-port branches are observationally identical in
// the test suite (both serve HTTPS), letting a regression that flips the
// branch slip past every other test.
func TestRunListeners_TLSSplitPort(t *testing.T) {
	t.Parallel()
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
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test cert is self-signed
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
	<-done
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
