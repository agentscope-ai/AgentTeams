package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/skillscan"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// globalSkillsPrefix is the deployment-wide skill staging area maintained by
// the dashboard's skill-upload flow. Listing it read-only gives the catalog
// its "shared" half — the set of skills available for distribution across
// the deployment.
//
// Retention semantics: no worker entrypoint consumes this prefix
// automatically. A shared skill reaches a worker only through per-worker
// distribution (dashboard Worker dialog, or L1/L2 PUT /workers skills),
// which also records the assignment in spec.skills. Deleting
// agents/global/skills/{name}/ removes the skill from the catalog (and the
// dashboard's global area); already-distributed per-worker copies
// (agents/<worker>/skills/{name}/) and existing spec.skills assignments are
// NOT touched — there is no cascade.
const globalSkillsPrefix = "agents/global/skills/"

// SkillInfo is one entry of the read-only skill catalog. It carries
// identity/availability only — never skill content, credentials, or
// registry connection details.
type SkillInfo struct {
	Name         string             `json:"name"`
	Description  string             `json:"description,omitempty"`
	Source       string             `json:"source"`                 // "builtin" | "shared" | "team"
	Version      string             `json:"version,omitempty"`      // builtin only: SKILL.md frontmatter version
	Requirements *SkillRequirements `json:"requirements,omitempty"` // builtin only: frontmatter requires block
	UpdatedAt    string             `json:"updated_at,omitempty"`   // shared/team only: last listing timestamp (RFC3339 UTC)
	Agents       []string           `json:"agents,omitempty"`       // builtin only: template dirs providing the skill
	Runtimes     []string           `json:"runtimes,omitempty"`     // runtimes for which the skill is available
}

// SkillRequirements mirrors the SKILL.md "requires" declaration
// (top-level or under metadata.{openclaw,qwenpaw,clawdbot}): the binaries,
// env vars, and MCP server names the skill needs at runtime. The three
// metadata namespaces are the conventions the OpenClaw, QwenPaw, and
// Clawdbot runtimes each honour when parsing skill frontmatter. The catalog
// exposes them so workbenches can warn before assignment; enforcement is
// runtime-dependent (the qwenpaw 2.2.x registry gates skill activation on
// require_bins/envs/mcps; other runtimes in AllWorkerRuntimes have no
// equivalent gate yet, so a satisfied declaration is necessary but not
// sufficient there).
type SkillRequirements struct {
	RequireBins []string `json:"require_bins,omitempty"`
	RequireEnvs []string `json:"require_envs,omitempty"`
	RequireMcps []string `json:"require_mcps,omitempty"`
}

// SkillListResponse is the payload of GET /api/v1/skills.
type SkillListResponse struct {
	Skills []SkillInfo `json:"skills"`
	Total  int         `json:"total"`
}

// SkillsHandler serves the read-only skill catalog: built-in skills from the
// controller's agent template directories (availability per runtime derived
// from service.BuiltinAgentDir, the same function the Deployer uses to seed
// workers) plus the deployment-wide shared skills staged under
// agents/global/skills/. It never reads skill content beyond the SKILL.md
// frontmatter (name/description/version/requires) of builtin skills; the
// shared half is name + listing timestamp by design (list-on-read, no
// per-skill object fetches — shared SKILL.md metadata such as description,
// version, and requires is a v2 candidate).
type SkillsHandler struct {
	workerAgentDir string
	oss            oss.StorageClient
	client         client.Client // Team CR existence checks for ?team= scope
	namespace      string
	scanner        skillscan.SkillScanner
}

func NewSkillsHandler(workerAgentDir string, o oss.StorageClient, c client.Client, namespace string, scanner skillscan.SkillScanner) *SkillsHandler {
	return &SkillsHandler{workerAgentDir: workerAgentDir, oss: o, client: c, namespace: namespace, scanner: scanner}
}

// ListSkills handles GET /api/v1/skills.
//
// No-param (the L1 deployment-level catalog): admin (L1) only — builtin +
// agents/global/skills/ (the "individual" layer, managed by the admin).
// Non-admin callers are rejected with a self-explanatory 400.
//
// ?team=T (the team-scoped catalog): builtin + teams/T/skills/.
//   - admin: any team (Team CR existence checked);
//   - L2 human: own teams only (Human CR accessibleTeams, by Team CR name);
//   - team leader: own team only;
//   - manager / worker: 403 (the Manager agent does not participate in
//     team-skill paths);
//   - cross-team or unknown team: 404 — deliberately indistinguishable
//     (W8 anti-probing: a 403 here would let a scoped caller probe which
//     teams exist).
//
// Team-scope check errors; the status-code mapping is centralized in
// writeTeamScopeError (403 = role not allowed, 404 = cross-team or unknown
// — deliberately indistinguishable, W8 anti-probing, 400 = bad input).
var (
	errScopeForbidden = errors.New("team-scope not available for this role")
	errScopeNotFound  = errors.New("team not found")
	errScopeRequired  = errors.New("team required")
)

// checkTeamScope validates (caller, team) against the team-scope matrix
// shared by the ?team= catalog and the POST /skills upload:
//
//	admin          → any team (Team CR existence checked)
//	L2 human       → own teams only (Human CR accessibleTeams, by Team CR name)
//	team leader    → own team only
//	manager/worker → not allowed
//
// Cross-team and unknown teams yield the same error (404 at the boundary)
// so a scoped caller cannot probe which teams exist.
func (h *SkillsHandler) checkTeamScope(ctx context.Context, caller *auth.CallerIdentity, team string) error {
	if team == "" {
		return errScopeRequired
	}
	if caller == nil {
		return errScopeForbidden
	}
	switch caller.Role {
	case auth.RoleAdmin:
	case auth.RoleHuman, auth.RoleTeamLeader:
		if !caller.TeamMatches(team) {
			return errScopeNotFound
		}
	default:
		return errScopeForbidden
	}
	if h.client == nil {
		return errScopeNotFound
	}
	var teamCR v1beta1.Team
	if err := h.client.Get(ctx, client.ObjectKey{Name: team, Namespace: h.namespace}, &teamCR); err != nil {
		return errScopeNotFound
	}
	return nil
}

func writeTeamScopeError(w http.ResponseWriter, err error) {
	switch err {
	case errScopeForbidden:
		httputil.WriteError(w, http.StatusForbidden, "team-scope catalog is not available for this role")
	case errScopeNotFound:
		httputil.WriteError(w, http.StatusNotFound, "team not found")
	default:
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
	}
}

func (h *SkillsHandler) ListSkills(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	team := r.URL.Query().Get("team")
	if team == "" {
		if caller == nil || caller.Role != auth.RoleAdmin {
			httputil.WriteError(w, http.StatusBadRequest, "team scope required")
			return
		}
		h.writeCatalog(w, r, nil)
		return
	}
	if err := h.checkTeamScope(r.Context(), caller, team); err != nil {
		writeTeamScopeError(w, err)
		return
	}
	h.writeCatalog(w, r, &team)
}

// SkillUploadRequest is the POST /api/v1/skills payload: one team skill
// (name + files). File contents are base64 — a skill may embed arbitrary
// bytes (scripts, data, images).
type SkillUploadRequest struct {
	Team  string            `json:"team"`
	Name  string            `json:"name"`
	Files map[string]string `json:"files"` // relative path -> base64 content
}

// SkillUploadScan is the upload-scan outcome in the response. Status:
// "pass" | "warn" | "skipped" — "skipped" means the scan could not run
// (no scanner backend or the container round trip failed) and the upload
// proceeded best-effort (a warning was logged). Blocked uploads never
// reach the response body (422 with findings).
type SkillUploadScan struct {
	Status   string                         `json:"status"`
	Findings []skillscan.SkillUploadFinding `json:"findings,omitempty"`
}

// SkillUploadResponse is the successful upload response.
type SkillUploadResponse struct {
	Name  string           `json:"name"`
	Scope string           `json:"scope"` // "team" | "deployment"
	Team  string           `json:"team,omitempty"`
	Files int              `json:"files"`
	Scan  *SkillUploadScan `json:"scan"`
}

// maxSkillUploadBytes bounds both the raw zip and the uncompressed payload
// (zip-bomb defence): 64 MB, matching the dashboard's current limit.
const maxSkillUploadBytes = 64 << 20

var skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validateSkillName enforces the skill-directory naming rule: lowercase
// kebab-case, at most 64 characters (the name doubles as the storage
// directory name, so it must stay a safe path component).
func validateSkillName(name string) error {
	if !skillNameRe.MatchString(name) {
		return fmt.Errorf("skill name %q is invalid: must match %s", name, skillNameRe.String())
	}
	return nil
}

// extractSkillZip unpacks the uploaded zip into a validated in-memory
// skill: (name, files). The zip must contain exactly one top-level
// directory (the skill root), which must be a valid skill name; SKILL.md
// must sit directly in the root and its frontmatter name must equal the
// directory name (the storage layout is keyed by that name).
//
// Security: absolute paths, backslashes, empty / "." / ".." components,
// symlink entries, and any payload decompressing beyond maxSkillUploadBytes
// (zip bomb) are rejected with a 400-class error.
func extractSkillZip(data []byte) (name string, files map[string][]byte, err error) {
	if len(data) > maxSkillUploadBytes {
		return "", nil, fmt.Errorf("zip exceeds the %d-byte limit", maxSkillUploadBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", nil, fmt.Errorf("invalid zip: %v", err)
	}
	out := make(map[string][]byte)
	var total int
	for _, entry := range zr.File {
		zname := entry.Name
		if zname == "" {
			return "", nil, errors.New("zip entry with an empty name")
		}
		// Zip paths are forward-slash; a backslash is a path-traversal
		// attempt on any consumer.
		if strings.ContainsRune(zname, '\\') || strings.HasPrefix(zname, "/") {
			return "", nil, fmt.Errorf("unsafe zip entry %q", zname)
		}
		if entry.Mode()&fs.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("symlink entries are not allowed: %q", zname)
		}
		parts := strings.Split(zname, "/")
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				return "", nil, fmt.Errorf("unsafe zip entry %q", zname)
			}
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if len(parts) < 2 {
			return "", nil, errors.New("the zip must contain a single top-level skill directory")
		}
		if name == "" {
			name = parts[0]
		} else if name != parts[0] {
			return "", nil, errors.New("the zip must contain exactly one top-level directory")
		}
		rel := strings.Join(parts[1:], "/")
		if _, exists := out[rel]; exists {
			return "", nil, fmt.Errorf("duplicate zip entry %q", rel)
		}
		rc, err := entry.Open()
		if err != nil {
			return "", nil, err
		}
		content, err := io.ReadAll(io.LimitReader(rc, maxSkillUploadBytes+1))
		rc.Close()
		if err != nil {
			return "", nil, err
		}
		if len(content) > maxSkillUploadBytes {
			return "", nil, fmt.Errorf("zip entry %q exceeds the %d-byte limit", rel, maxSkillUploadBytes)
		}
		total += len(content)
		if total > maxSkillUploadBytes {
			return "", nil, fmt.Errorf("uncompressed skill exceeds the %d-byte limit (zip bomb)", maxSkillUploadBytes)
		}
		out[rel] = content
	}
	if name == "" {
		return "", nil, errors.New("the zip is empty")
	}
	if err := validateSkillName(name); err != nil {
		return "", nil, err
	}
	skillMD, ok := out["SKILL.md"]
	if !ok {
		return "", nil, errors.New("SKILL.md is required at the skill root")
	}
	// Frontmatter name must equal the directory name (the storage key).
	block := frontmatterBlock(string(skillMD))
	var doc map[string]any
	if block == "" {
		return "", nil, errors.New("SKILL.md has no YAML frontmatter")
	}
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		return "", nil, fmt.Errorf("SKILL.md frontmatter is not valid YAML: %v", err)
	}
	if fmName, _ := doc["name"].(string); strings.TrimSpace(fmName) != name {
		return "", nil, fmt.Errorf("SKILL.md frontmatter name %q does not match the skill directory name %q", strings.TrimSpace(fmName), name)
	}
	return name, out, nil
}

// summarizeFindings compacts scan findings into an error fragment (severity
// + rule id; never file contents).
func summarizeFindings(findings []skillscan.SkillUploadFinding) string {
	if len(findings) == 0 {
		return "no findings reported"
	}
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, f.Severity+":"+f.RuleID)
	}
	return strings.Join(parts, ", ")
}

// UploadSkill handles POST /api/v1/skills: upload (or replace) one skill.
//
// Request: multipart/form-data —
//
//	file   the skill zip (single top-level directory, 64 MB max)
//	scope  "team" (→ teams/<t>/skills/<name>/) | "deployment"
//	       (→ agents/global/skills/<name>/, admin only)
//	team   Team CR name (required when scope=team)
//
// Flow: role gate (403 — manager/leader/worker are already denied by the
// authorizer; the handler re-checks) → structure validation (400) →
// team-scope check (403/404, the same matrix as the ?team= catalog) →
// scan ① (best-effort: block → 422 with findings; unavailable → proceed
// with scan.status="skipped" + a warning log) → exact-copy mirror
// (Overwrite + Remove: a re-upload replaces stale files).
//
// Response: 200 {name, scope, team?, files, scan:{status, findings?}}.
func (h *SkillsHandler) UploadSkill(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	logger := log.FromContext(r.Context())

	// Role gate (handler layer — the authorizer already denies
	// manager/leader/worker; both layers are pinned by tests).
	if caller == nil || (caller.Role != auth.RoleAdmin && caller.Role != auth.RoleHuman) {
		httputil.WriteError(w, http.StatusForbidden,
			"skill upload is admin (any scope) or L2 human (own team) only")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSkillUploadBytes+1<<20)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}
	scope := strings.TrimSpace(r.FormValue("scope"))
	team := strings.TrimSpace(r.FormValue("team"))
	file, _, err := r.FormFile("file")
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "missing form field \"file\" (the skill zip)")
		return
	}
	defer file.Close()
	zipData, err := io.ReadAll(io.LimitReader(file, maxSkillUploadBytes+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "read skill zip: "+err.Error())
		return
	}

	// Scope + target prefix.
	var targetPrefix string
	switch scope {
	case "team":
		if team == "" {
			httputil.WriteError(w, http.StatusBadRequest, "field \"team\" is required when scope=team")
			return
		}
		if err := h.checkTeamScope(r.Context(), caller, team); err != nil {
			writeTeamScopeError(w, err)
			return
		}
	case "deployment":
		if caller.Role != auth.RoleAdmin {
			httputil.WriteError(w, http.StatusForbidden, "scope=deployment is admin only")
			return
		}
	default:
		httputil.WriteError(w, http.StatusBadRequest, "field \"scope\" must be \"team\" or \"deployment\"")
		return
	}

	// Structure validation (400): single root dir, name rule, SKILL.md,
	// frontmatter name match, zip-slip defence, 64 MB.
	name, files, err := extractSkillZip(zipData)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if scope == "team" {
		targetPrefix = "teams/" + team + "/skills/" + name + "/"
	} else {
		targetPrefix = globalSkillsPrefix + name + "/"
	}

	// Scan ① — best-effort (the mandatory gate is scan ② at assign time):
	// block → 422 with findings; pass/warn → proceed, findings surfaced;
	// unavailable → proceed with scan.status="skipped" + a warning log.
	scan := SkillUploadScan{Status: "skipped"}
	if h.scanner != nil {
		if verdict, err := h.scanner.ScanSkill(r.Context(), name, files); err == nil {
			if verdict.Status == "block" {
				httputil.WriteError(w, http.StatusUnprocessableEntity,
					"skill scan blocked the upload: "+summarizeFindings(verdict.Findings))
				return
			}
			scan = SkillUploadScan{Status: verdict.Status, Findings: verdict.Findings}
		} else {
			logger.Info("skill scan unavailable; upload proceeds best-effort (scan marked skipped)",
				"skill", name, "scope", scope, "team", team, "err", err.Error())
		}
	}

	// Exact-copy mirror (a re-upload replaces stale files): stage the
	// files in a temp directory and mirror with Overwrite + Remove.
	stage, err := os.MkdirTemp("", "skill-upload-")
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "stage skill files: "+err.Error())
		return
	}
	defer os.RemoveAll(stage)
	for path, data := range files {
		full := filepath.Join(stage, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "stage skill files: "+err.Error())
			return
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "stage skill files: "+err.Error())
			return
		}
	}
	if err := h.oss.Mirror(r.Context(), stage, targetPrefix, oss.MirrorOptions{Overwrite: true, Remove: true}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "mirror skill into storage: "+err.Error())
		return
	}

	// Audit seam (PR-A parallel form: structured log line; switches to
	// audit.Record once the capability foundation lands): who/what/where/how.
	logger.Info("skill uploaded",
		"caller", caller.Username, "role", caller.Role,
		"scope", scope, "team", team, "skill", name,
		"files", len(files), "scan", scan.Status)

	httputil.WriteJSON(w, http.StatusOK, SkillUploadResponse{
		Name:  name,
		Scope: scope,
		Team:  team,
		Files: len(files),
		Scan:  &scan,
	})
}

// writeCatalog builds the catalog: the builtin half always, plus the shared
// half (agents/global/skills/) for the no-param view or the team half
// (teams/<t>/skills/) for the ?team= view.
func (h *SkillsHandler) writeCatalog(w http.ResponseWriter, r *http.Request, team *string) {
	skills := map[string]*SkillInfo{}

	for _, tmpl := range h.builtinTemplates() {
		skillRoot := filepath.Join(tmpl.dir, "skills")
		entries, err := os.ReadDir(skillRoot)
		if err != nil {
			continue // missing template dir for this deployment
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name, description, version, requirements := parseSkillFrontmatter(filepath.Join(skillRoot, entry.Name(), "SKILL.md"))
			if name == "" {
				name = entry.Name()
			}
			if info, ok := skills[name]; ok {
				if info.Source == "builtin" {
					info.Agents = appendUniqueStrings(info.Agents, tmpl.dirName)
					info.Runtimes = unionSorted(info.Runtimes, tmpl.runtimes)
					if info.Description == "" {
						info.Description = description
					}
					if info.Version == "" {
						info.Version = version
					}
					if info.Requirements == nil {
						info.Requirements = requirements
					}
				}
				continue
			}
			skills[name] = &SkillInfo{
				Name:         name,
				Description:  description,
				Version:      version,
				Requirements: requirements,
				Source:       "builtin",
				Agents:       []string{tmpl.dirName},
				Runtimes:     append([]string{}, tmpl.runtimes...),
			}
		}
	}

	// Object-storage half: the no-param view lists the deployment-wide
	// shared skills (agents/global/skills/, source "shared"); the ?team=
	// view lists the team layer (teams/<t>/skills/, source "team"). Both
	// halves share the exact same listing contract (listSkillDirs) — the
	// team layer is the shared layer's team-scope sibling.
	if team == nil {
		h.listSkillDirs(r.Context(), globalSkillsPrefix, "shared", skills)
	} else {
		h.listSkillDirs(r.Context(), "teams/"+*team+"/skills/", "team", skills)
	}

	list := make([]SkillInfo, 0, len(skills))
	for _, info := range skills {
		sort.Strings(info.Agents)
		sort.Strings(info.Runtimes)
		list = append(list, *info)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	httputil.WriteJSON(w, http.StatusOK, SkillListResponse{Skills: list, Total: len(list)})
}

// listSkillDirs fills skills with the direct-child directory entries under
// prefix, tagged with the given source ("shared" | "team"). Contract for
// both halves: a listing failure (prefix absent, storage down) degrades to
// an empty set rather than failing the whole catalog; directory entries
// only (mc ls marks them with a trailing "/"); bare files and dot-entries
// are non-skill artifacts; builtin names win on collision. Entries carry
// UpdatedAt from the listing when the backend exposes it (the mc ls line
// date; "" when unparseable) — no per-skill object fetch.
func (h *SkillsHandler) listSkillDirs(ctx context.Context, prefix, source string, skills map[string]*SkillInfo) {
	if h.oss == nil {
		return
	}
	entries, err := h.oss.ListObjectsDetailed(ctx, prefix)
	if err != nil {
		return
	}
	for _, entry := range entries {
		raw := entry.Name
		if !strings.HasSuffix(raw, "/") {
			continue
		}
		name := strings.TrimSuffix(raw, "/")
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		if _, ok := skills[name]; ok {
			continue
		}
		skills[name] = &SkillInfo{
			Name:      name,
			Source:    source,
			UpdatedAt: entry.UpdatedAt,
			Runtimes:  append([]string{}, service.AllWorkerRuntimes...),
		}
	}
}

// builtinTemplate is one template directory and the runtimes it seeds.
type builtinTemplate struct {
	dir      string // absolute template dir
	dirName  string // directory name (as reported in SkillInfo.Agents)
	runtimes []string
}

// builtinTemplates derives the template→runtime mapping from
// service.BuiltinAgentDir — the same function the Deployer uses to seed
// workers — so the catalog's per-runtime availability can never drift from
// what workers actually receive.
func (h *SkillsHandler) builtinTemplates() []builtinTemplate {
	if h.workerAgentDir == "" {
		return nil
	}
	byDir := map[string]*builtinTemplate{}
	order := []string{}
	bucket := func(dir string) *builtinTemplate {
		b, ok := byDir[dir]
		if !ok {
			b = &builtinTemplate{dir: dir, dirName: filepath.Base(dir)}
			byDir[dir] = b
			order = append(order, dir)
		}
		return b
	}
	for _, rt := range service.AllWorkerRuntimes {
		workerTmpl := bucket(service.BuiltinAgentDir(h.workerAgentDir, "worker", rt))
		workerTmpl.runtimes = appendUniqueStrings(workerTmpl.runtimes, rt)
		leaderTmpl := bucket(service.BuiltinAgentDir(h.workerAgentDir, "team_leader", rt))
		leaderTmpl.runtimes = appendUniqueStrings(leaderTmpl.runtimes, rt)
	}
	out := make([]builtinTemplate, 0, len(order))
	for _, dir := range order {
		t := byDir[dir]
		sort.Strings(t.runtimes)
		out = append(out, *t)
	}
	return out
}

// requirementsNamespaces are the provider metadata namespaces QwenPaw 2.2.x
// honours for the skill "requires" block (store.py
// _REQUIREMENTS_METADATA_NAMESPACES). Kept in sync with the worker-side
// parser so catalog declarations match runtime enforcement.
var requirementsNamespaces = []string{"openclaw", "qwenpaw", "clawdbot"}

// parseSkillFrontmatter extracts the catalog-relevant fields from the YAML
// frontmatter of a SKILL.md: name, description, version (top-level or under
// metadata), and the "requires" block (top-level, or under
// metadata.{openclaw,qwenpaw,clawdbot}). Returns zero values when the file or
// frontmatter is missing — callers fall back to the directory name. The
// parser is intentionally lenient: malformed frontmatter yields whatever
// fields decode cleanly, mirroring the worker-side tolerance.
func parseSkillFrontmatter(path string) (name, description, version string, requirements *SkillRequirements) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", nil
	}
	block := frontmatterBlock(string(data))
	if block == "" {
		return "", "", "", nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		return "", "", "", nil
	}
	strVal := func(v any) string {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		return ""
	}
	name = strVal(doc["name"])
	description = strVal(doc["description"])
	version = strVal(doc["version"])
	if version == "" {
		if meta, ok := doc["metadata"].(map[string]any); ok {
			version = strVal(meta["version"])
		}
	}
	requirements = parseRequires(doc)
	return name, description, version, requirements
}

// frontmatterBlock returns the text between the leading "---" line and the
// next "---" line, or "" when the file has no frontmatter.
func frontmatterBlock(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	var fm []string
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return strings.Join(fm, "\n")
		}
		fm = append(fm, line)
	}
	return ""
}

// parseRequires resolves the "requires" declaration with the same precedence
// as QwenPaw 2.2.x: metadata.{openclaw,qwenpaw,clawdbot}.requires first,
// then metadata.requires, then top-level requires. A bare list is shorthand
// for bins. Returns nil when no requires block is declared or it carries no
// usable entries.
func parseRequires(doc map[string]any) *SkillRequirements {
	var raw any
	if meta, ok := doc["metadata"].(map[string]any); ok {
		for _, ns := range requirementsNamespaces {
			if p, ok := meta[ns].(map[string]any); ok {
				if r, ok := p["requires"]; ok && r != nil {
					raw = r
					break
				}
			}
		}
		if raw == nil {
			if r, ok := meta["requires"]; ok {
				raw = r
			}
		}
	}
	if raw == nil {
		raw = doc["requires"]
	}
	if raw == nil {
		return nil
	}
	req := &SkillRequirements{}
	take := func(v any) []string {
		var out []string
		switch t := v.(type) {
		case []any:
			for _, x := range t {
				out = append(out, stringItems(x)...)
			}
		case string:
			out = append(out, t)
		}
		seen := map[string]bool{}
		var res []string
		for _, s := range out {
			s = strings.TrimSpace(s)
			if s != "" && !seen[s] {
				seen[s] = true
				res = append(res, s)
			}
		}
		return res
	}
	switch t := raw.(type) {
	case []any:
		req.RequireBins = take(t)
	case map[string]any:
		req.RequireBins = take(t["bins"])
		req.RequireEnvs = take(t["env"])
		req.RequireMcps = take(t["mcp"])
	}
	if len(req.RequireBins) == 0 && len(req.RequireEnvs) == 0 && len(req.RequireMcps) == 0 {
		return nil
	}
	sort.Strings(req.RequireBins)
	sort.Strings(req.RequireEnvs)
	sort.Strings(req.RequireMcps)
	return req
}

func stringItems(v any) []string {
	if s, ok := v.(string); ok {
		return []string{s}
	}
	return nil
}

func appendUniqueStrings(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// unionSorted merges two string slices, dropping duplicates.
func unionSorted(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
