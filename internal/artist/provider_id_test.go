package artist

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestProviderID_MarshalJSON pins the JSON shape of ProviderID: the exact key
// names and the omitempty behavior of FetchedAt. A change to a JSON tag here
// would be an API-breaking change, so this guards against accidental drift.
func TestProviderID_MarshalJSON(t *testing.T) {
	t.Parallel()

	fetched := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name string
		in   ProviderID
		// wantContains are substrings that MUST appear in the marshaled JSON.
		wantContains []string
		// wantOmitted are substrings that must NOT appear (e.g. omitted keys).
		wantOmitted []string
	}{
		{
			name: "all fields populated",
			in: ProviderID{
				Provider:   "musicbrainz",
				ProviderID: "abc-123",
				FetchedAt:  &fetched,
			},
			wantContains: []string{
				`"provider":"musicbrainz"`,
				`"provider_id":"abc-123"`,
				`"fetched_at":"2024-01-02T03:04:05Z"`,
			},
		},
		{
			name: "nil FetchedAt drops the key (omitempty)",
			in: ProviderID{
				Provider:   "discogs",
				ProviderID: "42",
				FetchedAt:  nil,
			},
			wantContains: []string{
				`"provider":"discogs"`,
				`"provider_id":"42"`,
			},
			wantOmitted: []string{`"fetched_at"`},
		},
		{
			name: "zero value keeps the two non-omitempty keys",
			in:   ProviderID{},
			wantContains: []string{
				`"provider":""`,
				`"provider_id":""`,
			},
			wantOmitted: []string{`"fetched_at"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("json.Marshal(%+v) returned error: %v", tt.in, err)
			}
			gotStr := string(got)

			for _, want := range tt.wantContains {
				if !strings.Contains(gotStr, want) {
					t.Errorf("marshaled JSON %s missing substring %q", gotStr, want)
				}
			}
			for _, omit := range tt.wantOmitted {
				if strings.Contains(gotStr, omit) {
					t.Errorf("marshaled JSON %s unexpectedly contains %q", gotStr, omit)
				}
			}
		})
	}
}

// TestProviderID_RoundTrip verifies that marshaling a ProviderID to JSON and
// unmarshaling it back yields an equal value, so the wire format is lossless.
func TestProviderID_RoundTrip(t *testing.T) {
	t.Parallel()

	fetched := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name string
		in   ProviderID
	}{
		{
			name: "all fields populated",
			in: ProviderID{
				Provider:   "musicbrainz",
				ProviderID: "abc-123",
				FetchedAt:  &fetched,
			},
		},
		{
			name: "nil FetchedAt",
			in: ProviderID{
				Provider:   "discogs",
				ProviderID: "42",
				FetchedAt:  nil,
			},
		},
		{
			name: "zero value",
			in:   ProviderID{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("json.Marshal(%+v) returned error: %v", tt.in, err)
			}

			var got ProviderID
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(%s) returned error: %v", data, err)
			}

			if got.Provider != tt.in.Provider {
				t.Errorf("Provider = %q, want %q", got.Provider, tt.in.Provider)
			}
			if got.ProviderID != tt.in.ProviderID {
				t.Errorf("ProviderID = %q, want %q", got.ProviderID, tt.in.ProviderID)
			}
			switch {
			case tt.in.FetchedAt == nil && got.FetchedAt != nil:
				t.Errorf("FetchedAt = %v, want nil", got.FetchedAt)
			case tt.in.FetchedAt != nil && got.FetchedAt == nil:
				t.Errorf("FetchedAt = nil, want %v", tt.in.FetchedAt)
			case tt.in.FetchedAt != nil && got.FetchedAt != nil && !got.FetchedAt.Equal(*tt.in.FetchedAt):
				t.Errorf("FetchedAt = %v, want %v", got.FetchedAt, tt.in.FetchedAt)
			}
		})
	}
}

// TestProviderID_UnmarshalFromJSON pins the inbound mapping: external JSON keys
// must populate the expected struct fields.
func TestProviderID_UnmarshalFromJSON(t *testing.T) {
	t.Parallel()

	const raw = `{"provider":"fanart","provider_id":"xyz","fetched_at":"2023-05-06T07:08:09Z"}`

	var got ProviderID
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", raw, err)
	}

	if got.Provider != "fanart" {
		t.Errorf("Provider = %q, want %q", got.Provider, "fanart")
	}
	if got.ProviderID != "xyz" {
		t.Errorf("ProviderID = %q, want %q", got.ProviderID, "xyz")
	}
	if got.FetchedAt == nil {
		t.Fatalf("FetchedAt = nil, want a non-nil time")
	}
	want := time.Date(2023, time.May, 6, 7, 8, 9, 0, time.UTC)
	if !got.FetchedAt.Equal(want) {
		t.Errorf("FetchedAt = %v, want %v", got.FetchedAt, want)
	}
}
