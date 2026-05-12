package encryption

import (
	"encoding/base64"
	"testing"
)

// FuzzEncryptionDecrypt feeds arbitrary strings to Encryptor.Decrypt to verify
// it never panics. Every stored secret in Stillwater (provider API keys, OIDC
// client secret, federated-auth tokens) flows through this function, so
// panic-safety is a hard requirement.
//
// The target function does: base64 decode -> length check against nonce size ->
// AES-GCM Open. Seeds exercise truncated nonces, bad padding, empty input,
// Unicode, CRLF-embedded base64, and ciphertexts with swapped AAD or truncated
// tags.
func FuzzEncryptionDecrypt(f *testing.F) {
	// Build a working Encryptor to generate valid seed ciphertexts.
	// NewEncryptor("") generates a random 32-byte key and returns it
	// base64-encoded alongside the Encryptor. The returned key is not used
	// here because we only need the Encryptor for seed generation.
	enc, _, err := NewEncryptor("")
	if err != nil {
		f.Fatalf("constructing test Encryptor: %v", err)
	}

	// Seed 1: a valid Encrypt() output -- the happy-path round-trip case.
	validCiphertext, err := enc.Encrypt("stillwater-test-secret")
	if err != nil {
		f.Fatalf("generating valid ciphertext seed: %v", err)
	}
	f.Add(validCiphertext)

	// Seed 2: empty string -- base64 decode succeeds (empty), then the
	// length check fires immediately.
	f.Add("")

	// Seed 3: base64 with invalid padding -- decode should return an error,
	// not a panic.
	f.Add("bm90dmFsaWRwYWRkaW5nIQ")

	// Seed 4: base64 of a byte slice shorter than the nonce size (12 bytes
	// for GCM). Here we encode 5 bytes -- well under the 12-byte nonce.
	f.Add(base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0x03, 0x04, 0x05}))

	// Seed 5: base64 of exactly nonce-size bytes (12). After slicing out the
	// nonce, the remaining ciphertext is empty, exercising the boundary
	// between the length check and gcm.Open.
	f.Add(base64.StdEncoding.EncodeToString(make([]byte, 12)))

	// Seed 6: Unicode string -- not valid base64, exercises the decode error
	// path with multi-byte runes.
	f.Add("こんにちは世界")

	// Seed 7: CRLF-embedded base64 -- standard base64 decoders reject \r\n;
	// verifies the error path rather than a panic.
	legitB64 := base64.StdEncoding.EncodeToString([]byte("someplaintext"))
	f.Add(legitB64[:4] + "\r\n" + legitB64[4:])

	// Seed 8: ciphertext encrypted by a different Encryptor (swapped key /
	// different AAD context). gcm.Open must return an authentication error,
	// not a panic.
	enc2, _, err := NewEncryptor("")
	if err != nil {
		f.Fatalf("constructing second test Encryptor: %v", err)
	}
	altCiphertext, err := enc2.Encrypt("different-key-secret")
	if err != nil {
		f.Fatalf("generating alt-key ciphertext seed: %v", err)
	}
	f.Add(altCiphertext)

	// Seed 9: valid ciphertext with the GCM tag truncated by 1 byte. After
	// base64 re-encoding the result is a syntactically valid base64 string
	// whose ciphertext section is one byte too short for the 16-byte GCM tag,
	// so gcm.Open must return an authentication failure.
	rawValid, err := base64.StdEncoding.DecodeString(validCiphertext)
	if err != nil {
		f.Fatalf("decoding valid ciphertext for truncation seed: %v", err)
	}
	if len(rawValid) > 0 {
		truncated := rawValid[:len(rawValid)-1]
		f.Add(base64.StdEncoding.EncodeToString(truncated))
	}

	f.Fuzz(func(t *testing.T, encoded string) {
		// Decrypt must never panic regardless of input. Returning an error
		// for invalid, malformed, or unauthenticated ciphertext is correct.
		_, _ = enc.Decrypt(encoded)
	})
}
