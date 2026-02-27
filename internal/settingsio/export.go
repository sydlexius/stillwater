package settingsio

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/internal/webhook"
	"golang.org/x/crypto/pbkdf2"
)

// pbkdf2Iterations is the OWASP-recommended iteration count for PBKDF2-SHA256.
const pbkdf2Iterations = 600_000

// Envelope is the outer JSON wrapper for an exported settings file.
type Envelope struct {
	Version    string `json:"version"`
	AppVersion string `json:"app_version"`
	CreatedAt  string `json:"created_at"`
	Salt       string `json:"salt"` // base64-encoded PBKDF2 salt
	Data       string `json:"data"` // base64-encoded nonce+ciphertext
}

// Payload is the decrypted inner content of an export.
type Payload struct {
	Settings           map[string]string  `json:"settings"`
	Connections        []ConnectionExport `json:"connections"`
	PlatformProfiles   []platform.Profile `json:"platform_profiles"`
	Webhooks           []webhook.Webhook  `json:"webhooks"`
	ProviderKeys       map[string]string  `json:"provider_keys"`
	ProviderPriorities []PriorityExport   `json:"provider_priorities"`
}

// ConnectionExport is a connection with its API key decrypted for export.
type ConnectionExport struct {
	Name                 string `json:"name"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	APIKey               string `json:"api_key"`
	Enabled              bool   `json:"enabled"`
	FeatureLibraryImport bool   `json:"feature_library_import"`
	FeatureNFOWrite      bool   `json:"feature_nfo_write"`
	FeatureImageWrite    bool   `json:"feature_image_write"`
}

// PriorityExport holds a field's provider priority list.
type PriorityExport struct {
	Field     string                  `json:"field"`
	Providers []provider.ProviderName `json:"providers"`
	Disabled  []provider.ProviderName `json:"disabled,omitempty"`
}

// ImportResult summarizes what was imported.
type ImportResult struct {
	Settings     int `json:"settings"`
	Connections  int `json:"connections"`
	Profiles     int `json:"platform_profiles"`
	Webhooks     int `json:"webhooks"`
	ProviderKeys int `json:"provider_keys"`
	Priorities   int `json:"priorities"`
}

// Service handles settings export and import.
type Service struct {
	db               *sql.DB
	providerSettings *provider.SettingsService
	connectionSvc    *connection.Service
	platformSvc      *platform.Service
	webhookSvc       *webhook.Service
}

// NewService creates a settings export/import service.
func NewService(
	db *sql.DB,
	ps *provider.SettingsService,
	cs *connection.Service,
	ps2 *platform.Service,
	ws *webhook.Service,
) *Service {
	return &Service{
		db:               db,
		providerSettings: ps,
		connectionSvc:    cs,
		platformSvc:      ps2,
		webhookSvc:       ws,
	}
}

// Export collects all settings data, encrypts it with the given passphrase,
// and returns an Envelope. The passphrase is used with PBKDF2 to derive an
// AES-256-GCM key, making exports portable across instances.
func (s *Service) Export(ctx context.Context, passphrase string) (*Envelope, error) {
	payload := Payload{
		Settings:     make(map[string]string),
		ProviderKeys: make(map[string]string),
	}

	// Collect settings from the KV table
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("querying settings: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scanning setting: %w", err)
		}
		payload.Settings[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating settings: %w", err)
	}

	// Collect and decrypt provider API keys
	keyStatuses, err := s.providerSettings.ListProviderKeyStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing provider keys: %w", err)
	}
	for _, ks := range keyStatuses {
		if ks.Status == "unconfigured" {
			continue
		}
		key, err := s.providerSettings.GetAPIKey(ctx, ks.Name)
		if err != nil || key == "" {
			continue
		}
		payload.ProviderKeys[string(ks.Name)] = key
	}

	// Collect connections with decrypted API keys
	conns, err := s.connectionSvc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing connections: %w", err)
	}
	for _, c := range conns {
		payload.Connections = append(payload.Connections, ConnectionExport{
			Name:                 c.Name,
			Type:                 c.Type,
			URL:                  c.URL,
			APIKey:               c.APIKey,
			Enabled:              c.Enabled,
			FeatureLibraryImport: c.FeatureLibraryImport,
			FeatureNFOWrite:      c.FeatureNFOWrite,
			FeatureImageWrite:    c.FeatureImageWrite,
		})
	}

	// Collect platform profiles
	profiles, err := s.platformSvc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing platform profiles: %w", err)
	}
	payload.PlatformProfiles = profiles

	// Collect webhooks
	webhooks, err := s.webhookSvc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing webhooks: %w", err)
	}
	payload.Webhooks = webhooks

	// Collect provider priorities
	priorities, err := s.providerSettings.GetPriorities(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting priorities: %w", err)
	}
	for _, p := range priorities {
		payload.ProviderPriorities = append(payload.ProviderPriorities, PriorityExport{
			Field:     p.Field,
			Providers: p.Providers,
			Disabled:  p.Disabled,
		})
	}

	// Marshal and encrypt payload
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling payload: %w", err)
	}

	data, salt, err := encryptWithPassphrase(payloadJSON, passphrase)
	if err != nil {
		return nil, fmt.Errorf("encrypting payload: %w", err)
	}

	envelope := &Envelope{
		Version:    "1.0",
		AppVersion: version.Version,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Salt:       salt,
		Data:       data,
	}

	return envelope, nil
}

// Import decrypts and applies settings from an Envelope using the given
// passphrase. The passphrase must match the one used during export.
func (s *Service) Import(ctx context.Context, env *Envelope, passphrase string) (*ImportResult, error) {
	if env.Data == "" {
		return nil, fmt.Errorf("empty export data")
	}

	// Decrypt
	plaintext, err := decryptWithPassphrase(env.Data, env.Salt, passphrase)
	if err != nil {
		return nil, fmt.Errorf("decrypting export data (wrong passphrase?): %w", err)
	}

	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return nil, fmt.Errorf("parsing export payload: %w", err)
	}

	result := &ImportResult{}

	// Import settings
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range payload.Settings {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now)
		if err != nil {
			return nil, fmt.Errorf("upserting setting %q: %w", k, err)
		}
		result.Settings++
	}

	// Import provider keys
	for name, key := range payload.ProviderKeys {
		if err := s.providerSettings.SetAPIKey(ctx, provider.ProviderName(name), key); err != nil {
			return nil, fmt.Errorf("setting provider key %q: %w", name, err)
		}
		result.ProviderKeys++
	}

	// Import connections: match by type + url, update or insert
	for _, ce := range payload.Connections {
		existing, err := s.connectionSvc.GetByTypeAndURL(ctx, ce.Type, ce.URL)
		if err != nil {
			return nil, fmt.Errorf("looking up connection %q: %w", ce.Name, err)
		}
		if existing != nil {
			// Update existing
			existing.Name = ce.Name
			existing.APIKey = ce.APIKey
			existing.Enabled = ce.Enabled
			existing.FeatureLibraryImport = ce.FeatureLibraryImport
			existing.FeatureNFOWrite = ce.FeatureNFOWrite
			existing.FeatureImageWrite = ce.FeatureImageWrite
			if err := s.connectionSvc.Update(ctx, existing); err != nil {
				return nil, fmt.Errorf("updating connection %q: %w", ce.Name, err)
			}
		} else {
			// Create new
			c := &connection.Connection{
				Name:                 ce.Name,
				Type:                 ce.Type,
				URL:                  ce.URL,
				APIKey:               ce.APIKey,
				Enabled:              ce.Enabled,
				FeatureLibraryImport: ce.FeatureLibraryImport,
				FeatureNFOWrite:      ce.FeatureNFOWrite,
				FeatureImageWrite:    ce.FeatureImageWrite,
			}
			if err := s.connectionSvc.Create(ctx, c); err != nil {
				return nil, fmt.Errorf("creating connection %q: %w", ce.Name, err)
			}
		}
		result.Connections++
	}

	// Import platform profiles: match by name, update or insert
	for _, p := range payload.PlatformProfiles {
		existing, err := s.platformSvc.GetByName(ctx, p.Name)
		if err != nil {
			return nil, fmt.Errorf("looking up platform profile %q: %w", p.Name, err)
		}
		if existing != nil {
			p.ID = existing.ID
			if err := s.platformSvc.Update(ctx, &p); err != nil {
				return nil, fmt.Errorf("updating platform profile %q: %w", p.Name, err)
			}
		} else {
			p.ID = ""          // Let Create generate a new ID
			p.IsActive = false // Avoid creating multiple active profiles on import
			if err := s.platformSvc.Create(ctx, &p); err != nil {
				return nil, fmt.Errorf("creating platform profile %q: %w", p.Name, err)
			}
		}
		result.Profiles++
	}

	// Import webhooks: match by name + url, upsert
	for _, w := range payload.Webhooks {
		existing, err := s.webhookSvc.GetByNameAndURL(ctx, w.Name, w.URL)
		if err != nil {
			return nil, fmt.Errorf("looking up webhook %q: %w", w.Name, err)
		}
		if existing != nil {
			w.ID = existing.ID
			if err := s.webhookSvc.Update(ctx, &w); err != nil {
				return nil, fmt.Errorf("updating webhook %q: %w", w.Name, err)
			}
		} else {
			w.ID = "" // Let Create generate a new ID
			if err := s.webhookSvc.Create(ctx, &w); err != nil {
				return nil, fmt.Errorf("creating webhook %q: %w", w.Name, err)
			}
		}
		result.Webhooks++
	}

	// Import provider priorities
	for _, p := range payload.ProviderPriorities {
		if err := s.providerSettings.SetPriority(ctx, p.Field, p.Providers); err != nil {
			return nil, fmt.Errorf("setting priority for %q: %w", p.Field, err)
		}
		if len(p.Disabled) > 0 {
			if err := s.providerSettings.SetDisabledProviders(ctx, p.Field, p.Disabled); err != nil {
				return nil, fmt.Errorf("setting disabled providers for %q: %w", p.Field, err)
			}
		}
		result.Priorities++
	}

	return result, nil
}

// deriveKey uses PBKDF2-SHA256 to derive a 32-byte AES-256 key from a
// passphrase and salt.
func deriveKey(passphrase string, salt []byte) []byte {
	return pbkdf2.Key([]byte(passphrase), salt, pbkdf2Iterations, 32, sha256.New)
}

// encryptWithPassphrase encrypts plaintext using a passphrase-derived
// AES-256-GCM key. Returns base64-encoded ciphertext and salt.
func encryptWithPassphrase(plaintext []byte, passphrase string) (data, salt string, err error) {
	saltBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, saltBytes); err != nil {
		return "", "", fmt.Errorf("generating salt: %w", err)
	}

	key := deriveKey(passphrase, saltBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", fmt.Errorf("creating GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext),
		base64.StdEncoding.EncodeToString(saltBytes),
		nil
}

// decryptWithPassphrase decrypts base64-encoded ciphertext using a
// passphrase-derived AES-256-GCM key with the given base64-encoded salt.
func decryptWithPassphrase(data, salt, passphrase string) ([]byte, error) {
	saltBytes, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return nil, fmt.Errorf("decoding salt: %w", err)
	}

	key := deriveKey(passphrase, saltBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("decoding ciphertext: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypting (wrong passphrase?): %w", err)
	}

	return plaintext, nil
}
