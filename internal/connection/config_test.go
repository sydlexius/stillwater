package connection

import "testing"

// These tests pin the type-discriminated config sub-struct contract (#1686):
// platform-specific fields live on Lidarr/Emby/Jellyfin sub-configs, the
// nil-safe getters never panic regardless of Type, and Validate() enforces
// that exactly one sub-config (matching Type) is populated, lazily allocating
// the matching empty config when the caller left it nil (normalize()).

func TestGetters_NilSafeAcrossTypes(t *testing.T) {
	// A Lidarr connection has no Emby/Jellyfin config; the platform getters
	// must return zero values rather than panicking on a nil deref. This is
	// the exact shape imagebridge/bridge.go relies on when it iterates a
	// mixed-type connection list.
	lidarr := &Connection{Type: TypeLidarr, Lidarr: &LidarrConfig{VerifyPathAfterUpdate: true}}
	if lidarr.GetPlatformUserID() != "" {
		t.Errorf("Lidarr GetPlatformUserID() = %q, want empty", lidarr.GetPlatformUserID())
	}
	if lidarr.GetPlatformServerID() != "" {
		t.Errorf("Lidarr GetPlatformServerID() = %q, want empty", lidarr.GetPlatformServerID())
	}
	if lidarr.GetFeatureImageWrite() {
		t.Error("Lidarr GetFeatureImageWrite() = true, want false")
	}
	if !lidarr.GetVerifyPathAfterUpdate() {
		t.Error("Lidarr GetVerifyPathAfterUpdate() = false, want true")
	}

	// A connection whose matching config pointer is nil must also be safe.
	bare := &Connection{Type: TypeEmby}
	if bare.GetPlatformUserID() != "" || bare.GetFeatureImageWrite() {
		t.Error("bare Emby connection getters must return zero values, not panic")
	}
}

func TestGetters_ReadFromMatchingConfig(t *testing.T) {
	emby := &Connection{Type: TypeEmby, Emby: &EmbyConfig{
		PlatformUserID:    "u-1",
		PlatformServerID:  "s-1",
		FeatureImageWrite: true,
		FeatureNFOWrite:   true,
	}}
	if emby.GetPlatformUserID() != "u-1" {
		t.Errorf("GetPlatformUserID() = %q, want u-1", emby.GetPlatformUserID())
	}
	if emby.GetPlatformServerID() != "s-1" {
		t.Errorf("GetPlatformServerID() = %q, want s-1", emby.GetPlatformServerID())
	}
	if !emby.GetFeatureImageWrite() || !emby.GetFeatureNFOWrite() {
		t.Error("Emby feature getters should reflect the config")
	}

	jelly := &Connection{Type: TypeJellyfin, Jellyfin: &JellyfinConfig{PlatformUserID: "j-1"}}
	if jelly.GetPlatformUserID() != "j-1" {
		t.Errorf("Jellyfin GetPlatformUserID() = %q, want j-1", jelly.GetPlatformUserID())
	}
}

func TestSetters_AllocateMatchingConfig(t *testing.T) {
	// Setters on a bare connection must lazily allocate the correct config
	// based on Type and never touch the wrong one.
	emby := &Connection{Type: TypeEmby}
	emby.SetPlatformUserID("u-9")
	emby.SetPlatformServerID("s-9")
	if emby.Emby == nil || emby.Emby.PlatformUserID != "u-9" || emby.Emby.PlatformServerID != "s-9" {
		t.Fatalf("SetPlatform* did not populate EmbyConfig: %+v", emby.Emby)
	}
	if emby.Lidarr != nil || emby.Jellyfin != nil {
		t.Error("SetPlatform* must not allocate non-matching configs")
	}

	// Lidarr has no platform identity; setters are no-ops there.
	lidarr := &Connection{Type: TypeLidarr}
	lidarr.SetPlatformUserID("ignored")
	if lidarr.GetPlatformUserID() != "" {
		t.Error("Lidarr SetPlatformUserID should be a no-op")
	}
}

func TestValidate_RejectsMismatchedConfig(t *testing.T) {
	c := &Connection{
		Name:   "bad",
		Type:   TypeEmby,
		URL:    "http://emby:8096",
		APIKey: "k",
		Lidarr: &LidarrConfig{VerifyPathAfterUpdate: true}, // wrong platform
	}
	if err := c.Validate(); err == nil {
		t.Error("Validate() must reject an Emby connection carrying a LidarrConfig")
	}
}

func TestValidate_RejectsMultipleConfigs(t *testing.T) {
	c := &Connection{
		Name:     "bad",
		Type:     TypeEmby,
		URL:      "http://emby:8096",
		APIKey:   "k",
		Emby:     &EmbyConfig{},
		Jellyfin: &JellyfinConfig{}, // a second, non-matching config
	}
	if err := c.Validate(); err == nil {
		t.Error("Validate() must reject a connection with multiple sub-configs")
	}
}

func TestValidate_NormalizesNilMatchingConfig(t *testing.T) {
	// All sub-configs nil is the common construction case (e.g. a fresh
	// Lidarr connection, or the federated-login auto-provision path). Validate
	// must normalize by allocating the matching empty config, not reject.
	c := &Connection{
		Name:   "fresh",
		Type:   TypeLidarr,
		URL:    "http://lidarr:8686",
		APIKey: "k",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil (should normalize)", err)
	}
	if c.Lidarr == nil {
		t.Error("Validate() must allocate the matching LidarrConfig when nil")
	}
	if c.Emby != nil || c.Jellyfin != nil {
		t.Error("Validate() must leave non-matching configs nil")
	}
}
