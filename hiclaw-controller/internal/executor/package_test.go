package executor

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteInlineConfigs_AllFields_OpenClaw(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "openclaw", "identity content", "soul content", "agents content")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "IDENTITY.md"), "identity content")
	assertFileContent(t, filepath.Join(dir, "SOUL.md"), "soul content")
	assertFileContains(t, filepath.Join(dir, "AGENTS.md"), "agents content")
}

func TestWriteInlineConfigs_AllFields_CoPaw(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "copaw", "identity content", "soul content", "agents content")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	// CoPaw: no IDENTITY.md
	if _, err := os.Stat(filepath.Join(dir, "IDENTITY.md")); err == nil {
		t.Error("IDENTITY.md should not exist for copaw runtime")
	}

	// SOUL.md should contain identity prepended to soul
	soulData, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		t.Fatalf("failed to read SOUL.md: %v", err)
	}
	soul := string(soulData)
	if !strings.HasPrefix(soul, "identity content") {
		t.Errorf("SOUL.md should start with identity content, got: %s", soul[:min(len(soul), 50)])
	}
	if !strings.Contains(soul, "soul content") {
		t.Error("SOUL.md should contain soul content")
	}

	assertFileContains(t, filepath.Join(dir, "AGENTS.md"), "agents content")
}

func TestWriteInlineConfigs_SoulOnly(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "", "", "soul only", "")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "SOUL.md"), "soul only")

	if _, err := os.Stat(filepath.Join(dir, "IDENTITY.md")); err == nil {
		t.Error("IDENTITY.md should not exist when identity is empty")
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err == nil {
		t.Error("AGENTS.md should not exist when agents is empty")
	}
}

func TestWriteInlineConfigs_OverridesExisting(t *testing.T) {
	dir := t.TempDir()

	// Pre-create files
	_ = os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("old soul"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("old agents"), 0o644)

	err := WriteInlineConfigs(dir, "", "", "new soul", "new agents")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "SOUL.md"), "new soul")
	assertFileContains(t, filepath.Join(dir, "AGENTS.md"), "new agents")

	// Verify old content is gone
	data, _ := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if strings.Contains(string(data), "old soul") {
		t.Error("SOUL.md should not contain old content")
	}
}

func TestWriteInlineConfigs_AgentsWrappedWithMarkers(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "", "", "", "custom agents rules")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("failed to read AGENTS.md: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "<!-- hiclaw-builtin-start -->") {
		t.Error("AGENTS.md should contain builtin-start marker")
	}
	if !strings.Contains(content, "<!-- hiclaw-builtin-end -->") {
		t.Error("AGENTS.md should contain builtin-end marker")
	}
	if !strings.Contains(content, "custom agents rules") {
		t.Error("AGENTS.md should contain custom content")
	}
}

func TestWriteInlineConfigs_CoPawMergesIdentityIntoSoul(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "copaw", "# Identity\nName: Alice", "# Role\nDevOps engineer", "")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		t.Fatalf("failed to read SOUL.md: %v", err)
	}
	content := string(data)

	// Identity should come before soul
	idxIdentity := strings.Index(content, "# Identity")
	idxRole := strings.Index(content, "# Role")
	if idxIdentity < 0 || idxRole < 0 {
		t.Fatalf("expected both identity and role in SOUL.md, got: %s", content)
	}
	if idxIdentity >= idxRole {
		t.Error("identity should be prepended before soul content")
	}
}

func TestWriteInlineConfigs_CoPawIdentityOnly(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "copaw", "identity only", "", "")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "SOUL.md"), "identity only")
}

func TestWriteInlineConfigs_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "agent")

	err := WriteInlineConfigs(dir, "", "", "soul", "")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "SOUL.md"), "soul")
}

func TestWriteInlineConfigs_EmptyFields(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "", "", "", "")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no files written when all fields empty, got %d", len(entries))
	}
}

func TestValidateNacosURI_FormatErrors(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		wantErr string
	}{
		{
			name:    "wrong scheme",
			uri:     "http://host:8848/ns/spec",
			wantErr: "scheme must be nacos://",
		},
		{
			name:    "missing host",
			uri:     "nacos:///ns/spec",
			wantErr: "missing host",
		},
		{
			name:    "missing namespace and spec (no path)",
			uri:     "nacos://host:8848",
			wantErr: "expected nacos://",
		},
		{
			name:    "missing spec name (only namespace)",
			uri:     "nacos://host:8848/ns",
			wantErr: "expected nacos://",
		},
		{
			name:    "empty string",
			uri:     "",
			wantErr: "scheme must be nacos://",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNacosURI(context.Background(), tt.uri)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestValidateNacosURI_ValidFormat_UnreachableServer(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{
			name: "basic host:port",
			uri:  "nacos://127.0.0.1:19999/ns/my-spec",
		},
		{
			name: "with credentials",
			uri:  "nacos://admin:secret@127.0.0.1:19999/ns/my-spec",
		},
		{
			name: "with version",
			uri:  "nacos://127.0.0.1:19999/ns/my-spec/v1.0.0",
		},
		{
			name: "with label version",
			uri:  "nacos://admin:pass@127.0.0.1:19999/ns/my-spec/label:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNacosURI(context.Background(), tt.uri)
			if err == nil {
				t.Fatal("expected connection error for unreachable server, got nil")
			}
			if strings.Contains(err.Error(), "scheme must be") ||
				strings.Contains(err.Error(), "missing host") ||
				strings.Contains(err.Error(), "expected nacos://[user:pass@]host:port") {
				t.Errorf("got format error instead of connection error: %v", err)
			}
			if !strings.Contains(err.Error(), "preflight check failed") {
				t.Errorf("expected preflight check error, got: %v", err)
			}
		})
	}
}

func TestResolveNacos_URIParsing(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		wantErr string
	}{
		{
			name:    "too few path segments",
			uri:     "nacos://host:8848/only-namespace",
			wantErr: "invalid nacos URI",
		},
		{
			name:    "empty path",
			uri:     "nacos://host:8848",
			wantErr: "invalid nacos URI",
		},
	}

	resolver := NewPackageResolver(t.TempDir())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.uri)
			if err != nil {
				t.Fatalf("url.Parse failed: %v", err)
			}
			_, err = resolver.resolveNacos(context.Background(), u)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestResolveNacos_AddrExtraction(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{
			name: "plain host",
			uri:  "nacos://10.0.0.1:8848/ns/spec",
		},
		{
			name: "host with credentials",
			uri:  "nacos://user:pass@10.0.0.1:8848/ns/spec",
		},
	}

	resolver := NewPackageResolver(t.TempDir())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.uri)
			if err != nil {
				t.Fatalf("url.Parse failed: %v", err)
			}
			_, err = resolver.resolveNacos(context.Background(), u)
			if err == nil {
				t.Fatal("expected error for unreachable server, got nil")
			}
			if strings.Contains(err.Error(), "HICLAW_NACOS_ADDR") {
				t.Errorf("error should not reference HICLAW_NACOS_ADDR, got: %v", err)
			}
		})
	}
}

func TestParseNacosAddr_UsesEnvCredentialsAsDefault(t *testing.T) {
	t.Setenv("HICLAW_NACOS_USERNAME", "env-user")
	t.Setenv("HICLAW_NACOS_PASSWORD", "env-pass")

	host, port, username, password, err := parseNacosAddr("nacos.internal:8848")
	if err != nil {
		t.Fatalf("parseNacosAddr returned error: %v", err)
	}
	if host != "nacos.internal" {
		t.Fatalf("host = %q, want %q", host, "nacos.internal")
	}
	if port != "8848" {
		t.Fatalf("port = %q, want %q", port, "8848")
	}
	if username != "env-user" || password != "env-pass" {
		t.Fatalf("credentials = %q/%q, want env-user/env-pass", username, password)
	}
}

func TestParseNacosAddr_URIAuthOverridesEnv(t *testing.T) {
	t.Setenv("HICLAW_NACOS_USERNAME", "env-user")
	t.Setenv("HICLAW_NACOS_PASSWORD", "env-pass")

	host, port, username, password, err := parseNacosAddr("nacos://uri-user:uri-pass@nacos.internal/ns/spec")
	if err != nil {
		t.Fatalf("parseNacosAddr returned error: %v", err)
	}
	if host != "nacos.internal" {
		t.Fatalf("host = %q, want %q", host, "nacos.internal")
	}
	if port != "8848" {
		t.Fatalf("port = %q, want %q", port, "8848")
	}
	if username != "uri-user" || password != "uri-pass" {
		t.Fatalf("credentials = %q/%q, want uri-user/uri-pass", username, password)
	}
}

func TestValidateNacosURI_UsesEnvCredentialsWhenURIHasNoAuth(t *testing.T) {
	t.Setenv("HICLAW_NACOS_USERNAME", "env-user")
	t.Setenv("HICLAW_NACOS_PASSWORD", "env-pass")

	err := ValidateNacosURI(context.Background(), "nacos://127.0.0.1:19999/ns/my-spec")
	if err == nil {
		t.Fatal("expected connection error for unreachable server, got nil")
	}
	if strings.Contains(err.Error(), "scheme must be") ||
		strings.Contains(err.Error(), "missing host") ||
		strings.Contains(err.Error(), "expected nacos://[user:pass@]host:port") {
		t.Errorf("got format error instead of connection error: %v", err)
	}
	if !strings.Contains(err.Error(), "preflight check failed") {
		t.Errorf("expected preflight check error, got: %v", err)
	}
}

func TestValidateNacosURI_PartialEnvCredentialsFail(t *testing.T) {
	t.Setenv("HICLAW_NACOS_USERNAME", "env-user")
	t.Setenv("HICLAW_NACOS_PASSWORD", "")

	err := ValidateNacosURI(context.Background(), "nacos://127.0.0.1:19999/ns/my-spec")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "both username and password are required") {
		t.Fatalf("expected credential pair error, got: %v", err)
	}
}

// --- helpers ---
func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", filepath.Base(path), err)
	}
	content := strings.TrimSpace(string(data))
	if content != expected {
		t.Errorf("%s: expected %q, got %q", filepath.Base(path), expected, content)
	}
}

func assertFileContains(t *testing.T, path, substr string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", filepath.Base(path), err)
	}
	if !strings.Contains(string(data), substr) {
		t.Errorf("%s should contain %q", filepath.Base(path), substr)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
