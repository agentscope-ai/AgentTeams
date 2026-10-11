// Package server — worker coding-CLI management surface (P2, controller-side).
//
// The controller manages each worker's coding CLIs (qwen-code, opencode)
// by running commands INSIDE the worker container through the backend's
// exec capability (embedded docker mode holds the docker socket). No
// worker-side cooperation is required: no worker router, no image change,
// no dependency on a specific worker runtime release. Any current runtime
// (openclaw / qwenpaw / coworker / deepseek-harness / ...) with the CLI
// binary + npm in the container is manageable — the same surface works
// for all of them.
//
// Routes (fixed, never a generic reverse proxy):
//
//	GET  /api/v1/workers/{name}/coding-cli            probe all CLIs
//	GET  /api/v1/workers/{name}/coding-cli/{cli}/settings   read (secrets redacted)
//	PUT  /api/v1/workers/{name}/coding-cli/{cli}/settings   merge + atomic write
//	POST /api/v1/workers/{name}/coding-cli/{cli}/install    async npm install/uninstall -> 202
//	GET  /api/v1/workers/{name}/coding-cli/{cli}/install/{task_id}
//
// Discipline:
//   - embedded-mode gate (503 in kube mode — the exec capability is
//     docker-backend only; uniform so worker existence cannot be probed);
//   - team scope via the same findTeamMember predicate as channels;
//   - team leaders are read-only on the coding-CLI surface;
//   - settings reads are re-redacted server-side (defense in depth);
//   - writes are read-modify-merge + atomic replace (tmp + rename) so a
//     partial write can never corrupt the CLI's settings;
//   - installs run as controller-side async tasks (goroutine + in-memory
//     task map) because npm on a weak ARM box can take minutes;
//   - every mutation is audit-logged.
//
// Relation to the worker-side surface (agentscope-ai/QwenPaw#8156): the
// qwenpaw app also exposes /api/coding-cli for the same CLIs (worker
// self-management, works in kube mode). This controller-side surface is
// the runtime-agnostic path that ships without any QwenPaw release; both
// speak the same CLI contract (registry, redaction, merge semantics).
package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// codingCliExecTimeout bounds synchronous exec calls (probe, cat,
	// settings read). npm installs run async with codingCliInstallTimeout.
	codingCliExecTimeout = 15 * time.Second
	// codingCliInstallTimeout bounds the npm install/uninstall run.
	codingCliInstallTimeout = 10 * time.Minute
	// codingCliBodyCap bounds request/response bodies.
	codingCliBodyCap = 128 << 10
	// codingCliTaskTTL prunes finished install tasks from memory.
	codingCliTaskTTL = 2 * time.Hour
)

var (
	// codingCliTaskIDPattern matches controller-produced task ids.
	codingCliTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// codingCliSpec is the per-CLI management contract. Mirrors the qwenpaw
// CliSpec (agentscope-ai/QwenPaw P1a registry) so both surfaces agree on
// binary, package, settings location and auth detection.
type codingCliSpec struct {
	ID            string
	NpmPackage    string
	BinName       string
	SettingsPath  string // ~ expanded at use time via the container shell
	AuthDotPaths  []string
	ExtraAuthFiles []string
}

// codingCliRegistry is the ordered single source of truth for managed CLIs:
// membership = presence in the slice, display order = slice order. There is no
// parallel id allow-list (a second list drifts — the whack-a-mole trap). A new
// CLI is added here once and picked up by probe/settings/install automatically.
var codingCliRegistry = []codingCliSpec{
	{
		ID:           "qwen-code",
		NpmPackage:   "@qwen-code/qwen-code",
		BinName:      "qwen",
		SettingsPath: "~/.qwen/settings.json",
		AuthDotPaths: []string{"security.auth.apiKey"},
	},
	{
		ID:             "opencode",
		NpmPackage:     "opencode-ai",
		BinName:        "opencode",
		SettingsPath:   "~/.config/opencode/opencode.json",
		ExtraAuthFiles: []string{"~/.local/share/opencode/auth.json"},
	},
}

// codingCliSpecByID resolves a CLI by id against the registry (the single
// source). Unknown ids are rejected (400) so the route never doubles as a
// path oracle.
func codingCliSpecByID(id string) (codingCliSpec, bool) {
	for _, s := range codingCliRegistry {
		if s.ID == id {
			return s, true
		}
	}
	return codingCliSpec{}, false
}

// codingCliExecer is the minimal backend capability this handler needs.
// *backend.DockerBackend implements it (embedded mode); the kubernetes
// backend does not, which is why the surface is embedded-gated.
type codingCliExecer interface {
	Exec(ctx context.Context, name string, command []string, timeout time.Duration) (string, string, int, error)
	WriteFile(ctx context.Context, name, path, content string) error
}

// CodingCliHandler manages worker coding CLIs via container exec.
type CodingCliHandler struct {
	client    client.Client
	namespace string
	kubeMode  string
	audit     *audit.Client

	// execBackend resolves the worker's exec-capable backend. Injectable
	// for tests; default reads from the backend registry.
	execBackend func(ctx context.Context, name string) (codingCliExecer, error)

	// install tasks (in-memory; controller restart drops in-flight state,
	// same property as the qwenpaw worker-side task map).
	tasksMu    sync.Mutex
	tasks      map[string]*codingCliInstallTask
	taskSeq    int
	lastPrune  time.Time
}

type codingCliInstallTask struct {
	worker     string // owning worker: busy gate + status lookup are per-worker
	cli        string
	action     string // install | uninstall
	version    string // npm version tag (install only)
	state      string // running | done | failed
	exitCode   int
	stderr     string // tail
	pkg        string
	finishedAt time.Time
}

// NewCodingCliHandler creates the handler with the default backend
// resolution from the registry.
func NewCodingCliHandler(c client.Client, namespace, kubeMode string, ac *audit.Client, reg *backend.Registry) *CodingCliHandler {
	h := &CodingCliHandler{
		client:     c,
		namespace:  namespace,
		kubeMode:   kubeMode,
		audit:      ac,
		tasks:      map[string]*codingCliInstallTask{},
	}
	h.execBackend = func(ctx context.Context, name string) (codingCliExecer, error) {
		b, err := reg.GetWorkerBackend(ctx, name)
		if err != nil {
			return nil, err
		}
		eb, ok := b.(codingCliExecer)
		if !ok {
			return nil, fmt.Errorf("worker backend %q does not support container exec", b.Name())
		}
		return eb, nil
	}
	return h
}

// --- scope ---------------------------------------------------------------

// codingCliScope validates the worker name, enforces embedded mode and team
// scope, and resolves the exec-capable backend. Mirrors channelsScope.
func (h *CodingCliHandler) codingCliScope(w http.ResponseWriter, r *http.Request, name string) (codingCliExecer, bool) {
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return nil, false
	}
	// Uniform 503 in non-embedded mode: the exec capability is
	// docker-backend only, and a uniform answer keeps worker existence
	// unprobed — same discipline as the checkpoint proxy.
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker coding-CLI management requires embedded mode")
		return nil, false
	}
	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return nil, false
		}
		writeK8sError(w, "get worker coding-cli", err)
		return nil, false
	}
	// Standalone workers (no team) resolve to "" which TeamMatches rejects,
	// hiding them from team-scoped callers as 404; L3 humans may read
	// exactly their assigned workers.
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker coding-cli", err)
		return nil, false
	}
	teamName := ""
	if teamObj != nil {
		teamName = teamObj.Name
	}
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) {
		allowed := caller.TeamMatches(teamName)
		if !allowed && r.Method == http.MethodGet {
			allowed = caller.WorkerReadable(teamName, name)
		}
		if !allowed {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return nil, false
		}
	}
	eb, err := h.execBackend(r.Context(), name)
	if err != nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker exec backend unavailable: "+err.Error())
		return nil, false
	}
	return eb, true
}

// requireKnownCLI validates the {cli} path value against the allow-list.
func requireKnownCLI(w http.ResponseWriter, cli string) (codingCliSpec, bool) {
	spec, ok := codingCliSpecByID(cli)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "unknown coding CLI: "+cli)
		return spec, false
	}
	return spec, true
}

// --- probe ---------------------------------------------------------------

// listCLIs probes every registered CLI: binary present? version? auth set?
func (h *CodingCliHandler) listCLIs(w http.ResponseWriter, r *http.Request) {
	eb, ok := h.codingCliScope(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	result := map[string]interface{}{}
	for _, spec := range codingCliRegistry {
		result[spec.ID] = h.probeCLI(r.Context(), eb, r.PathValue("name"), spec)
	}
	writeCodingCliJSON(w, http.StatusOK, result)
}

func (h *CodingCliHandler) probeCLI(ctx context.Context, eb codingCliExecer, name string, spec codingCliSpec) map[string]interface{} {
	out, _, exit, err := eb.Exec(ctx, name, []string{spec.BinName, "--version"}, codingCliExecTimeout)
	if err != nil {
		return map[string]interface{}{"cli": spec.ID, "installed": false, "error": "probe failed: " + err.Error()}
	}
	if exit != 0 {
		return map[string]interface{}{"cli": spec.ID, "installed": false}
	}
	version := parseCodingCliVersion(out)
	return map[string]interface{}{
		"cli":       spec.ID,
		"installed": true,
		"version":   version,
		"auth":      h.probeCLIAuth(ctx, eb, name, spec),
	}
}

// parseCodingCliVersion extracts the first version-looking token from a
// CLI --version stdout (formats: "0.25.0", "qwen 0.25.0", "1.2.3-beta").
func parseCodingCliVersion(out string) string {
	for _, field := range strings.Fields(out) {
		f := strings.Trim(field, ".,")
		if len(f) >= 5 && isVersionLike(f) {
			return f
		}
	}
	return strings.TrimSpace(out)
}

func isVersionLike(s string) bool {
	if !strings.Contains(s, ".") || !strings.ContainsAny(s, "0123456789") {
		return false
	}
	for i, c := range s {
		if i == 0 {
			if c < '0' || c > '9' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c == '.' || c == '-' || c == '+' || c == 'a' || c == 'b' || c == 'v') {
			return false
		}
	}
	return true
}

func (h *CodingCliHandler) probeCLIAuth(ctx context.Context, eb codingCliExecer, name string, spec codingCliSpec) map[string]interface{} {
	// Extra auth files (opencode auth.json): check existence.
	for _, f := range spec.ExtraAuthFiles {
		if _, _, exit, err := eb.Exec(ctx, name, []string{"sh", "-c", "test -s " + f}, codingCliExecTimeout); err == nil && exit == 0 {
			return map[string]interface{}{"configured": true, "method": "auth-file"}
		}
	}
	// Settings dot-paths (qwen security.auth.apiKey).
	if _, _, exit, err := eb.Exec(ctx, name, []string{"sh", "-c", "test -f " + spec.SettingsPath}, codingCliExecTimeout); err == nil && exit == 0 {
		out, _, cexit, cerr := eb.Exec(ctx, name, []string{"sh", "-c", "cat " + spec.SettingsPath}, codingCliExecTimeout)
		if cerr == nil && cexit == 0 {
			var doc map[string]interface{}
			if json.Unmarshal([]byte(out), &doc) == nil {
				for _, dp := range spec.AuthDotPaths {
					if v := codingCliDotGet(doc, dp); v != nil {
						if s, isStr := v.(string); !isStr || s != "" {
							return map[string]interface{}{"configured": true, "method": "settings"}
						}
					}
				}
			}
		}
	}
	return map[string]interface{}{"configured": false}
}

// codingCliDotGet resolves "a.b.c" inside a decoded JSON object.
func codingCliDotGet(doc map[string]interface{}, dotPath string) interface{} {
	cur := interface{}(doc)
	for _, key := range strings.Split(dotPath, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur, ok = m[key]
		if !ok {
			return nil
		}
	}
	return cur
}

// --- settings ------------------------------------------------------------

func (h *CodingCliHandler) getCliSettings(w http.ResponseWriter, r *http.Request) {
	spec, ok := requireKnownCLI(w, r.PathValue("cli"))
	if !ok {
		return
	}
	eb, ok := h.codingCliScope(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	name := r.PathValue("name")
	out, _, exit, err := eb.Exec(r.Context(), name, []string{"sh", "-c", "cat " + spec.SettingsPath}, codingCliExecTimeout)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker exec failed: "+err.Error())
		return
	}
	if exit != 0 {
		// Settings file absent — report an empty (not error) state so the
		// frontend can offer "create from defaults".
		writeCodingCliJSON(w, http.StatusOK, map[string]interface{}{
			"cli": spec.ID, "path": spec.SettingsPath, "exists": false, "settings": map[string]interface{}{},
		})
		return
	}
	var settings map[string]interface{}
	if err := json.Unmarshal([]byte(out), &settings); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker settings file is not valid JSON: "+err.Error())
		return
	}
	writeCodingCliJSON(w, http.StatusOK, redactCodingCliSecrets(map[string]interface{}{
		"cli": spec.ID, "path": spec.SettingsPath, "exists": true, "settings": settings,
	}))
}

func (h *CodingCliHandler) putCliSettings(w http.ResponseWriter, r *http.Request) {
	spec, ok := requireKnownCLI(w, r.PathValue("cli"))
	if !ok {
		return
	}
	eb, ok := h.codingCliScope(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	name := r.PathValue("name")
	caller := authpkg.CallerFromContext(r.Context())
	if caller != nil && caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusForbidden, "team leaders have read-only access to worker coding CLIs")
		return
	}
	body, ok := readCodingCliBody(w, r)
	if !ok {
		return
	}
	var incoming map[string]interface{}
	if err := json.Unmarshal(body, &incoming); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be a JSON object: "+err.Error())
		return
	}

	// Read current (may be absent) first: the "***" sentinel needs it to
	// verify there is an existing value to keep.
	out, _, exit, err := eb.Exec(r.Context(), name, []string{"sh", "-c", "cat " + spec.SettingsPath}, codingCliExecTimeout)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker exec failed: "+err.Error())
		return
	}
	current := map[string]interface{}{}
	if exit == 0 && strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &current); err != nil {
			httputil.WriteError(w, http.StatusConflict, "existing settings file is not valid JSON; fix or delete it first")
			return
		}
	}
	// Sentinel contract (identical to the qwenpaw worker-side P1a
	// semantics): a "***" value keeps the existing secret. It must
	// reference an existing value; a sentinel with nothing to keep is
	// rejected rather than stored literally.
	if err := codingCliApplySentinels(current, incoming); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	merged := codingCliDeepMerge(current, incoming)
	home, err := h.workerHome(r.Context(), eb, name)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "resolve worker home: "+err.Error())
		return
	}
	mergedJSON, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "encode settings: "+err.Error())
		return
	}
	if err := eb.WriteFile(r.Context(), name, strings.ReplaceAll(spec.SettingsPath, "~", home), string(mergedJSON)); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "write settings into worker: "+err.Error())
		return
	}
	h.auditCodingCliWrite(r.Context(), name, "/coding-cli/"+spec.ID+"/settings", caller)
	log.FromContext(r.Context()).Info("worker coding-cli settings updated",
		"worker", name, "cli", spec.ID, "actor", authzActor(caller))
	writeCodingCliJSON(w, http.StatusOK, redactCodingCliSecrets(map[string]interface{}{
		"cli": spec.ID, "path": spec.SettingsPath, "exists": true, "settings": merged,
	}))
}

// workerHome resolves the worker's $HOME via the container shell (the
// settings paths are ~-relative and must be absolute for WriteFile).
func (h *CodingCliHandler) workerHome(ctx context.Context, eb codingCliExecer, name string) (string, error) {
	out, _, exit, err := eb.Exec(ctx, name, []string{"sh", "-c", "printf %s \"$HOME\""}, 5*time.Second)
	if err != nil {
		return "", err
	}
	if exit != 0 {
		return "", fmt.Errorf("exec exit %d", exit)
	}
	home := strings.TrimSpace(out)
	if !strings.HasPrefix(home, "/") {
		return "", fmt.Errorf("worker HOME is not an absolute path")
	}
	return home, nil
}

// --- install -------------------------------------------------------------

func (h *CodingCliHandler) startInstall(w http.ResponseWriter, r *http.Request) {
	spec, ok := requireKnownCLI(w, r.PathValue("cli"))
	if !ok {
		return
	}
	eb, ok := h.codingCliScope(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	name := r.PathValue("name")
	caller := authpkg.CallerFromContext(r.Context())
	if caller != nil && caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusForbidden, "team leaders have read-only access to worker coding CLIs")
		return
	}
	var req struct {
		Action  string `json:"action"`
		Version string `json:"version"`
	}
	body, ok := readCodingCliBody(w, r)
	if !ok {
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be JSON: "+err.Error())
		return
	}
	switch req.Action {
	case "install":
		if req.Version == "" {
			req.Version = "latest"
		}
		if !npmVersionPattern.MatchString(req.Version) {
			httputil.WriteError(w, http.StatusBadRequest, "invalid npm version tag: "+req.Version)
			return
		}
	case "uninstall":
	default:
		httputil.WriteError(w, http.StatusBadRequest, "action must be \"install\" or \"uninstall\"")
		return
	}

	// Busy gate: one install per CLI (npm global lock anyway).
	h.tasksMu.Lock()
	for _, t := range h.tasks {
		if t.state == "running" && t.worker == name && t.cli == spec.ID {
			h.tasksMu.Unlock()
			httputil.WriteError(w, http.StatusConflict, "install already running for "+spec.ID)
			return
		}
	}
	taskID := newCodingCliTaskID()
	h.tasks[taskID] = &codingCliInstallTask{
		worker: name, cli: spec.ID, action: req.Action, version: req.Version, state: "running", pkg: spec.NpmPackage,
	}
	h.tasksMu.Unlock()

	h.auditCodingCliWrite(r.Context(), name, "/coding-cli/"+spec.ID+"/install", caller)
	log.FromContext(r.Context()).Info("worker coding-cli install started",
		"worker", name, "cli", spec.ID, "action", req.Action, "version", req.Version, "task", taskID, "actor", authzActor(caller))

	go h.runInstall(name, eb, spec, taskID, req.Action, req.Version)
	writeCodingCliJSON(w, http.StatusAccepted, map[string]interface{}{
		"task_id": taskID, "cli": spec.ID, "action": req.Action, "version": req.Version, "state": "running",
	})
}

// npmVersionPattern accepts npm dist-tags (latest, beta...) and semver-like
// versions, rejecting shell metacharacters (the tag only ever reaches a
// string argument to npm, but belt-and-braces).
var npmVersionPattern = regexp.MustCompile(`^([0-9][A-Za-z0-9.+-]*|\w+)$`)

// runInstall installs/uninstalls the CLI inside the worker container via
// `npm install/uninstall -g`. Persistence contract (joint-scenario note,
// verified on the ARM worker image): the global npm payload lands in the
// container's EPHEMERAL layer (the image layer at $npm prefix, e.g.
// /usr/local/lib/node_modules) — NOT in the persistent agent home. A worker
// recreation (image upgrade, a spec change such as an LLM-runtime parameter,
// or any controller-driven container rebuild) wipes that layer, so the CLI
// binary is lost even though the container "comes back". Operators must re-run
// this install after a recreation; the settings surface is written
// container-local too (no forced MinIO push), so auth may also need re-applying
// — probe settings after a recreation and re-apply if the probe shows them gone.
func (h *CodingCliHandler) runInstall(name string, eb codingCliExecer, spec codingCliSpec, taskID, action, version string) {
	var cmd []string
	if action == "uninstall" {
		cmd = []string{"npm", "uninstall", "-g", spec.NpmPackage}
	} else {
		cmd = []string{"npm", "install", "-g", spec.NpmPackage + "@" + version}
	}
	_, stderr, exit, err := eb.Exec(context.Background(), name, cmd, codingCliInstallTimeout)
	h.tasksMu.Lock()
	defer h.tasksMu.Unlock()
	t := h.tasks[taskID]
	if t == nil {
		return
	}
	t.finishedAt = time.Now()
	if err != nil {
		t.state = "failed"
		t.stderr = truncateForMessage(err.Error())
		return
	}
	if exit != 0 {
		t.state = "failed"
		t.exitCode = exit
		t.stderr = truncateForMessage(stderr)
		return
	}
	t.state = "done"
	t.exitCode = 0
}

func (h *CodingCliHandler) getInstallStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireKnownCLI(w, r.PathValue("cli")); !ok {
		return
	}
	// Scope check still applies (name/worker must resolve).
	if _, ok := h.codingCliScope(w, r, r.PathValue("name")); !ok {
		return
	}
	name := r.PathValue("name")
	taskID := r.PathValue("task_id")
	if !codingCliTaskIDPattern.MatchString(taskID) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid install task id")
		return
	}
	h.tasksMu.Lock()
	h.pruneFinishedLocked()
	t, ok := h.tasks[taskID]
	// The task map is global, but a task belongs to exactly one worker. A
	// caller scoped to worker A must not read a task created for worker B —
	// treat a cross-worker id as not-found (no existence oracle).
	if ok && t.worker != name {
		ok = false
	}
	var snapshot map[string]interface{}
	if ok {
		snapshot = map[string]interface{}{
			"task_id": taskID, "cli": t.cli, "action": t.action, "version": t.version,
			"state": t.state, "exit_code": t.exitCode,
		}
		if t.stderr != "" {
			snapshot["stderr_tail"] = t.stderr
		}
	}
	h.tasksMu.Unlock()
	if !ok {
		httputil.WriteError(w, http.StatusNotFound, "install task not found")
		return
	}
	writeCodingCliJSON(w, http.StatusOK, snapshot)
}

// pruneFinishedLocked drops finished tasks older than codingCliTaskTTL.
// Caller holds h.tasksMu.
func (h *CodingCliHandler) pruneFinishedLocked() {
	now := time.Now()
	if now.Sub(h.lastPrune) < time.Minute {
		return
	}
	h.lastPrune = now
	for id, t := range h.tasks {
		if t.state != "running" && now.Sub(t.finishedAt) > codingCliTaskTTL {
			delete(h.tasks, id)
		}
	}
}

// --- helpers -------------------------------------------------------------

func newCodingCliTaskID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

func writeCodingCliJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// codingCliApplySentinels resolves "***" placeholder values in incoming
// against current: the key is dropped from incoming (so the deep merge
// keeps the existing value). A sentinel with no existing value is an
// error — the same contract as the qwenpaw worker-side P1a merge.
func codingCliApplySentinels(current, incoming map[string]interface{}) error {
	return codingCliSentinelsAt(current, incoming, "")
}

func codingCliSentinelsAt(current, incoming map[string]interface{}, prefix string) error {
	for k, v := range incoming {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		switch vv := v.(type) {
		case string:
			if vv == "***" {
				cur := codingCliDotGet(current, path)
				if cur == nil {
					return fmt.Errorf("sentinel \"***\" at %q has no existing value to keep", path)
				}
				if s, isStr := cur.(string); isStr && s == "" {
					return fmt.Errorf("sentinel \"***\" at %q has no existing value to keep", path)
				}
				// Drop so the deep merge keeps the existing value.
				delete(incoming, k)
			}
		case map[string]interface{}:
			if err := codingCliSentinelsAt(current, vv, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// codingCliDeepMerge merges incoming into current (incoming wins), the
// same shallow-per-level semantics as the qwenpaw worker-side merge.
func codingCliDeepMerge(current, incoming map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range current {
		out[k] = v
	}
	for k, v := range incoming {
		if incomingSub, isMap := v.(map[string]interface{}); isMap {
			if currentSub, isMap2 := out[k].(map[string]interface{}); isMap2 {
				out[k] = codingCliDeepMerge(currentSub, incomingSub)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// readCodingCliBody reads a bounded JSON body and returns a normalized
// copy (valid JSON or {} for an empty body).
func readCodingCliBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return []byte("{}"), true
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, codingCliBodyCap))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return nil, false
	}
	var v interface{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "request body must be JSON: "+err.Error())
			return nil, false
		}
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return raw, true
	}
	return normalized, true
}

// redactCodingCliSecrets walks the JSON document and replaces any
// credential field that holds a raw (non-empty) string with the
// redaction object {"redacted": true, "has_value": true}. Only
// credential-shaped keys are touched so legitimate string config values
// survive. Defense in depth: the controller is the sole redaction layer
// for this surface.
func redactCodingCliSecrets(v interface{}) interface{} {
	return redactCodingCliNode(v)
}

func redactCodingCliNode(node interface{}) interface{} {
	switch n := node.(type) {
	case map[string]interface{}:
		for k, v := range n {
			if isCodingCliCredentialKey(k) {
				if s, isStr := v.(string); isStr && s != "" {
					n[k] = map[string]interface{}{"redacted": true, "has_value": true}
					continue
				}
			}
			n[k] = redactCodingCliNode(v)
		}
		return n
	case []interface{}:
		for i, v := range n {
			n[i] = redactCodingCliNode(v)
		}
		return n
	default:
		return node
	}
}

// isCodingCliCredentialKey reports whether a JSON key holds secret
// material in coding-CLI settings. Mirrors the worker-side
// CREDENTIAL_KEY_PATTERNS so the two redaction layers stay in sync.
func isCodingCliCredentialKey(key string) bool {
	k := strings.ToLower(key)
	for _, p := range []string{"api_key", "apikey", "api-key", "token", "secret", "password", "access_key"} {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

func (h *CodingCliHandler) auditCodingCliWrite(ctx context.Context, worker, path string, caller *authpkg.CallerIdentity) {
	if h.audit == nil || caller == nil {
		return
	}
	action := "coding_cli_write"
	if strings.Contains(path, "/install") {
		action = "coding_cli_install"
	}
	h.audit.Record(ctx, audit.Event{
		Who:    caller.Username,
		Role:   caller.Role,
		Target: worker + path,
		Action: action,
	})
}
