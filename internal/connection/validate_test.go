package connection

import (
	"testing"
)

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		// Valid URLs
		{name: "http localhost", input: "http://localhost:8096", want: "http://localhost:8096"},
		{name: "https domain", input: "https://emby.example.com", want: "https://emby.example.com"},
		{name: "http private IP with port", input: "http://192.168.1.100:8096", want: "http://192.168.1.100:8096"},
		{name: "http 10.x network", input: "http://10.0.0.50:8686", want: "http://10.0.0.50:8686"},
		{name: "http loopback", input: "http://127.0.0.1:8096", want: "http://127.0.0.1:8096"},
		{name: "http IPv6 loopback", input: "http://[::1]:8096", want: "http://[::1]:8096"},
		{name: "trailing slash stripped", input: "http://emby:8096/", want: "http://emby:8096"},
		{name: "scheme lowercased", input: "HTTP://EMBY:8096", want: "http://EMBY:8096"},
		{name: "path in base URL allowed", input: "https://emby.local/emby", want: "https://emby.local/emby"},
		{name: "path with trailing slash", input: "https://emby.local/emby/", want: "https://emby.local/emby"},

		// Invalid URLs
		{name: "empty string", input: "", wantErr: true},
		{name: "ftp scheme", input: "ftp://files.example.com", wantErr: true},
		{name: "file scheme", input: "file:///etc/passwd", wantErr: true},
		{name: "gopher scheme", input: "gopher://evil.com", wantErr: true},
		{name: "javascript scheme", input: "javascript:alert(1)", wantErr: true},
		{name: "empty host", input: "http://", wantErr: true},
		{name: "userinfo present", input: "http://user:pass@emby:8096", wantErr: true},
		{name: "query string", input: "http://emby:8096?foo=bar", wantErr: true},
		{name: "fragment", input: "http://emby:8096#frag", wantErr: true},
		{name: "missing scheme", input: "://missing-scheme", wantErr: true},
		{name: "not a URL", input: "not-a-url", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateBaseURL(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateBaseURL(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ValidateBaseURL(%q) error = %v, want nil", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("ValidateBaseURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConnectionValidate_URL(t *testing.T) {
	tests := []struct {
		name    string
		conn    Connection
		wantErr bool
		wantURL string
	}{
		{
			name: "valid connection with trailing slash stripped",
			conn: Connection{
				Name:   "test",
				Type:   TypeEmby,
				URL:    "http://localhost:8096/",
				APIKey: "test-key",
			},
			wantURL: "http://localhost:8096",
		},
		{
			name: "valid connection with scheme lowercased",
			conn: Connection{
				Name:   "test",
				Type:   TypeJellyfin,
				URL:    "HTTP://192.168.1.100:8096",
				APIKey: "test-key",
			},
			wantURL: "http://192.168.1.100:8096",
		},
		{
			name: "rejects ftp scheme",
			conn: Connection{
				Name:   "test",
				Type:   TypeEmby,
				URL:    "ftp://files.example.com",
				APIKey: "test-key",
			},
			wantErr: true,
		},
		{
			name: "rejects userinfo",
			conn: Connection{
				Name:   "test",
				Type:   TypeLidarr,
				URL:    "http://user:pass@lidarr:8686",
				APIKey: "test-key",
			},
			wantErr: true,
		},
		{
			name: "rejects empty URL",
			conn: Connection{
				Name:   "test",
				Type:   TypeEmby,
				URL:    "",
				APIKey: "test-key",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.conn.Validate()
			if tt.wantErr {
				if err == nil {
					t.Errorf("Validate() = nil, want error")
				}
				return
			}
			if err != nil {
				t.Errorf("Validate() error = %v, want nil", err)
				return
			}
			if tt.conn.URL != tt.wantURL {
				t.Errorf("after Validate(), URL = %q, want %q", tt.conn.URL, tt.wantURL)
			}
		})
	}
}
