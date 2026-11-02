package server

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits for paths inside a human's file area (#1414 P1). The total bound
// covers the whole relative path; the segment bound matches the longest
// filename component commonly supported by POSIX and Windows filesystems.
const (
	maxHumanFilePathBytes    = 1024
	maxHumanPathSegmentBytes = 255
)

// Reserved top-level names inside a human's file area. They map to internal
// machinery (the recycle bin and a future sync state) and must not be
// addressable through the files API.
var reservedHumanFilePrefixes = []string{".trash", ".sync", ".control"}

// windowsReservedNames are the DOS device names Windows refuses as a file
// base name (CON.txt is reserved, myCON.txt is not). Checked case-
// insensitively after stripping any extension.
var windowsReservedNames = map[string]struct{}{
	"con": {}, "prn": {}, "aux": {}, "nul": {},
	"com1": {}, "com2": {}, "com3": {}, "com4": {},
	"com5": {}, "com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {},
	"lpt5": {}, "lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

// isWindowsReservedName reports whether seg is a reserved Windows device
// name, ignoring an extension ("CON" and "con.txt" are both reserved).
func isWindowsReservedName(seg string) bool {
	base := seg
	if i := strings.IndexByte(seg, '.'); i >= 0 {
		base = seg[:i]
	}
	if base == "" {
		return false
	}
	_, ok := windowsReservedNames[strings.ToLower(base)]
	return ok
}

// validateHumanFilePath normalizes and validates a relative path inside a
// human's file area. raw may be the empty string (the files root) or end
// with a single "/" (directory form, stripped by the caller's intent). It
// returns the normalized path — NFC, no leading or trailing slash, no
// "." segments — ready to be joined under the human's files/ prefix.
//
// Rejected: absolute paths, ".." and "." segments, empty segments
// ("a//b", "a/"), backslashes, control characters, non-UTF-8 input,
// total length over maxHumanFilePathBytes, any segment over
// maxHumanPathSegmentBytes, the reserved top-level names (.trash, .sync, .control),
// and Windows reserved device names.
func validateHumanFilePath(raw string) (string, error) {
	if utf8.ValidString(raw) == false {
		return "", fmt.Errorf("path contains invalid UTF-8")
	}
	if len(raw) > maxHumanFilePathBytes {
		return "", fmt.Errorf("path exceeds %d bytes", maxHumanFilePathBytes)
	}
	// Canonicalize first so every later check runs on one stable form.
	p := norm.NFC.String(raw)
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	if strings.Contains(p, "\\") {
		return "", fmt.Errorf("backslashes are not allowed in paths")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("control characters are not allowed in paths")
		}
	}
	// Directory form: exactly one trailing "/" is the directory marker and
	// is stripped here; a second one leaves an empty segment below.
	if strings.HasSuffix(p, "/") && !strings.Contains(p, "//") {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" {
		return "", nil // the files root
	}
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		if seg == "" {
			return "", fmt.Errorf("path contains an empty segment: %q", raw)
		}
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("path segment %q is not allowed", seg)
		}
		if len(seg) > maxHumanPathSegmentBytes {
			return "", fmt.Errorf("path segment exceeds %d bytes", maxHumanPathSegmentBytes)
		}
		if isWindowsReservedName(seg) {
			return "", fmt.Errorf("path segment %q is a reserved name", seg)
		}
		out = append(out, seg)
	}
	normalized := strings.Join(out, "/")
	for _, reserved := range reservedHumanFilePrefixes {
		if normalized == reserved || strings.HasPrefix(normalized, reserved+"/") {
			return "", fmt.Errorf("path %q is reserved", reserved)
		}
	}
	return normalized, nil
}
