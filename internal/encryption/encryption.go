package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Encryptor provides AES-256-GCM encryption and decryption.
type Encryptor struct {
	gcm cipher.AEAD
}

// NewEncryptor creates an Encryptor from a 32-byte key (base64-encoded or raw).
// If key is empty, it generates a random key and returns it encoded.
func NewEncryptor(key string) (*Encryptor, string, error) {
	var keyBytes []byte

	if key == "" {
		keyBytes = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, keyBytes); err != nil {
			return nil, "", fmt.Errorf("generating encryption key: %w", err)
		}
		key = base64.StdEncoding.EncodeToString(keyBytes)
	} else {
		decoded, err := base64.StdEncoding.DecodeString(key)
		if err != nil {
			// Try using the key as raw bytes (for testing)
			if len(key) == 32 {
				keyBytes = []byte(key)
			} else {
				return nil, "", fmt.Errorf("decoding encryption key: %w", err)
			}
		} else {
			keyBytes = decoded
		}
	}

	if len(keyBytes) != 32 {
		return nil, "", fmt.Errorf("encryption key must be 32 bytes, got %d", len(keyBytes))
	}

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, "", fmt.Errorf("creating AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", fmt.Errorf("creating GCM: %w", err)
	}

	return &Encryptor{gcm: gcm}, key, nil
}

// Encrypt encrypts plaintext and returns a base64-encoded ciphertext.
func (e *Encryptor) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := e.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a base64-encoded ciphertext and returns the plaintext.
func (e *Encryptor) Decrypt(encoded string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding ciphertext: %w", err)
	}

	nonceSize := e.gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := e.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypting: %w", err)
	}

	return string(plaintext), nil
}
