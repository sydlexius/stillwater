package platform

import (
	"errors"
	"testing"
)

func TestNFOWriteAllowed(t *testing.T) {
	cases := []struct {
		name string
		prof *Profile
		err  error
		want bool
	}{
		{"plex disabled", &Profile{Name: "Plex", NFOEnabled: false}, nil, false},
		{"emby enabled", &Profile{Name: "Emby", NFOEnabled: true}, nil, true},
		{"nil profile fails open", nil, nil, true},
		{"error fails open", nil, errors.New("db down"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NFOWriteAllowed(tc.prof, tc.err); got != tc.want {
				t.Errorf("NFOWriteAllowed(%v,%v)=%v want %v", tc.prof, tc.err, got, tc.want)
			}
		})
	}
}
