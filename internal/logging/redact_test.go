package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestRedactingReplaceAttr_SensitiveFields verifies that every known sensitive
// field name produces a "[REDACTED]" value regardless of the original type.
func TestRedactingReplaceAttr_SensitiveFields(t *testing.T) {
	sensitive := []string{
		"api_key",
		"apikey",
		"api-key",
		"password",
		"passwd",
		"secret",
		"client_secret",
		"token",
		"access_token",
		"refresh_token",
		"private_key",
		"privatekey",
		"authorization",
		"hmac_secret",
	}

	for _, key := range sensitive {
		t.Run(key, func(t *testing.T) {
			a := slog.String(key, "supersecret")
			got := RedactingReplaceAttr(nil, a)
			if got.Value.String() != "[REDACTED]" {
				t.Errorf("key %q: expected [REDACTED], got %q", key, got.Value.String())
			}
		})
	}
}

// TestRedactingReplaceAttr_NonSensitiveFields verifies that non-sensitive
// fields are passed through unchanged.
func TestRedactingReplaceAttr_NonSensitiveFields(t *testing.T) {
	cases := []struct {
		key   string
		value string
	}{
		{"artist", "Beethoven"},
		{"user_id", "42"},
		{"component", "scanner"},
		{"level", "info"},
		{"message", "hello world"},
		{"duration_ms", "150"},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			a := slog.String(tc.key, tc.value)
			got := RedactingReplaceAttr(nil, a)
			if got.Value.String() != tc.value {
				t.Errorf("key %q: expected %q unchanged, got %q", tc.key, tc.value, got.Value.String())
			}
		})
	}
}

// TestRedactingReplaceAttr_CaseInsensitive verifies that key matching is
// case-insensitive so that API_KEY, Password, TOKEN etc. are all redacted.
func TestRedactingReplaceAttr_CaseInsensitive(t *testing.T) {
	cases := []string{
		"API_KEY",
		"ApiKey",
		"APIKEY",
		"Password",
		"PASSWORD",
		"TOKEN",
		"Token",
		"Authorization",
		"AUTHORIZATION",
		"Secret",
		"SECRET",
		"Client_Secret",
		"CLIENT_SECRET",
	}

	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			a := slog.String(key, "sensitive-value")
			got := RedactingReplaceAttr(nil, a)
			if got.Value.String() != "[REDACTED]" {
				t.Errorf("key %q: expected [REDACTED], got %q", key, got.Value.String())
			}
		})
	}
}

// TestRedactingReplaceAttr_NestedGroups verifies that sensitive fields are
// redacted even when the groups context is non-empty (i.e. nested in a group).
func TestRedactingReplaceAttr_NestedGroups(t *testing.T) {
	groups := []string{"connection", "credentials"}
	a := slog.String("api_key", "sk-abcdef")
	got := RedactingReplaceAttr(groups, a)
	if got.Value.String() != "[REDACTED]" {
		t.Errorf("nested group: expected [REDACTED], got %q", got.Value.String())
	}
}

// TestRedactingReplaceAttr_EmptyValue verifies that an empty string value is
// NOT changed to "[REDACTED]". Preserving zero values lets callers distinguish
// "field not set" from "field was redacted".
func TestRedactingReplaceAttr_EmptyValue(t *testing.T) {
	a := slog.String("api_key", "")
	got := RedactingReplaceAttr(nil, a)
	if got.Value.String() == "[REDACTED]" {
		t.Error("empty api_key value should not be changed to [REDACTED]")
	}
	// The key and empty value should be preserved as-is.
	if got.Key != "api_key" {
		t.Errorf("key changed unexpectedly: got %q", got.Key)
	}
}

// TestRedactingReplaceAttr_Integration constructs a real slog.Logger backed by
// a JSON handler with RedactingReplaceAttr wired in, logs a record that
// contains api_key="secret123", and asserts the raw secret does not appear in
// the captured output.
func TestRedactingReplaceAttr_Integration(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: RedactingReplaceAttr,
	})
	logger := slog.New(handler)

	logger.Info("connection established", "api_key", "secret123", "host", "example.com")

	output := buf.String()
	if strings.Contains(output, "secret123") {
		t.Errorf("raw secret found in log output: %s", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output but did not find it: %s", output)
	}
	// Non-sensitive fields must still appear.
	if !strings.Contains(output, "example.com") {
		t.Errorf("non-sensitive field 'host' missing from output: %s", output)
	}
}

// TestRedactingReplaceAttr_IntegrationPassword is a second integration test
// covering the password field specifically.
func TestRedactingReplaceAttr_IntegrationPassword(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: RedactingReplaceAttr,
	})
	logger := slog.New(handler)

	logger.Warn("auth attempt", "user", "alice", "password", "hunter2")

	output := buf.String()
	if strings.Contains(output, "hunter2") {
		t.Errorf("password found in log output: %s", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output: %s", output)
	}
	if !strings.Contains(output, "alice") {
		t.Errorf("non-sensitive 'user' field missing: %s", output)
	}
}
