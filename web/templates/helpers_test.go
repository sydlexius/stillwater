package templates

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

func TestMirrorServerType(t *testing.T) {
	tests := []struct {
		name   string
		mirror *provider.MirrorConfig
		want   string
	}{
		{"nil mirror is official", nil, "official"},
		{"beta URL is beta", &provider.MirrorConfig{BaseURL: betaMirrorURL, RateLimit: 1}, "beta"},
		{"custom URL is custom", &provider.MirrorConfig{BaseURL: "http://192.168.1.126:5000/ws/2", RateLimit: 10}, "custom"},
		{"official URL is custom", &provider.MirrorConfig{BaseURL: "https://musicbrainz.org/ws/2", RateLimit: 1}, "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mirrorServerType(tt.mirror)
			if got != tt.want {
				t.Errorf("mirrorServerType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMirrorStatusLabel(t *testing.T) {
	tests := []struct {
		name   string
		mirror *provider.MirrorConfig
		want   string
	}{
		{"nil mirror has no label", nil, ""},
		{"beta shows label", &provider.MirrorConfig{BaseURL: betaMirrorURL, RateLimit: 1}, "Beta server"},
		{"custom shows label", &provider.MirrorConfig{BaseURL: "http://10.0.0.1:5000/ws/2", RateLimit: 10}, "Custom mirror"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mirrorStatusLabel(tt.mirror)
			if got != tt.want {
				t.Errorf("mirrorStatusLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
