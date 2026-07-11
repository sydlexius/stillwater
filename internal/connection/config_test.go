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
	}}
	if emby.GetPlatformUserID() != "u-1" {
		t.Errorf("GetPlatformUserID() = %q, want u-1", emby.GetPlatformUserID())
	}
	if emby.GetPlatformServerID() != "s-1" {
		t.Errorf("GetPlatformServerID() = %q, want s-1", emby.GetPlatformServerID())
	}
	if !emby.GetFeatureImageWrite() {
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

func TestValidate_NormalizesNilJellyfinConfig(t *testing.T) {
	// TypeJellyfin with nil Jellyfin must lazily allocate, matching the Lidarr
	// and Emby paths already covered by TestValidate_NormalizesNilMatchingConfig.
	c := &Connection{
		Name:   "fresh-jf",
		Type:   TypeJellyfin,
		URL:    "http://jf:8096",
		APIKey: "k",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for bare Jellyfin", err)
	}
	if c.Jellyfin == nil {
		t.Error("Validate() must allocate JellyfinConfig when nil")
	}
	if c.Emby != nil || c.Lidarr != nil {
		t.Error("Validate() must leave non-matching configs nil")
	}
}

func TestSetters_JellyfinPath(t *testing.T) {
	// SetPlatformUserID / SetPlatformServerID for Jellyfin must lazily allocate
	// JellyfinConfig and store into it.
	jf := &Connection{Type: TypeJellyfin}
	jf.SetPlatformUserID("u-jf")
	jf.SetPlatformServerID("s-jf")
	if jf.Jellyfin == nil {
		t.Fatal("SetPlatform* must allocate JellyfinConfig for TypeJellyfin")
	}
	if jf.Jellyfin.PlatformUserID != "u-jf" {
		t.Errorf("PlatformUserID = %q, want u-jf", jf.Jellyfin.PlatformUserID)
	}
	if jf.Jellyfin.PlatformServerID != "s-jf" {
		t.Errorf("PlatformServerID = %q, want s-jf", jf.Jellyfin.PlatformServerID)
	}
	if jf.Emby != nil || jf.Lidarr != nil {
		t.Error("SetPlatform* must not allocate non-matching configs")
	}

	// Lidarr has no platform server identity either.
	lidarr := &Connection{Type: TypeLidarr}
	lidarr.SetPlatformServerID("ignored")
	if lidarr.GetPlatformServerID() != "" {
		t.Error("Lidarr SetPlatformServerID should be a no-op")
	}
}

func TestSetFeatures_AllocatesAndSets(t *testing.T) {
	// SetFeatures on a bare Emby connection must lazily allocate EmbyConfig
	// and write the three surviving feature flags (imageWrite, metadataPush,
	// triggerRefresh).
	emby := &Connection{Type: TypeEmby}
	emby.SetFeatures(true, false, true)
	if emby.Emby == nil {
		t.Fatal("SetFeatures must allocate EmbyConfig for TypeEmby")
	}
	if !emby.Emby.FeatureImageWrite {
		t.Error("FeatureImageWrite must be true")
	}
	if emby.Emby.FeatureMetadataPush {
		t.Error("FeatureMetadataPush must be false")
	}
	if !emby.Emby.FeatureTriggerRefresh {
		t.Error("FeatureTriggerRefresh must be true")
	}

	// Jellyfin path: same contract.
	jf := &Connection{Type: TypeJellyfin}
	jf.SetFeatures(false, true, false)
	if jf.Jellyfin == nil {
		t.Fatal("SetFeatures must allocate JellyfinConfig for TypeJellyfin")
	}
	if jf.Jellyfin.FeatureImageWrite {
		t.Error("FeatureImageWrite must be false")
	}
	if !jf.Jellyfin.FeatureMetadataPush {
		t.Error("FeatureMetadataPush must be true")
	}

	// Lidarr has no features; SetFeatures must be a silent no-op.
	lidarr := &Connection{Type: TypeLidarr}
	lidarr.SetFeatures(true, true, true)
	if lidarr.Emby != nil || lidarr.Jellyfin != nil {
		t.Error("SetFeatures must not allocate any sub-config for Lidarr")
	}
	if lidarr.GetFeatureImageWrite() {
		t.Error("SetFeatures on Lidarr must have no observable effect")
	}
}
