package settingsio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fuzzPassphrase is a fixed passphrase used to encrypt all fuzz seeds so the
// decryption step succeeds and the fuzzer can explore the json.Unmarshal and
// per-section import paths. The passphrase is not a secret; its value only
// needs to be consistent between seed construction and the fuzz body.
const fuzzPassphrase = "fuzz-test-passphrase"

// encryptPayload marshals v to JSON, encrypts it with fuzzPassphrase, and
// returns the raw bytes of a JSON-encoded Envelope. The helper is used by
// seed construction to produce byte slices that f.Add can ingest.
func encryptPayload(f *testing.F, v any) []byte {
	f.Helper()
	plaintext, err := json.Marshal(v)
	if err != nil {
		f.Fatalf("marshaling fuzz seed payload: %v", err)
	}
	data, salt, err := encryptWithPassphrase(plaintext, fuzzPassphrase)
	if err != nil {
		f.Fatalf("encrypting fuzz seed: %v", err)
	}
	env := Envelope{
		Version: CurrentEnvelopeVersion,
		Salt:    salt,
		Data:    data,
	}
	b, err := json.Marshal(env)
	if err != nil {
		f.Fatalf("marshaling fuzz seed envelope: %v", err)
	}
	return b
}

// FuzzImportEnvelope feeds arbitrary byte slices to the settings import path.
// The import function must never panic regardless of input; returning an error
// for invalid or malformed input is the expected and correct behavior.
//
// The byte slice delivered by the fuzzer is treated as the JSON encoding of an
// Envelope (Path A). If it unmarshals to a structurally valid Envelope, Import
// is called directly, exercising the version guard, base64 decode, AES-GCM
// decrypt error path, and the ErrWrongPassphrase wrap.
//
// Path B (re-encrypting fuzz bytes as plaintext with fuzzPassphrase) was
// removed because encryptWithPassphrase + decryptWithPassphrase each run
// 600,000 PBKDF2 iterations, making the fuzz body ~770ms per iteration and
// yielding fewer than 80 executions in a 60-second CI cron run. The full
// import path (decrypt -> json.Unmarshal -> per-section import) is already
// covered by the encrypted seed corpus, which the fuzzer replays at no
// PBKDF2 cost. Path A alone reaches tens of thousands of executions per
// cron run, making mutation exploration practical.
func FuzzImportEnvelope(f *testing.F) {
	// Seed 1: a known-good encrypted export bundle with all sections present.
	// This exercises the full happy path through every import sub-function.
	f.Add(encryptPayload(f, Payload{
		Settings:     map[string]string{"key.one": "val1", "key.two": "val2"},
		Connections:  []ConnectionExport{{Name: "Emby", Type: "emby", URL: "http://emby.local:8096", APIKey: "k", Enabled: true}},
		ProviderKeys: map[string]string{"musicbrainz": "mbkey"},
		Rules: []RuleExport{
			{ID: "thumb_exists", Enabled: true, AutomationMode: "auto"},
			{ID: "biography_present", Enabled: false, AutomationMode: "manual"},
		},
		Libraries: []LibraryExport{
			{Name: "Music", Path: "/srv/music", Type: "regular", Source: "manual", FSWatch: 1, FSPollInterval: 60},
		},
		APITokens: []APITokenExport{
			{Name: "CI Token", TokenHash: "bcrypt$hash$here", Scopes: "read,write", Username: "admin", Status: "active"},
		},
		Users: []UserExport{
			{Username: "admin", Role: "administrator", IsActive: true, CreatedAt: "2024-01-01T00:00:00Z"},
		},
		UserPreferences: []UserPrefsExport{
			{Username: "admin", Preferences: map[string]string{"theme": "dark", "lang": "en"}},
		},
	}))

	// Seed 2: an unencrypted JSON envelope variant. The outer Data field
	// contains arbitrary non-base64 content to exercise the base64 decode error
	// path in decryptWithPassphrase and the early return from ImportWithOptions.
	f.Add([]byte(`{"version":"1.3","salt":"AAAAAAAAAAAAAAAAAAAAAA==","data":"not-valid-ciphertext"}`))

	// Seed 3: malformed AES-GCM ciphertext -- valid base64 but truncated so
	// the ciphertext is shorter than the GCM nonce size. Exercises the
	// "ciphertext too short" guard in decryptWithPassphrase.
	f.Add([]byte(`{"version":"1.3","salt":"AAAAAAAAAAAAAAAAAAAAAA==","data":"AAEC"}`))

	// Seed 4: bad nonce -- ciphertext is long enough to have a nonce but the
	// tag verification will fail, exercising the ErrWrongPassphrase wrap path.
	f.Add([]byte(`{"version":"1.3","salt":"AAAAAAAAAAAAAAAAAAAAAA==","data":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`))

	// Seed 5: JSON with truncated / empty arrays of LibraryExport and UserExport.
	// Exercises the per-section import loops with zero-length slices.
	f.Add(encryptPayload(f, Payload{
		Settings:        map[string]string{},
		Connections:     []ConnectionExport{},
		ProviderKeys:    map[string]string{},
		Libraries:       []LibraryExport{},
		APITokens:       []APITokenExport{},
		Users:           []UserExport{},
		UserPreferences: []UserPrefsExport{},
	}))

	// Seed 6: oversized arrays -- many entries per section to exercise loop
	// performance bounds and any per-item allocation paths.
	manyLibs := make([]LibraryExport, 100)
	for i := range manyLibs {
		manyLibs[i] = LibraryExport{
			Name:           strings.Repeat("L", i+1),
			Type:           "regular",
			Source:         "manual",
			FSPollInterval: 60,
		}
	}
	manyTokens := make([]APITokenExport, 100)
	for i := range manyTokens {
		manyTokens[i] = APITokenExport{
			Name:      strings.Repeat("T", i+1),
			TokenHash: strings.Repeat("h", i+1),
			Scopes:    "read,write",
			Username:  "ghost",
			Status:    "active",
		}
	}
	f.Add(encryptPayload(f, Payload{
		Libraries: manyLibs,
		APITokens: manyTokens,
	}))

	// Seed 7: large-value settings map -- 50 keys with long repeated-string
	// values, to push JSON allocation beyond typical sizes. Payload.Settings
	// is map[string]string, so true recursive nesting isn't expressible here;
	// this seed targets size pressure rather than structural depth.
	deepSettings := make(map[string]string, 50)
	for i := range 50 {
		deepSettings[strings.Repeat("k", i+1)] = strings.Repeat("v", 1000)
	}
	f.Add(encryptPayload(f, Payload{Settings: deepSettings}))

	// Seed 8: bundles with empty / blank / zero-value fields that trip
	// boundary checks inside importLibraries, importAPITokens, importUsers.
	f.Add(encryptPayload(f, Payload{
		Libraries: []LibraryExport{
			{Name: "", Type: "", Source: "", FSWatch: -1, FSPollInterval: -1}, // empty name => skip
			{Name: "A", Type: "unknown", Source: "unknown", FSWatch: 999, FSPollInterval: 9999},
		},
		APITokens: []APITokenExport{
			{Name: "blank-hash", TokenHash: "", Scopes: "", Username: "", Status: ""},           // empty hash => skip
			{Name: "bad-status", TokenHash: "h2", Scopes: "", Username: "ghost", Status: "???"}, // unknown status => "revoked"
		},
		Users: []UserExport{
			{Username: "", Role: "administrator"},                              // empty username => skip
			{Username: "u", Role: "superadmin", IsActive: true, CreatedAt: ""}, // unknown role => "operator"
		},
		Rules: []RuleExport{
			{ID: "", Enabled: true, AutomationMode: "auto"},                        // empty ID => skip
			{ID: "nonexistent_rule_xyz", Enabled: true, AutomationMode: "invalid"}, // bad mode => skip
		},
	}))

	// Seed 9: envelope with explicit Version "" to exercise the legacy
	// backward-compat path (treated as "1.0"). Built inline because
	// encryptPayload always stamps CurrentEnvelopeVersion; using it here
	// would silently miss the empty-version branch in Import.
	{
		legacyPayload, err := json.Marshal(map[string]any{
			"settings": map[string]string{"legacy": "yes"},
		})
		if err != nil {
			f.Fatalf("marshaling legacy fuzz seed payload: %v", err)
		}
		legacyData, legacySalt, err := encryptWithPassphrase(legacyPayload, fuzzPassphrase)
		if err != nil {
			f.Fatalf("encrypting legacy fuzz seed: %v", err)
		}
		legacyEnv, err := json.Marshal(Envelope{Version: "", Salt: legacySalt, Data: legacyData})
		if err != nil {
			f.Fatalf("marshaling legacy fuzz seed envelope: %v", err)
		}
		f.Add(legacyEnv)
	}

	// Seed 10: valid envelope JSON but with unexpected extra fields and
	// version "99.9" (unsupported) to exercise ErrUnsupportedVersion.
	f.Add([]byte(`{"version":"99.9","salt":"AAAAAAAAAAAAAAAAAAAAAA==","data":"AAEC","extra_field":"ignored"}`))

	// Seed 11: nil / empty Data field to trigger the "empty export data" guard.
	f.Add([]byte(`{"version":"1.3","salt":"AAAAAAAAAAAAAAAAAAAAAA==","data":""}`))

	// Seed 12: completely empty byte slice -- exercises the json.Unmarshal
	// error path in the outer envelope decode.
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		ctx := context.Background()

		// Path A: treat data as JSON encoding of an Envelope and call Import
		// directly. This exercises the full production path through
		// ImportWithOptions: nil/empty guard, version validation, base64 decode,
		// AES-GCM decrypt (always fails for non-seed inputs because the fuzz
		// engine cannot reverse PBKDF2, exercising ErrWrongPassphrase), and the
		// error-wrapping chain. For seed inputs the entire import path runs,
		// including json.Unmarshal on the inner Payload and every per-section
		// import function. The outer decode failing is expected and acceptable;
		// a panic is not.
		var env Envelope
		if json.Unmarshal(data, &env) == nil && env.Data != "" {
			db := setupTestDB(t)
			provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
			svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
			// Return value (error) is intentionally discarded: errors are
			// expected for malformed payloads. Only panics are bugs.
			_, _ = svc.Import(ctx, &env, fuzzPassphrase)
		}
	})
}
