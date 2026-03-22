package encryption

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewEncryptor_GeneratesKey(t *testing.T) {
	enc, key, err := NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor with empty key: %v", err)
	}
	if enc == nil {
		t.Fatal("expected non-nil Encryptor")
	}
	if key == "" {
		t.Fatal("expected generated key to be non-empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatalf("generated key is not valid base64: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("generated key length = %d, want 32", len(decoded))
	}
}

func TestNewEncryptor_InvalidKeyLength(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("tooshort"))
	_, _, err := NewEncryptor(short)
	if err == nil {
		t.Fatal("expected error for short key")
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("expected '32 bytes' in error, got: %v", err)
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	enc, _, err := NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	original := "secret api key value"
	ciphertext, err := enc.Encrypt(original)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ciphertext == original {
		t.Fatal("ciphertext should differ from plaintext")
	}

	decrypted, err := enc.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if decrypted != original {
		t.Fatalf("Decrypt = %q, want %q", decrypted, original)
	}
}

func TestEncryptDecrypt_EmptyPlaintext(t *testing.T) {
	enc, _, err := NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	ciphertext, err := enc.Encrypt("")
	if err != nil {
		t.Fatalf("Encrypt empty: %v", err)
	}

	decrypted, err := enc.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt empty: %v", err)
	}
	if decrypted != "" {
		t.Fatalf("Decrypt = %q, want empty string", decrypted)
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	enc1, _, err := NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor 1: %v", err)
	}
	enc2, _, err := NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor 2: %v", err)
	}

	ciphertext, err := enc1.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	_, err = enc2.Decrypt(ciphertext)
	if err == nil {
		t.Fatal("expected error decrypting with wrong key")
	}
}

func TestDecrypt_MalformedCiphertext(t *testing.T) {
	enc, _, err := NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	// Too short (less than nonce size)
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	_, err = enc.Decrypt(short)
	if err == nil {
		t.Fatal("expected error for short ciphertext")
	}

	// Invalid base64
	_, err = enc.Decrypt("not-valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestEncrypt_ProducesDifferentCiphertexts(t *testing.T) {
	enc, _, err := NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	plaintext := "same input"
	ct1, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	ct2, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}

	if ct1 == ct2 {
		t.Fatal("two encryptions of the same plaintext should produce different ciphertexts (random nonce)")
	}
}

func TestNewEncryptor_RawKey(t *testing.T) {
	// 32-byte raw string key that is not valid base64
	rawKey := "this-is-not-valid-base64!@#$%^&*"
	enc, _, err := NewEncryptor(rawKey)
	if err != nil {
		t.Fatalf("NewEncryptor with raw key: %v", err)
	}

	ct, err := enc.Encrypt("test")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "test" {
		t.Fatalf("Decrypt = %q, want %q", pt, "test")
	}
}
