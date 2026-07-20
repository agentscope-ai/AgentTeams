package executor

import (
	"archive/zip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credprovider"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
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

func TestWriteInlineConfigs_AllFields_Hermes(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "hermes", "identity content", "soul content", "agents content")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "IDENTITY.md")); err == nil {
		t.Error("IDENTITY.md should not exist for hermes runtime")
	}

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

func TestWriteInlineConfigs_HermesMergesIdentityIntoSoul(t *testing.T) {
	dir := t.TempDir()

	err := WriteInlineConfigs(dir, "hermes", "# Identity\nName: Alice", "# Role\nDevOps engineer", "")
	if err != nil {
		t.Fatalf("WriteInlineConfigs failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		t.Fatalf("failed to read SOUL.md: %v", err)
	}
	content := string(data)

	idxIdentity := strings.Index(content, "# Identity")
	idxRole := strings.Index(content, "# Role")
	if idxIdentity < 0 || idxRole < 0 {
		t.Fatalf("expected both identity and role in SOUL.md, got: %s", content)
	}
	if idxIdentity >= idxRole {
		t.Error("identity should be prepended before soul content")
	}
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
			err := ValidateNacosURI(context.Background(), tt.uri, ValidateNacosURIOptions{})
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
			err := ValidateNacosURI(context.Background(), tt.uri, ValidateNacosURIOptions{})
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
			if strings.Contains(err.Error(), "AGENTTEAMS_NACOS_ADDR") {
				t.Errorf("error should not reference AGENTTEAMS_NACOS_ADDR, got: %v", err)
			}
		})
	}
}

func TestParseNacosAddr_UsesEnvCredentialsAsDefault(t *testing.T) {
	t.Setenv("AGENTTEAMS_NACOS_USERNAME", "env-user")
	t.Setenv("AGENTTEAMS_NACOS_PASSWORD", "env-pass")

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
	t.Setenv("AGENTTEAMS_NACOS_USERNAME", "env-user")
	t.Setenv("AGENTTEAMS_NACOS_PASSWORD", "env-pass")

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
	t.Setenv("AGENTTEAMS_NACOS_USERNAME", "env-user")
	t.Setenv("AGENTTEAMS_NACOS_PASSWORD", "env-pass")

	err := ValidateNacosURI(context.Background(), "nacos://127.0.0.1:19999/ns/my-spec", ValidateNacosURIOptions{})
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
	t.Setenv("AGENTTEAMS_NACOS_USERNAME", "env-user")
	t.Setenv("AGENTTEAMS_NACOS_PASSWORD", "")

	err := ValidateNacosURI(context.Background(), "nacos://127.0.0.1:19999/ns/my-spec", ValidateNacosURIOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "both username and password are required") {
		t.Fatalf("expected credential pair error, got: %v", err)
	}
}

func TestValidateNacosURI_ChecksAgentSpecExists(t *testing.T) {
	server := newNacosAgentSpecCheckTestServer(t,
		http.StatusOK,
		`{"code":0,"message":"success","data":{"totalCount":0,"pageItems":[]}}`,
		0,
		"",
	)
	defer server.Close()

	err := ValidateNacosURI(context.Background(), "nacos://"+server.Listener.Addr().String()+"/public/missing-spec", ValidateNacosURIOptions{})
	if err == nil {
		t.Fatal("expected missing agentspec error, got nil")
	}
	if !strings.Contains(err.Error(), `agentspec "missing-spec" not found`) {
		t.Fatalf("expected explicit missing-spec error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "missing-spec") {
		t.Fatalf("expected spec name in error, got: %v", err)
	}
}

func TestValidateNacosURI_SucceedsWhenAgentSpecExists(t *testing.T) {
	server := newNacosAgentSpecCheckTestServer(t,
		http.StatusOK,
		`{"code":0,"message":"success","data":{"totalCount":1,"pageItems":[{"namespaceId":"public","name":"existing-spec","description":"demo","enable":true,"onlineCnt":1,"labels":{"latest":"v1"}}]}}`,
		0,
		"",
	)
	defer server.Close()

	err := ValidateNacosURI(context.Background(), "nacos://"+server.Listener.Addr().String()+"/public/existing-spec", ValidateNacosURIOptions{})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestValidateNacosURI_FailsWhenAgentSpecHasNoOnlineVersion(t *testing.T) {
	server := newNacosAgentSpecCheckTestServer(t,
		http.StatusOK,
		`{"code":0,"message":"success","data":{"totalCount":1,"pageItems":[{"namespaceId":"public","name":"offline-spec","description":"demo","enable":true,"onlineCnt":0,"labels":{}}]}}`,
		0,
		"",
	)
	defer server.Close()

	err := ValidateNacosURI(context.Background(), "nacos://"+server.Listener.Addr().String()+"/public/offline-spec", ValidateNacosURIOptions{})
	if err == nil {
		t.Fatal("expected offline agentspec error, got nil")
	}
	if !strings.Contains(err.Error(), "has no online version") {
		t.Fatalf("expected no-online-version error, got: %v", err)
	}
}

func TestValidateNacosURI_FailsWhenRequestedVersionIsNotOnline(t *testing.T) {
	server := newNacosAgentSpecCheckTestServer(t,
		http.StatusOK,
		`{"code":0,"message":"success","data":{"totalCount":1,"pageItems":[{"namespaceId":"public","name":"mixed-spec","description":"demo","enable":true,"onlineCnt":1,"labels":{"latest":"v2"}}]}}`,
		http.StatusNotFound,
		`{"code":404,"message":"AgentSpec version not online: mixed-spec"}`,
	)
	defer server.Close()

	err := ValidateNacosURI(context.Background(), "nacos://"+server.Listener.Addr().String()+"/public/mixed-spec/v1", ValidateNacosURIOptions{})
	if err == nil {
		t.Fatal("expected offline version error, got nil")
	}
	if !strings.Contains(err.Error(), `online version "v1" not found`) {
		t.Fatalf("expected online-version-not-found error, got: %v", err)
	}
}

func TestResolveAndExtract_FileDirectoryWithoutSoulmd_Succeeds(t *testing.T) {
	ctx := context.Background()
	importDir := t.TempDir()
	pkgRoot := filepath.Join(importDir, "pkgroot")
	if err := os.MkdirAll(pkgRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgRoot, "note.txt"), []byte("no soul"), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(pkgRoot)
	if err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	pr := NewPackageResolver(importDir)
	out, err := pr.ResolveAndExtract(ctx, u.String(), "ignored")
	if err != nil {
		t.Fatalf("ResolveAndExtract: %v", err)
	}
	if filepath.Clean(out) != filepath.Clean(pkgRoot) {
		t.Fatalf("got dir %q want %q", out, pkgRoot)
	}
}

func TestResolveAndExtract_ZipWithoutSoulmd_Succeeds(t *testing.T) {
	ctx := context.Background()
	importDir := t.TempDir()
	zipPath := filepath.Join(importDir, "minimal.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("skills/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("skill")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	absZip, err := filepath.Abs(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absZip)}
	pr := NewPackageResolver(importDir)
	out, err := pr.ResolveAndExtract(ctx, u.String(), "w1")
	if err != nil {
		t.Fatalf("ResolveAndExtract: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "skills", "x.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != "skill" {
		t.Fatalf("content = %q", string(data))
	}
}

func TestValidateNacosURI_STSHiclaw_RequiresCredClient(t *testing.T) {
	uri := "nacos://127.0.0.1:19999/public/my-spec?authType=sts-hiclaw"
	err := ValidateNacosURI(context.Background(), uri, ValidateNacosURIOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "sts-hiclaw auth requires a credprovider.Client") {
		t.Fatalf("expected credprovider requirement error, got: %v", err)
	}
}

func TestValidateNacosURI_STSHiclaw_SucceedsWithCredClient(t *testing.T) {
	server := newNacosAgentSpecCheckTestServer(t,
		http.StatusOK,
		`{"code":0,"message":"success","data":{"totalCount":1,"pageItems":[{"namespaceId":"public","name":"sts-spec","description":"demo","enable":true,"onlineCnt":1,"labels":{"latest":"v1"}}]}}`,
		0,
		"",
	)
	defer server.Close()

	stub := stubCredClient{}
	err := ValidateNacosURI(context.Background(), "nacos://"+server.Listener.Addr().String()+"/public/sts-spec?authType=sts-hiclaw", ValidateNacosURIOptions{
		CredClient: stub,
	})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestPutPackageObjectSeedOnlyPreservesExistingStorageObject(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	if err := store.PutObject(ctx, "agents/alice/config/credagent.json", []byte("runtime")); err != nil {
		t.Fatal(err)
	}

	seeded, err := putPackageObjectSeedOnly(ctx, store, true, "agents/alice/config/credagent.json", []byte("template"))
	if err != nil {
		t.Fatalf("putPackageObjectSeedOnly failed: %v", err)
	}
	if seeded {
		t.Fatalf("existing object should not be seeded")
	}

	got, err := store.GetObject(ctx, "agents/alice/config/credagent.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "runtime" {
		t.Fatalf("seed-only object overwritten: %q", got)
	}

	seeded, err = putPackageObjectSeedOnly(ctx, store, true, "agents/alice/config/new.json", []byte("template"))
	if err != nil {
		t.Fatalf("putPackageObjectSeedOnly new object failed: %v", err)
	}
	if !seeded {
		t.Fatalf("new object should be seeded")
	}
	got, err = store.GetObject(ctx, "agents/alice/config/new.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "template" {
		t.Fatalf("new seed object = %q", got)
	}
}

func TestSeedPackageDirectoryObjectCreatesPrefixMarker(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()

	if err := seedPackageDirectoryObject(ctx, store, "agents/alice"); err != nil {
		t.Fatalf("seedPackageDirectoryObject failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/alice/.agentteams-keep")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("directory marker should be empty, got %q", got)
	}
}

func TestWriteFileSeedOnlyPreservesExistingLocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "credagent.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("runtime"), 0644); err != nil {
		t.Fatal(err)
	}

	seeded, err := writeFileSeedOnly(path, []byte("template"))
	if err != nil {
		t.Fatalf("writeFileSeedOnly failed: %v", err)
	}
	if seeded {
		t.Fatalf("existing file should not be seeded")
	}
	assertFileContent(t, path, "runtime")

	newPath := filepath.Join(filepath.Dir(path), "new.json")
	seeded, err = writeFileSeedOnly(newPath, []byte("template"))
	if err != nil {
		t.Fatalf("writeFileSeedOnly new file failed: %v", err)
	}
	if !seeded {
		t.Fatalf("new file should be seeded")
	}
	assertFileContent(t, newPath, "template")
}

func TestCopyDirSeedOnlyPreservesExistingLocalFiles(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "settings.json"), []byte("template"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "new.json"), []byte("new-template"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "nested", "settings.json"), []byte("runtime"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := copyDirSeedOnly(src, dst); err != nil {
		t.Fatalf("copyDirSeedOnly failed: %v", err)
	}
	assertFileContent(t, filepath.Join(dst, "nested", "settings.json"), "runtime")
	assertFileContent(t, filepath.Join(dst, "nested", "new.json"), "new-template")
}

func TestSeedDirToStoragePreservesExistingObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "skills")
	if err := os.MkdirAll(filepath.Join(src, "tool"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "tool", "SKILL.md"), []byte("template"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "tool", "extra.md"), []byte("extra-template"), 0644); err != nil {
		t.Fatal(err)
	}

	store := ossfake.NewMemory()
	if err := store.PutObject(ctx, "agents/alice/skills/tool/SKILL.md", []byte("runtime")); err != nil {
		t.Fatal(err)
	}

	if err := seedDirToStorage(ctx, store, src, "agents/alice/skills"); err != nil {
		t.Fatalf("seedDirToStorage failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/alice/skills/tool/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "runtime" {
		t.Fatalf("existing storage object overwritten: %q", got)
	}
	got, err = store.GetObject(ctx, "agents/alice/skills/tool/extra.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "extra-template" {
		t.Fatalf("new storage seed object = %q", got)
	}
}

// --- helpers ---

// stubCredClient returns a static STS-like triple for Nacos Spas signing tests.
type stubCredClient struct{}

func (stubCredClient) Issue(ctx context.Context, req credprovider.IssueRequest) (*credprovider.IssueResponse, error) {
	return &credprovider.IssueResponse{
		AccessKeyID:     "test-access-key-id",
		AccessKeySecret: "test-access-key-secret",
		SecurityToken:   "test-security-token",
		Expiration:      "2099-01-01T00:00:00Z",
	}, nil
}

// GetKubeconfig is a stub to satisfy the credprovider.Client interface; this
// test only exercises Nacos Spas signing.
func (stubCredClient) GetKubeconfig(context.Context, string) (*credprovider.KubeconfigResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func newNacosAgentSpecCheckTestServer(t *testing.T, listStatus int, listBody string, getStatus int, getBody string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nacos/v3/admin/ai/agentspecs/list":
			if r.URL.Query().Get("namespaceId") == "" || r.URL.Query().Get("agentSpecName") == "" {
				t.Fatalf("expected namespaceId and agentSpecName query params, got: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("search") != "accurate" {
				t.Fatalf("expected accurate search, got: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("pageNo") != "1" || r.URL.Query().Get("pageSize") != "1" {
				t.Fatalf("expected first-page single-item query, got: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(listStatus)
			_, _ = w.Write([]byte(listBody))
		case "/nacos/v3/client/ai/agentspecs":
			if getStatus == 0 {
				t.Fatalf("unexpected get request: %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			if r.URL.Query().Get("namespaceId") == "" || r.URL.Query().Get("name") == "" {
				t.Fatalf("expected namespaceId and name query params, got: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(getStatus)
			_, _ = w.Write([]byte(getBody))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
}

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
