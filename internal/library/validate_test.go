package library

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePath(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "empty path allowed",
			input: "",
			want:  "",
		},
		{
			name:    "relative path rejected",
			input:   "music/lib",
			wantErr: true,
		},
		{
			name:    "dot-relative path rejected",
			input:   "./music/lib",
			wantErr: true,
		},
		{
			name:    "parent traversal rejected",
			input:   "../etc/passwd",
			wantErr: true,
		},
		{
			name:    "absolute parent traversal rejected",
			input:   tmpDir + string(filepath.Separator) + "..",
			wantErr: true,
		},
		{
			name:    "absolute embedded traversal rejected",
			input:   tmpDir + string(filepath.Separator) + ".." + string(filepath.Separator) + "etc",
			wantErr: true,
		},
		{
			name:  "absolute path accepted",
			input: tmpDir,
			want:  tmpDir,
		},
		{
			name:  "trailing slash cleaned",
			input: tmpDir + string(filepath.Separator),
			want:  tmpDir,
		},
		{
			name:  "redundant separators cleaned",
			input: tmpDir + string(filepath.Separator) + string(filepath.Separator) + ".",
			want:  tmpDir,
		},
		{
			name:  "dotdot in directory name accepted",
			input: tmpDir + "/..snapshots",
			want:  filepath.Join(tmpDir, "..snapshots"),
		},
		{
			name:  "dotdot suffix in directory name accepted",
			input: tmpDir + "/releases..old",
			want:  filepath.Join(tmpDir, "releases..old"),
		},
		{
			name:    "actual traversal segment rejected",
			input:   tmpDir + "/../etc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidatePath(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidatePath(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ValidatePath(%q) error = %v, want nil", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("ValidatePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCheckPathExists(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "not-a-dir.txt")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating temp file: %v", err)
	}

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "empty path rejected",
			input:   "",
			wantErr: true,
		},
		{
			name:    "relative path rejected",
			input:   "music/lib",
			wantErr: true,
		},
		{
			name:  "valid directory accepted",
			input: tmpDir,
		},
		{
			name:    "nonexistent path rejected",
			input:   filepath.Join(tmpDir, "no-such-dir"),
			wantErr: true,
		},
		{
			name:    "file not directory rejected",
			input:   tmpFile,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPathExists(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("CheckPathExists(%q) = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Errorf("CheckPathExists(%q) = %v, want nil", tt.input, err)
			}
		})
	}
}
