// Package skillscan runs the skill content scan (scan ① at upload, scan ②
// at assign-time materialization) by exec'ing the qwenpaw skill scanner
// inside a running qwenpaw container.
//
// The scanner lives in the runtime image (qwenpaw.security.skill_scanner);
// the controller has no Python, so the scan is a container round trip:
//
//   - embedded (docker) mode (production today): Docker Engine API over the
//     mounted unix socket (raw HTTP — the controller image has no docker
//     CLI). The payload goes in as one tar (archive API), the probe runs
//     detached, and the verdict comes back as a file (archive API).
//   - k8s mode: not implemented in v1 — the scan fails closed (uploads
//     mark scan.status="skipped"; assign materialization skips + warns).
//
// The gate is the gate: the probe bypasses the runtime scanner's
// off/warn/block configuration (it calls SkillScanner.scan_skill directly,
// or forces block mode on older runtimes) and maps CRITICAL/HIGH → block,
// MEDIUM/LOW/INFO → warn, none → pass. Any transport, exec, or parse
// failure is an error ("unavailable") — never a pass:
//
//   - scan ① (upload, best-effort): unavailable → the upload proceeds and
//     the response carries scan.status="skipped" (a warning is logged);
//   - scan ② (assign copy, mandatory): unavailable → the skill is NOT
//     copied and the warning is surfaced (the gate does not default open).
//
// Results are cached by content hash (default 30 min TTL, 100 entries,
// FIFO eviction) so a re-scan of unchanged content costs no exec.
package skillscan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tmpRoot is the container-local scratch root for scan payloads and
// verdicts (world-writable /tmp; deleted after each scan).
const tmpRoot = "/tmp/.skillscan"

// SkillScanner is the content-scan contract. err != nil means the scan
// could not run ("unavailable") — callers must fail closed (upload: mark
// skipped; assign: do not copy).
type SkillScanner interface {
	ScanSkill(ctx context.Context, name string, files map[string][]byte) (SkillScanVerdict, error)
}

// SkillScanVerdict is the scan outcome. Status: "pass" (no findings),
// "warn" (MEDIUM/LOW/INFO findings — allowed, surfaced), "block"
// (CRITICAL/HIGH findings — rejected).
type SkillScanVerdict struct {
	Status   string               `json:"status"`
	Findings []SkillUploadFinding `json:"findings,omitempty"`
}

// SkillUploadFinding is one scanner finding (metadata only — never file
// contents).
type SkillUploadFinding struct {
	RuleID   string `json:"rule_id"`
	Category string `json:"category,omitempty"`
	Severity string `json:"severity"` // CRITICAL | HIGH | MEDIUM | LOW | INFO
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title"`
}

// probeOutput is the one-line JSON the probe writes.
type probeOutput struct {
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Findings []struct {
		RuleID   string `json:"rule_id"`
		Category string `json:"category"`
		Severity string `json:"severity"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Title    string `json:"title"`
	} `json:"findings"`
}

// ErrUnavailable is returned when the scan could not run (no eligible
// container, transport failure, parse failure). Callers map it to their
// fail-closed semantics.
var ErrUnavailable = errors.New("skill scan unavailable")

// Config configures a Client.
type Config struct {
	// KubeMode selects the backend: "embedded" → Docker Engine API;
	// anything else → the k8s backend (fail-closed in v1).
	KubeMode string
	// Client/Namespace resolve the Manager CR (container + runtime
	// check). A nil Client makes embedded scans unavailable.
	Client    client.Client
	Namespace string
	// ResourcePrefix derives the manager container name ("" = default).
	ResourcePrefix auth.ResourcePrefix
	// SocketPath is the Docker unix socket (embedded).
	SocketPath string
	// PythonBin is the interpreter inside the container (default
	// "python3" — the container PATH already prefers the qwenpaw venv).
	PythonBin string
	Timeout   time.Duration
	CacheTTL  time.Duration // default 30m
	CacheMax  int           // default 100
	// HTTPClient overrides the Docker API client (tests use an httptest
	// TCP server instead of the unix socket).
	HTTPClient *http.Client
	// BaseURL overrides the API base (tests).
	BaseURL string
}

// Client executes skill scans in a container.
type Client struct {
	cfg       Config
	backend   scanBackend
	mu        sync.Mutex
	cache     map[string]cacheEntry
	cacheFIFO []string
}

type cacheEntry struct {
	at      time.Time
	verdict SkillScanVerdict
}

// New creates a Client. KubeMode selects the backend: "embedded" → Docker
// Engine API over the socket; any other value → the k8s backend
// (fail-closed in v1).
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 30 * time.Minute
	}
	if cfg.CacheMax <= 0 {
		cfg.CacheMax = 100
	}
	if cfg.PythonBin == "" {
		cfg.PythonBin = "python3"
	}
	var backend scanBackend
	if cfg.KubeMode == "embedded" {
		cli := cfg.HTTPClient
		base := cfg.BaseURL
		if cli == nil {
			cli = newSocketClient(cfg.SocketPath)
			base = "http://localhost"
		}
		backend = newDockerBackend(cfg, cli, base)
	} else {
		backend = &k8sBackend{}
	}
	return &Client{cfg: cfg, backend: backend, cache: map[string]cacheEntry{}}
}

// ScanSkill scans the in-memory skill payload. A returned error means the
// scan could not run — callers must fail closed.
func (c *Client) ScanSkill(ctx context.Context, name string, files map[string][]byte) (SkillScanVerdict, error) {
	key := contentKey(name, files)
	if v, ok := c.cacheGet(key); ok {
		return v, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	out, err := c.backend.Run(runCtx, name, files)
	if err != nil {
		return SkillScanVerdict{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	verdict, err := parseVerdict(out)
	if err != nil {
		return SkillScanVerdict{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	c.cachePut(key, verdict)
	return verdict, nil
}

// scanBackend runs one scan and returns the probe's raw JSON line.
type scanBackend interface {
	Run(ctx context.Context, name string, files map[string][]byte) (string, error)
}

// contentKey is the cache key: the sha256 of (name + sorted
// path\x00content pairs). Unchanged content → same key.
func contentKey(name string, files map[string][]byte) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(files[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Client) cacheGet(key string) (SkillScanVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok {
		return SkillScanVerdict{}, false
	}
	if time.Since(e.at) > c.cfg.CacheTTL {
		delete(c.cache, key)
		return SkillScanVerdict{}, false
	}
	return e.verdict, true
}

func (c *Client) cachePut(key string, v SkillScanVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.cache[key]; !exists {
		c.cacheFIFO = append(c.cacheFIFO, key)
	}
	c.cache[key] = cacheEntry{at: time.Now(), verdict: v}
	// FIFO eviction (v1; true LRU is not worth the machinery at this size).
	for len(c.cacheFIFO) > c.cfg.CacheMax {
		old := c.cacheFIFO[0]
		c.cacheFIFO = c.cacheFIFO[1:]
		delete(c.cache, old)
	}
}

// parseVerdict parses the probe's JSON line. The probe writes the verdict
// file itself, so the content is the JSON line verbatim (no interpreter
// noise); trailing whitespace is tolerated.
func parseVerdict(out string) (SkillScanVerdict, error) {
	line := strings.TrimSpace(out)
	if line == "" {
		return SkillScanVerdict{}, errors.New("scanner produced no output")
	}
	// The last line is the payload (tolerate a leading traceback from a
	// hard crash that still managed to emit).
	if idx := strings.LastIndex(line, "\n"); idx >= 0 {
		line = strings.TrimSpace(line[idx+1:])
	}
	var po probeOutput
	if err := json.Unmarshal([]byte(line), &po); err != nil {
		if len(line) > 120 {
			line = line[:120] + "…"
		}
		return SkillScanVerdict{}, fmt.Errorf("unparseable scanner output %q: %w", line, err)
	}
	switch po.Status {
	case "pass", "warn", "block":
	case "unavailable":
		return SkillScanVerdict{}, fmt.Errorf("scanner unavailable: %s", po.Detail)
	default:
		return SkillScanVerdict{}, fmt.Errorf("unknown scan status %q", po.Status)
	}
	findings := make([]SkillUploadFinding, 0, len(po.Findings))
	for _, f := range po.Findings {
		findings = append(findings, SkillUploadFinding{
			RuleID:   f.RuleID,
			Category: f.Category,
			Severity: f.Severity,
			File:     f.File,
			Line:     f.Line,
			Title:    f.Title,
		})
	}
	return SkillScanVerdict{Status: po.Status, Findings: findings}, nil
}
