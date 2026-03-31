package image

import "testing"

func TestSlotAspectRatio(t *testing.T) {
	tests := []struct {
		slot string
		want float64
	}{
		{"thumb", 1.0},
		{"fanart", 16.0 / 9.0},
		{"banner", 5.4},
		{"logo", 0},
		{"unknown", 0},
	}
	for _, tt := range tests {
		got := SlotAspectRatio(tt.slot)
		if got != tt.want {
			t.Errorf("SlotAspectRatio(%q) = %f, want %f", tt.slot, got, tt.want)
		}
	}
}

func TestCheckGeometry(t *testing.T) {
	tests := []struct {
		name      string
		width     int
		height    int
		slot      string
		wantCrop  bool
		wantRatio float64
	}{
		{
			name:      "square thumb matches",
			width:     1000,
			height:    1000,
			slot:      "thumb",
			wantCrop:  false,
			wantRatio: 1.0,
		},
		{
			name:      "thumb within 10% tolerance",
			width:     1000,
			height:    950,
			slot:      "thumb",
			wantCrop:  false,
			wantRatio: 1.0,
		},
		{
			name:      "wide image for thumb needs crop",
			width:     1920,
			height:    1080,
			slot:      "thumb",
			wantCrop:  true,
			wantRatio: 1.0,
		},
		{
			name:      "16:9 fanart matches",
			width:     1920,
			height:    1080,
			slot:      "fanart",
			wantCrop:  false,
			wantRatio: 16.0 / 9.0,
		},
		{
			name:      "square image for fanart needs crop",
			width:     1000,
			height:    1000,
			slot:      "fanart",
			wantCrop:  true,
			wantRatio: 16.0 / 9.0,
		},
		{
			name:      "5.4:1 banner matches",
			width:     1080,
			height:    200,
			slot:      "banner",
			wantCrop:  false,
			wantRatio: 5.4,
		},
		{
			name:      "banner image too tall needs crop",
			width:     1000,
			height:    500,
			slot:      "banner",
			wantCrop:  true,
			wantRatio: 5.4,
		},
		{
			name:      "logo never needs crop",
			width:     800,
			height:    310,
			slot:      "logo",
			wantCrop:  false,
			wantRatio: 0,
		},
		{
			name:      "zero dimensions are accepted",
			width:     0,
			height:    0,
			slot:      "thumb",
			wantCrop:  false,
			wantRatio: 1.0,
		},
		{
			name:      "zero height is accepted",
			width:     100,
			height:    0,
			slot:      "thumb",
			wantCrop:  false,
			wantRatio: 1.0,
		},
		{
			name:      "unknown slot never needs crop",
			width:     1920,
			height:    1080,
			slot:      "unknown",
			wantCrop:  false,
			wantRatio: 0,
		},
		{
			name:      "tall image for thumb needs crop",
			width:     500,
			height:    1000,
			slot:      "thumb",
			wantCrop:  true,
			wantRatio: 1.0,
		},
		{
			name:      "slightly off 16:9 within tolerance",
			width:     1920,
			height:    1100,
			slot:      "fanart",
			wantCrop:  false, // 1.745 vs 1.778 is ~1.8% off, within 10%
			wantRatio: 16.0 / 9.0,
		},
		{
			name:      "thumb exactly 10% off does not need crop",
			width:     900,
			height:    1000,
			slot:      "thumb",
			wantCrop:  false, // 0.9 vs 1.0 is exactly 10%, within tolerance
			wantRatio: 1.0,
		},
		{
			name:      "thumb just over 10% off needs crop",
			width:     899,
			height:    1000,
			slot:      "thumb",
			wantCrop:  true, // 0.899 vs 1.0 is ~10.1%, exceeds 10% tolerance
			wantRatio: 1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CheckGeometry(tt.width, tt.height, tt.slot)
			if result.NeedsCrop != tt.wantCrop {
				t.Errorf("NeedsCrop = %v, want %v (actual ratio: %f, required: %f)",
					result.NeedsCrop, tt.wantCrop, result.ActualRatio, result.RequiredRatio)
			}
			if result.RequiredRatio != tt.wantRatio {
				t.Errorf("RequiredRatio = %f, want %f", result.RequiredRatio, tt.wantRatio)
			}
			if result.Width != tt.width || result.Height != tt.height {
				t.Errorf("dimensions = %dx%d, want %dx%d", result.Width, result.Height, tt.width, tt.height)
			}
		})
	}
}
