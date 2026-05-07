package executor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PackageResolver handles file://, http(s)://, and nacos:// package URIs.
type PackageResolver struct {
	ImportDir  string // e.g. /tmp/import
	ExtractDir string // e.g. /tmp/import/extracted
}

func NewPackageResolver(importDir string) *PackageResolver {
	extractDir := filepath.Join(importDir, "extracted")
	os.MkdirAll(extractDir, 0755)
	return &PackageResolver{ImportDir: importDir, ExtractDir: extractDir}
}

// Resolve downloads or locates a package and returns the local path.
// For nacos:// URIs the result is a directory; for all others it is a ZIP file.
// Supported schemes: file://, http://, https://, nacos://
func (p *PackageResolver) Resolve(ctx context.Context, uri string) (string, error) {
	if uri == "" {
		return "", nil
	}

	parsed, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("invalid package URI %q: %w", uri, err)
	}

	switch parsed.Scheme {
	case "file":
		return p.resolveFile(parsed)
	case "http", "https":
		return p.resolveHTTP(ctx, uri)
	case "nacos":
		return p.resolveNacos(ctx, parsed)
	case "oss":
		return p.resolveOSS(ctx, parsed)
	default:
		// Treat as relative MinIO path (e.g. "packages/alice.zip")
		// Use content-addressable cache: download to /tmp/import/{md5}.zip
		// If the same content already exists locally, skip re-download.
		storagePrefix := os.Getenv("HICLAW_STORAGE_PREFIX")
		if storagePrefix == "" {
			storagePrefix = "hiclaw/hiclaw-storage"
		}
		minioPath := fmt.Sprintf("%s/hiclaw-config/%s", storagePrefix, uri)

		// Get remote file's ETag (MD5) via mc stat
		etag := getMinIOETag(ctx, minioPath)
		if etag == "" {
			// Fallback: use URI hash if mc stat fails
			h := sha256.Sum256([]byte(uri))
			etag = fmt.Sprintf("%x", h[:8])
		}

		destPath := filepath.Join(p.ImportDir, etag+".zip")
		if _, err := os.Stat(destPath); err == nil {
			return destPath, nil // cache hit, same content
		}

		cmd := exec.CommandContext(ctx, "mc", "cp", minioPath, destPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to download %s from MinIO: %s: %w", minioPath, string(out), err)
		}
		return destPath, nil
	}
}

// ResolveAndExtract downloads/locates a package, extracts it, and returns the
// extracted directory path. Typical layout (SOUL.md is optional; DeployWorkerConfig
// supplies a default SOUL when missing):
//
//	{extractDir}/{name}/
//	├── config/
//	│   ├── SOUL.md (optional)
//	│   └── AGENTS.md (optional)
//	├── skills/ (optional)
//	└── Dockerfile (optional)
func (p *PackageResolver) ResolveAndExtract(ctx context.Context, uri, name string) (string, error) {
	if uri == "" {
		return "", nil
	}

	resolved, err := p.Resolve(ctx, uri)
	if err != nil {
		return "", fmt.Errorf("resolve package: %w", err)
	}

	// If Resolve already returned a directory (e.g. nacos://), use it directly.
	if info, err := os.Stat(resolved); err == nil && info.IsDir() {
		return resolved, nil
	}

	// Otherwise treat as ZIP and extract.
	destDir := filepath.Join(p.ExtractDir, name)
	os.RemoveAll(destDir)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("create extract dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, "unzip", "-q", "-o", resolved, "-d", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract ZIP %s: %s: %w", resolved, string(out), err)
	}

	return destDir, nil
}

// DeployToMinIO copies extracted package contents to the worker's MinIO agent space.
// Config files and skills from the package are applied when present; SOUL may come
// from the package, inline spec, or DeployWorkerConfig defaults.
//
// To avoid a race with the background MinIO→local sync (which could overwrite local
// files between the local write and the mc mirror push), we push to MinIO FIRST from
// the extracted directory (immune to background sync), then copy to the local agent dir.
func (p *PackageResolver) DeployToMinIO(ctx context.Context, extractedDir, workerName string, excludeMemory bool) error {
	agentDir := fmt.Sprintf("/root/hiclaw-fs/agents/%s", workerName)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return fmt.Errorf("create agent dir: %w", err)
	}

	storagePrefix := os.Getenv("HICLAW_STORAGE_PREFIX")
	if storagePrefix == "" {
		storagePrefix = "hiclaw/hiclaw-storage"
	}
	minioBase := fmt.Sprintf("%s/agents/%s", storagePrefix, workerName)

	// Collect transformed config files and subdirectory names from the package.
	type fileEntry struct {
		name string
		data []byte
	}
	var configFiles []fileEntry
	var configSubdirs []string

	configDir := filepath.Join(extractedDir, "config")
	if info, err := os.Stat(configDir); err == nil && info.IsDir() {
		entries, _ := os.ReadDir(configDir)
		for _, e := range entries {
			if e.IsDir() {
				configSubdirs = append(configSubdirs, e.Name())
				continue
			}
			if excludeMemory && e.Name() == "MEMORY.md" {
				continue
			}
			src := filepath.Join(configDir, e.Name())
			data, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			if e.Name() == "AGENTS.md" {
				data = wrapWithBuiltinMarkers(data)
			}
			configFiles = append(configFiles, fileEntry{name: e.Name(), data: data})
		}
	} else {
		// Fallback: SOUL.md at root level
		src := filepath.Join(extractedDir, "SOUL.md")
		if data, err := os.ReadFile(src); err == nil {
			configFiles = append(configFiles, fileEntry{name: "SOUL.md", data: data})
		}
	}

	// Collect transformed crons data (if present).
	skillsDir := filepath.Join(extractedDir, "skills")
	cronsDir := filepath.Join(extractedDir, "crons")
	var cronData []byte
	if info, err := os.Stat(cronsDir); err == nil && info.IsDir() {
		if raw, err := os.ReadFile(filepath.Join(cronsDir, "jobs.json")); err == nil {
			trimmed := strings.TrimSpace(string(raw))
			if strings.HasPrefix(trimmed, "[") {
				cronData = []byte(fmt.Sprintf(`{"version":1,"jobs":%s}`, trimmed))
			} else {
				cronData = raw
			}
		}
	}

	// ── Phase 1: Push to MinIO FIRST from extracted dir (immune to background sync) ──

	// Config files — when excludeMemory is true (update path), skip SOUL.md and AGENTS.md
	// because DeployWorkerConfig handles them with proper inline override priority.
	for _, f := range configFiles {
		if excludeMemory && (f.name == "SOUL.md" || f.name == "AGENTS.md") {
			continue
		}
		if err := mcPut(ctx, minioBase+"/"+f.name, f.data); err != nil {
			return fmt.Errorf("push %s to MinIO: %w", f.name, err)
		}
	}

	// Skills
	if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
		mcCmd := exec.CommandContext(ctx, "mc", "mirror", skillsDir+"/", minioBase+"/skills/", "--overwrite")
		if out, err := mcCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("mc mirror skills to MinIO: %s: %w", string(out), err)
		}
	}

	// Crons
	if cronData != nil {
		if err := mcPut(ctx, minioBase+"/.openclaw/cron/jobs.json", cronData); err != nil {
			return fmt.Errorf("push crons to MinIO: %w", err)
		}
	}

	// ── Phase 2: Copy to local agent dir (safe — MinIO already has new content) ──

	for _, f := range configFiles {
		if excludeMemory && (f.name == "SOUL.md" || f.name == "AGENTS.md") {
			continue
		}
		os.WriteFile(filepath.Join(agentDir, f.name), f.data, 0644)
	}
	for _, dirName := range configSubdirs {
		src := filepath.Join(configDir, dirName)
		dst := filepath.Join(agentDir, dirName)
		cpCmd := exec.CommandContext(ctx, "cp", "-r", src, dst)
		cpCmd.CombinedOutput()
	}

	// Skills
	if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
		destSkills := filepath.Join(agentDir, "skills")
		os.MkdirAll(destSkills, 0755)
		cpCmd := exec.CommandContext(ctx, "cp", "-r", skillsDir+"/.", destSkills+"/")
		cpCmd.CombinedOutput()
	}

	// Crons
	if cronData != nil {
		destCron := filepath.Join(agentDir, ".openclaw", "cron")
		os.MkdirAll(destCron, 0755)
		os.WriteFile(filepath.Join(destCron, "jobs.json"), cronData, 0644)
	}

	return nil
}

// mcPut writes data to a MinIO path via temp file + mc cp.
func mcPut(ctx context.Context, minioPath string, data []byte) error {
	tmpFile, err := os.CreateTemp("", "hiclaw-deploy-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()

	cmd := exec.CommandContext(ctx, "mc", "cp", tmpName, minioPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mc cp to %s: %s: %w", minioPath, string(out), err)
	}
	return nil
}

// wrapWithBuiltinMarkers wraps user AGENTS.md content with hiclaw-builtin markers.
// Uses the same format as builtin-merge.sh (BUILTIN_HEADER + BUILTIN_END) so that
// upgrade-builtins.sh can fill the builtin section without destroying user content.
// If markers already exist, the content is returned as-is.
// YAML frontmatter (---...---) is preserved before the markers.
func wrapWithBuiltinMarkers(data []byte) []byte {
	content := string(data)
	if strings.Contains(content, "<!-- hiclaw-builtin-start -->") {
		return data // already has markers
	}

	var frontmatter, body string
	// Extract YAML frontmatter if present
	if strings.HasPrefix(content, "---\n") {
		if end := strings.Index(content[4:], "\n---\n"); end >= 0 {
			fmEnd := 4 + end + 4 // past the closing "---\n"
			frontmatter = content[:fmEnd]
			body = content[fmEnd:]
		} else {
			body = content
		}
	} else {
		body = content
	}

	// Match the exact format from builtin-merge.sh BUILTIN_HEADER + BUILTIN_END
	wrapped := ""
	if frontmatter != "" {
		wrapped += frontmatter + "\n"
	}
	wrapped += "<!-- hiclaw-builtin-start -->\n" +
		"> ⚠️ **DO NOT EDIT** this section. It is managed by HiClaw and will be automatically\n" +
		"> replaced on upgrade. To customize, add your content **after** the\n" +
		"> `<!-- hiclaw-builtin-end -->` marker below.\n" +
		"\n" +
		"<!-- hiclaw-builtin-end -->\n\n" +
		body
	return []byte(wrapped)
}

// WriteInlineConfigs writes inline identity/soul/agents content to the agent directory.
// For copaw and hermes runtimes, identity is merged into SOUL.md since neither
// supports a separate IDENTITY.md file.
// This function is called AFTER DeployToMinIO so inline fields override package files.
func WriteInlineConfigs(agentDir, runtime, identity, soul, agents string) error {
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return fmt.Errorf("create agent dir %s: %w", agentDir, err)
	}

	mergeIdentityIntoSoul := strings.EqualFold(runtime, "copaw") ||
		strings.EqualFold(runtime, "hermes")

	if mergeIdentityIntoSoul {
		// CoPaw / Hermes: merge identity into soul (prepend)
		merged := ""
		if identity != "" {
			merged += strings.TrimSpace(identity) + "\n\n"
		}
		if soul != "" {
			merged += strings.TrimSpace(soul)
		}
		if merged != "" {
			if err := os.WriteFile(filepath.Join(agentDir, "SOUL.md"), []byte(merged+"\n"), 0644); err != nil {
				return fmt.Errorf("write SOUL.md: %w", err)
			}
		}
	} else {
		// OpenClaw: write IDENTITY.md and SOUL.md separately
		if identity != "" {
			if err := os.WriteFile(filepath.Join(agentDir, "IDENTITY.md"), []byte(strings.TrimSpace(identity)+"\n"), 0644); err != nil {
				return fmt.Errorf("write IDENTITY.md: %w", err)
			}
		}
		if soul != "" {
			if err := os.WriteFile(filepath.Join(agentDir, "SOUL.md"), []byte(strings.TrimSpace(soul)+"\n"), 0644); err != nil {
				return fmt.Errorf("write SOUL.md: %w", err)
			}
		}
	}

	if agents != "" {
		wrapped := wrapWithBuiltinMarkers([]byte(strings.TrimSpace(agents)))
		if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), wrapped, 0644); err != nil {
			return fmt.Errorf("write AGENTS.md: %w", err)
		}
	}

	return nil
}

// getMinIOETag returns the ETag (content MD5) of a MinIO object via mc stat.
// Returns empty string if mc stat fails.
func getMinIOETag(ctx context.Context, minioPath string) string {
	cmd := exec.CommandContext(ctx, "mc", "stat", "--json", minioPath)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// mc stat --json outputs {"etag":"xxx",...}
	// Simple extraction without json dependency
	s := string(out)
	if idx := strings.Index(s, `"etag":"`); idx >= 0 {
		rest := s[idx+8:]
		if end := strings.Index(rest, `"`); end >= 0 {
			etag := rest[:end]
			// Remove quotes and dashes from ETag
			etag = strings.ReplaceAll(etag, "-", "")
			if etag != "" {
				return etag
			}
		}
	}
	return ""
}

// --- Private resolve methods ---

// resolveOSS downloads a package from MinIO/OSS storage.
// URI format: oss://hiclaw-config/packages/{name}-{md5}.zip
// The filename contains the content hash, so it's naturally content-addressable:
// same hash → same content → cache hit.
func (p *PackageResolver) resolveOSS(ctx context.Context, u *url.URL) (string, error) {
	// oss://hiclaw-config/packages/alice-abc123.zip → hiclaw-config/packages/alice-abc123.zip
	ossPath := strings.TrimPrefix(u.Host+u.Path, "/")
	filename := filepath.Base(ossPath)

	// Content-addressable cache: filename includes MD5, so same file = cache hit
	destPath := filepath.Join(p.ImportDir, filename)
	if _, err := os.Stat(destPath); err == nil {
		return destPath, nil // cache hit
	}

	// Download from MinIO
	storagePrefix := os.Getenv("HICLAW_STORAGE_PREFIX")
	if storagePrefix == "" {
		storagePrefix = "hiclaw/hiclaw-storage"
	}
	minioPath := fmt.Sprintf("%s/%s", storagePrefix, ossPath)

	cmd := exec.CommandContext(ctx, "mc", "cp", minioPath, destPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to download oss://%s from MinIO: %s: %w", ossPath, string(out), err)
	}

	return destPath, nil
}

func (p *PackageResolver) resolveFile(u *url.URL) (string, error) {
	filename := filepath.Base(u.Path)
	localPath := filepath.Join(p.ImportDir, filename)

	if _, err := os.Stat(localPath); err != nil {
		if _, err2 := os.Stat(u.Path); err2 != nil {
			return "", fmt.Errorf("file package not found at %s or %s", localPath, u.Path)
		}
		return u.Path, nil
	}
	return localPath, nil
}

func (p *PackageResolver) resolveHTTP(ctx context.Context, uri string) (string, error) {
	filename := filepath.Base(uri)
	if !strings.HasSuffix(filename, ".zip") {
		filename += ".zip"
	}
	destPath := filepath.Join(p.ImportDir, filename)

	if _, err := os.Stat(destPath); err == nil {
		return destPath, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request for %s: %w", uri, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download %s: %w", uri, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s returned status %d", uri, resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("failed to create %s: %w", destPath, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		os.Remove(destPath)
		return "", fmt.Errorf("failed to write %s: %w", destPath, err)
	}

	return destPath, nil
}

// resolveNacos downloads a Worker template from Nacos via the AgentSpec client API.
// URI format: nacos://[user:pass@]host:port/{namespace}/{agentspec-name}[/{version}]
// The Nacos server address (and optional credentials) are extracted directly from the URI.
func (p *PackageResolver) resolveNacos(ctx context.Context, u *url.URL) (string, error) {
	// Parse URI path segments: /{namespace}/{agentspec-name}/{version}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid nacos URI: expected nacos://[user:pass@]host:port/{namespace}/{agentspec-name}[/{version}], got %s", u.String())
	}

	// Build the Nacos server address from the URI authority (host, port, userinfo).
	nacosAddr := u.Host
	if u.User != nil {
		nacosAddr = u.User.String() + "@" + u.Host
	}

	namespace := parts[0]
	specName := parts[1]
	version := ""
	if len(parts) >= 3 {
		version = parts[2]
	}

	outputDir := filepath.Join(p.ImportDir, "nacos")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create nacos import dir %s: %w", outputDir, err)
	}
	destPath := filepath.Join(outputDir, specName)
	if err := os.RemoveAll(destPath); err != nil {
		return "", fmt.Errorf("failed to clean previous nacos package %s: %w", destPath, err)
	}

	client, err := newNacosAgentSpecClient(ctx, nacosAddr, namespace)
	if err != nil {
		return "", err
	}

	label := ""
	if strings.HasPrefix(version, "label:") {
		label = strings.TrimPrefix(version, "label:")
		version = ""
	}

	if err := client.GetAgentSpec(ctx, specName, outputDir, version, label); err != nil {
		return "", fmt.Errorf("fetch agentspec %s from nacos failed: %w", specName, err)
	}

	info, err := os.Stat(destPath)
	if err != nil {
		return "", fmt.Errorf("agentspec download finished but %s was not created: %w", destPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("agentspec output %s is not a directory", destPath)
	}

	return destPath, nil
}

// ValidateNacosURI checks that a nacos:// URI is well-formed, the server is
// reachable, and any embedded credentials are accepted.  It is intended as a
// preflight check before persisting a Worker resource.
func ValidateNacosURI(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid nacos URI %q: %w", raw, err)
	}
	if u.Scheme != "nacos" {
		return fmt.Errorf("invalid nacos URI %q: scheme must be nacos://", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid nacos URI %q: missing host", raw)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return fmt.Errorf("invalid nacos URI %q: expected nacos://[user:pass@]host:port/{namespace}/{agentspec-name}[/{version}]", raw)
	}

	nacosAddr := u.Host
	if u.User != nil {
		nacosAddr = u.User.String() + "@" + u.Host
	}
	namespace := parts[0]
	specName := parts[1]
	version := ""
	if len(parts) >= 3 {
		version = parts[2]
	}

	label := ""
	if strings.HasPrefix(version, "label:") {
		label = strings.TrimPrefix(version, "label:")
		version = ""
	}

	// newNacosAgentSpecClient validates the address format, connects, and
	// performs login when credentials are present.
	client, err := newNacosAgentSpecClient(ctx, nacosAddr, namespace)
	if err != nil {
		return fmt.Errorf("nacos preflight check failed for %q: %w", raw, err)
	}
	if err := client.CheckAgentSpecExists(ctx, specName, version, label); err != nil {
		return fmt.Errorf("nacos preflight check failed for %q: %w", raw, err)
	}
	return nil
}
