package server

import (
	"bytes"
	"mime"
	"path"
	"regexp"
	"strings"
	"unicode"
)

// Preserve the existing TeamHarness artifact publication checks when automatic
// file projection moves from the MCP publisher into the common task service.
var artifactSensitiveName = regexp.MustCompile(`(?i)(secret|token|cookie|authorization|private[_-]?key|credential|client[_-]?secret)`)
var artifactSensitiveText = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\bAuthorization\s*:\s*(?:Bearer|Basic)\s+(\S+)`),
	regexp.MustCompile(`(?i)\b(?:access[_-]?key[_-]?secret|client[_-]?secret|secret[_-]?key|api[_-]?key|token)\b\s*[:=]\s*['"]?([A-Za-z0-9_./+=:-]{16,})`),
}
var artifactPlaceholderMarker = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])(your|my|our|paste|insert|replace|placeholder|example|sample|dummy|fake|redacted|masked|change[-_]?me|todo|tbd|here|xxx|yyy|zzz)([^A-Za-z0-9]|$)`)
var artifactLongCredential = regexp.MustCompile(`[A-Za-z0-9]{16,}`)

func artifactPlaceholder(value string) bool {
	value = strings.Trim(value, "'\"")
	inner, excluded := "", ""
	switch {
	case strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">"):
		inner, excluded = value[1:len(value)-1], "<>"
	case len(value) >= 2 && strings.HasPrefix(value, "`") && strings.HasSuffix(value, "`"):
		inner, excluded = value[1:len(value)-1], "`"
	case len(value) >= 4 && strings.HasPrefix(value, "**") && strings.HasSuffix(value, "**"):
		inner, excluded = value[2:len(value)-2], "*"
	default:
		return false
	}
	return inner != "" && !strings.ContainsAny(inner, excluded) && strings.IndexFunc(inner, unicode.IsSpace) < 0 && artifactPlaceholderMarker.MatchString(inner) && !artifactLongCredential.MatchString(inner)
}

func artifactMediaType(name string) string {
	value := mime.TypeByExtension(path.Ext(name))
	if value == "" {
		value = "application/octet-stream"
	}
	return value
}

func artifactSensitive(name, mediaType string, data []byte) bool {
	if artifactSensitiveName.MatchString(name) {
		return true
	}
	text := strings.HasPrefix(mediaType, "text/")
	switch strings.Split(mediaType, ";")[0] {
	case "application/json", "application/javascript", "application/xml", "application/x-yaml", "application/yaml", "application/toml":
		text = true
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".cfg", ".conf", ".css", ".csv", ".html", ".ini", ".js", ".json", ".jsx", ".log", ".md", ".py", ".rb", ".sh", ".sql", ".toml", ".ts", ".tsx", ".txt", ".xml", ".yaml", ".yml":
		text = true
	}
	sample := data[:min(len(data), 256*1024)]
	if !text || bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	for _, pattern := range artifactSensitiveText {
		for _, match := range pattern.FindAllSubmatch(sample, -1) {
			value := match[0]
			if len(match) > 1 {
				value = match[1]
			}
			if !artifactPlaceholder(string(value)) {
				return true
			}
		}
	}
	return false
}
