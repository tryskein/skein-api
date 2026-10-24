package attachments

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ValidationError is returned by validators in this package. Code is the
// stable machine-readable error string the REST/MCP layers map directly
// onto their problem-details body.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// IsValidationError unwraps a ValidationError for the typical "if v, ok"
// idiom used elsewhere in this codebase.
func IsValidationError(err error) (*ValidationError, bool) {
	var v *ValidationError
	if errors.As(err, &v) {
		return v, true
	}
	return nil, false
}

// MaxFilenameLength matches the practical S3 / Linux fs limit and keeps
// the UI from rendering pathological names.
const MaxFilenameLength = 255

// MaxContentTypeLength bounds what we accept as MIME — anything longer
// is almost certainly user input pasted into the wrong field.
const MaxContentTypeLength = 255

// blockedExtensions is the deny-list (G-4 (a) — internal tracker, low
// risk so we deny only executables). Compared case-insensitively against
// the filename suffix. Any extension not on this list is allowed.
//
// The list mirrors the spec's "block executables" rule. We compare against
// the EXTENSION ONLY (the trailing dot-suffix), not the MIME type, because
// MIME values are user-controlled and trivially spoofable.
//
// Coverage targets the standard Unix + macOS + Windows executable surface
// (.exe / .com / .scr / .pif / .cpl / .hta / .lnk / .reg / .vbs / .jar)
// without trying to be comprehensive — the threat model is "stop a casual
// drag-of-malware-into-the-tracker", not "defend against a determined
// attacker who can rename files".
var blockedExtensions = map[string]struct{}{
	// Unix / macOS
	".sh": {}, ".so": {}, ".app": {}, ".deb": {}, ".rpm": {}, ".dmg": {},
	// Windows native executables + DLLs
	".exe": {}, ".dll": {}, ".com": {}, ".scr": {}, ".pif": {},
	// Windows scripts / shortcuts / installers / control-panel files
	".bat": {}, ".cmd": {}, ".ps1": {}, ".vbs": {}, ".vbe": {},
	".js": {}, ".jse": {}, ".wsf": {}, ".wsh": {}, ".msi": {},
	".msp": {}, ".mst": {}, ".cpl": {}, ".hta": {}, ".lnk": {},
	".reg": {}, ".msc": {},
	// JVM / cross-platform launchers
	".jar": {},
}

// ValidateFilename canonicalises and checks the user-supplied filename.
// Returns the trimmed filename or a ValidationError. The filename is kept
// verbatim (case preserved) for the Content-Disposition header on
// download; we only reject obvious abuse.
func ValidateFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", &ValidationError{Code: "invalid_filename", Message: "filename is required"}
	}
	if !utf8.ValidString(name) {
		return "", &ValidationError{Code: "invalid_filename", Message: "filename must be valid UTF-8"}
	}
	if utf8.RuneCountInString(name) > MaxFilenameLength {
		return "", &ValidationError{
			Code:    "invalid_filename",
			Message: "filename too long",
		}
	}
	// Reject path separators, NUL, control chars. Path separators in
	// particular would let a malicious caller try to inject a forged S3
	// key — even though we ALSO sanitise the s3 key build path, defence
	// in depth here costs nothing.
	for _, r := range name {
		if r < 0x20 || r == '/' || r == '\\' {
			return "", &ValidationError{
				Code:    "invalid_filename",
				Message: "filename contains illegal characters",
			}
		}
	}
	// Block executables. Always test the LAST extension; the ".tar.gz"
	// case still passes because ".gz" isn't on the list.
	ext := strings.ToLower(filepath.Ext(name))
	if _, blocked := blockedExtensions[ext]; blocked {
		return "", &ValidationError{
			Code:    "mime_blocked",
			Message: "file type is not allowed (executable)",
		}
	}
	return name, nil
}

// ValidateContentType checks the MIME string is well-formed and the
// trailing extension on the filename is not on the deny-list. Note: the
// blocked-extensions check is on the FILENAME (handled by ValidateFilename)
// — this validator only sanity-checks the MIME shape.
//
// We do NOT enforce a MIME allow-list (G-4 (a) — deny-list policy).
// MIME is mostly informational; the real gate is the extension list.
func ValidateContentType(ct string) (string, error) {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return "", &ValidationError{Code: "invalid_content_type", Message: "contentType is required"}
	}
	if len(ct) > MaxContentTypeLength {
		return "", &ValidationError{
			Code:    "invalid_content_type",
			Message: "contentType too long",
		}
	}
	if !utf8.ValidString(ct) {
		return "", &ValidationError{
			Code:    "invalid_content_type",
			Message: "contentType must be valid UTF-8",
		}
	}
	// MIME shape: type "/" subtype, e.g. "image/png". Be lenient —
	// allow "+" and "." in the subtype (e.g. "application/vnd.api+json").
	if !strings.Contains(ct, "/") {
		return "", &ValidationError{
			Code:    "invalid_content_type",
			Message: "contentType must be of form type/subtype",
		}
	}
	for _, r := range ct {
		if r < 0x20 {
			return "", &ValidationError{
				Code:    "invalid_content_type",
				Message: "contentType contains control characters",
			}
		}
	}
	return ct, nil
}

// ValidateSize checks the user-declared size against the API cap. Zero is
// rejected (an empty file is almost certainly a client bug or abuse).
func ValidateSize(size int64, maxSize int64) error {
	if size <= 0 {
		return &ValidationError{Code: "invalid_size", Message: "sizeBytes must be > 0"}
	}
	if size > maxSize {
		return &ValidationError{Code: "file_too_large", Message: "file exceeds the maximum allowed size"}
	}
	return nil
}
