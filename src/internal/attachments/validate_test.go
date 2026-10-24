package attachments

import (
	"strings"
	"testing"
)

func TestValidateFilename(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantCode  string
		wantValue string
	}{
		{"plain name", "screenshot.png", "", "screenshot.png"},
		{"trim leading/trailing spaces", "  doc.pdf  ", "", "doc.pdf"},
		{"unicode allowed", "Файл-1.txt", "", "Файл-1.txt"},
		{"empty", "", "invalid_filename", ""},
		{"all spaces", "    ", "invalid_filename", ""},
		{"path-traversal slash", "../etc/passwd", "invalid_filename", ""},
		{"path-traversal backslash", "..\\Windows\\system32", "invalid_filename", ""},
		{"control char", "bad\x01name.txt", "invalid_filename", ""},
		{"too long", strings.Repeat("a", 256) + ".txt", "invalid_filename", ""},
		{"blocked .exe", "virus.exe", "mime_blocked", ""},
		{"blocked .EXE upper", "virus.EXE", "mime_blocked", ""},
		{"blocked .sh", "shell.sh", "mime_blocked", ""},
		{"blocked .msi", "installer.msi", "mime_blocked", ""},
		{"tar.gz allowed (only last ext checked)", "build.tar.gz", "", "build.tar.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateFilename(tc.input)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected ok, got error: %v", err)
				}
				if got != tc.wantValue {
					t.Fatalf("got %q, want %q", got, tc.wantValue)
				}
				return
			}
			ve, ok := IsValidationError(err)
			if !ok {
				t.Fatalf("expected validation error, got %v", err)
			}
			if ve.Code != tc.wantCode {
				t.Fatalf("got code %q, want %q", ve.Code, tc.wantCode)
			}
		})
	}
}

func TestValidateContentType(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantCode string
	}{
		{"image/png", "image/png", ""},
		{"vnd+json", "application/vnd.api+json", ""},
		{"empty", "", "invalid_content_type"},
		{"no slash", "imagepng", "invalid_content_type"},
		{"control char", "image/png\x01", "invalid_content_type"},
		{"too long", strings.Repeat("a/b", 200), "invalid_content_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateContentType(tc.input)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected ok, got: %v", err)
				}
				return
			}
			ve, ok := IsValidationError(err)
			if !ok {
				t.Fatalf("expected validation error, got %v", err)
			}
			if ve.Code != tc.wantCode {
				t.Fatalf("got code %q, want %q", ve.Code, tc.wantCode)
			}
		})
	}
}

func TestValidateSize(t *testing.T) {
	const cap = 1000
	cases := []struct {
		name     string
		size     int64
		wantCode string
	}{
		{"ok", 500, ""},
		{"at cap", cap, ""},
		{"over cap", cap + 1, "file_too_large"},
		{"zero", 0, "invalid_size"},
		{"negative", -1, "invalid_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSize(tc.size, cap)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected ok, got: %v", err)
				}
				return
			}
			ve, ok := IsValidationError(err)
			if !ok {
				t.Fatalf("expected validation error, got %v", err)
			}
			if ve.Code != tc.wantCode {
				t.Fatalf("got code %q, want %q", ve.Code, tc.wantCode)
			}
		})
	}
}
