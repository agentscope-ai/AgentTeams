// Package server — worker coding-CLI management surface (P2, controller-side).
//
// The controller manages each worker's coding CLIs (qwen-code, opencode, qoder)
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
	"path"
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
	// AuthEnvVars are container env vars whose non-empty value counts as
	// configured auth (reported with method "env"). Qoder's headless auth
	// is QODER_PERSONAL_ACCESS_TOKEN; env values are never echoed in the
	// probe response.
	AuthEnvVars []string
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
	{
		ID:           "qoder",
		NpmPackage:   "@qoder-ai/qodercli",
		BinName:      "qoder",
		SettingsPath: "~/.qoder/settings.json",
		// Qoder auth is not an OpenAI-style settings dot-path: headless/CI
		// auth is the QODER_PERSONAL_ACCESS_TOKEN env var, interactive
		// /login keeps an auto-refreshed token in its config dir (no
		// documented stable file name — not probed). Settings still
		// manages model/tools/mcp like the other CLIs.
		AuthEnvVars: []string{"QODER_PERSONAL_ACCESS_TOKEN"},
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
	// Protected by tasksMu; each entry covers the whole settings read/merge/write.
	settingsBusy map[string]bool
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
// resolution from the registry: the auto-detected worker backend (docker in
// embedded mode), matching how lifecycle_handler resolves its backend.
func NewCodingCliHandler(c client.Client, namespace, kubeMode string, ac *audit.Client, reg *backend.Registry) *CodingCliHandler {
	h := &CodingCliHandler{
		client:     c,
		namespace:  namespace,
		kubeMode:   kubeMode,
		audit:      ac,
		tasks:      map[string]*codingCliInstallTask{},
	}
	// The worker name passed at call time is NOT a backend name — the
	// backend is auto-detected. Treating the worker name as a backend name
	// (GetWorkerBackend(ctx, name)) makes every request fail with
	// "unknown worker backend: <worker-name>".
	h.execBackend = func(ctx context.Context, _ string) (codingCliExecer, error) {
		if reg == nil {
			return nil, fmt.Errorf("no worker backend available")
		}
		b := reg.DetectWorkerBackend(ctx)
		if b == nil {
			return nil, fmt.Errorf("no worker backend available")
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
	bin := spec.BinName
	// Installs made through the management tab/API land in the persistent
	// agent home ($HOME/.coding-cli), not on PATH. Prefer that binary.
	if home, err := h.workerHome(ctx, eb, name); err == nil {
		candidate := path.Join(home, ".coding-cli", "bin", spec.BinName)
		if _, _, exit, _ := eb.Exec(ctx, name, []string{"test", "-x", candidate}, 5*time.Second); exit == 0 {
			bin = candidate
		}
	}
	// The CLI resolves its config from $HOME; run it with the same effective
	// home the settings API reads and writes. Containers without `env` (or
	// with a probe wrapper) fall back to the plain invocation.
	out, _, exit, err := eb.Exec(ctx, name, []string{bin, "--version"}, codingCliExecTimeout)
	if home, herr := h.workerHome(ctx, eb, name); herr == nil && home != "" {
		if envOut, _, envExit, envErr := eb.Exec(ctx, name, []string{"env", "HOME=" + home, bin, "--version"}, codingCliExecTimeout); envErr == nil && envExit == 0 {
			out, exit, err = envOut, envExit, nil
		}
	}
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
	home := h.settingsHome(ctx, eb, name)
	settingsPath := expandHomePath(spec.SettingsPath, home)
	legacyPath := ""
	if rawHome, rerr := h.shellHome(ctx, eb, name); rerr == nil && rawHome != "" && home != "" {
		if legacy := expandHomePath(spec.SettingsPath, rawHome); legacy != settingsPath {
			legacyPath = legacy
		}
	}
	// Auth env vars (qoder QODER_PERSONAL_ACCESS_TOKEN): a non-empty value
	// means configured. Qoder's own precedence rule is PAT-over-login, so
	// env is checked first. Only presence is reported, never the value.
	for _, ev := range spec.AuthEnvVars {
		if _, _, exit, err := eb.Exec(ctx, name, []string{"sh", "-c", "test -n \"$" + ev + "\""}, codingCliExecTimeout); err == nil && exit == 0 {
			return map[string]interface{}{"configured": true, "method": "env"}
		}
	}
	// Extra auth files (opencode auth.json): check existence.
	for _, f := range spec.ExtraAuthFiles {
		if _, _, exit, err := eb.Exec(ctx, name, shellTestNonEmpty(expandHomePath(f, home)), codingCliExecTimeout); err == nil && exit == 0 {
			return map[string]interface{}{"configured": true, "method": "auth-file"}
		}
	}
	// Settings credentials: Qwen's fixed path and OpenCode's provider API keys.
	probePath := settingsPath
	if _, _, exit, err := eb.Exec(ctx, name, shellTestFile(probePath), codingCliExecTimeout); err != nil || exit != 0 {
		// Compatibility: credentials that only exist in the legacy $HOME
		// location still count as configured.
		if legacyPath == "" {
			return map[string]interface{}{"configured": false}
		}
		probePath = legacyPath
	}
	if _, _, exit, err := eb.Exec(ctx, name, shellTestFile(probePath), codingCliExecTimeout); err == nil && exit == 0 {
		out, _, cexit, cerr := eb.Exec(ctx, name, shellCatFile(probePath), codingCliExecTimeout)
		if cerr == nil && cexit == 0 {
			var doc map[string]interface{}
			if json.Unmarshal([]byte(out), &doc) == nil {
				if spec.ID == "opencode" {
					providers, _ := doc["provider"].(map[string]interface{})
					for _, value := range providers {
						provider, _ := value.(map[string]interface{})
						if key, ok := codingCliDotGet(provider, "options.apiKey").(string); ok && key != "" {
							return map[string]interface{}{"configured": true, "method": "settings"}
						}
					}
				}
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

// readCliSettings returns the path used for this read so a subsequent write
// targets the same file. Qoder resolves its config directory inside the Worker;
// pass the resulting path as an argument so shell characters stay literal.
func (h *CodingCliHandler) readCliSettings(ctx context.Context, eb codingCliExecer, name string, spec codingCliSpec) (contents, filePath string, exitCode int, err error) {
	// Every settings read/write goes through the same effective home; the
	// raw $HOME is only consulted as a compatibility fallback so settings
	// written by older releases stay visible (and are migrated on the next
	// write).
	home := h.settingsHome(ctx, eb, name)
	filePath = expandHomePath(spec.SettingsPath, home)
	if spec.ID == "qoder" {
		qoderScript := `dir="${QODER_CONFIG_DIR:-$HOME/.qoder}"
case "$dir" in /*) ;; *) dir="$PWD/$dir" ;; esac
printf '%s/settings.json' "${dir%/}"`
		if home != "" {
			qoderScript = `dir="${QODER_CONFIG_DIR:-"$1"/.qoder}"
case "$dir" in /*) ;; *) dir="$PWD/$dir" ;; esac
printf '%s/settings.json' "${dir%/}"`
		}
		qoderArgs := []string{"sh", "-c", qoderScript}
		if home != "" {
			qoderArgs = append(qoderArgs, "sh", home)
		}
		filePath, _, exitCode, err = eb.Exec(ctx, name, qoderArgs, codingCliExecTimeout)
		if err != nil {
			return
		}
		if exitCode != 0 {
			err = fmt.Errorf("resolve Qoder settings path: exec exit %d", exitCode)
			return
		}
		if !strings.HasPrefix(filePath, "/") {
			err = fmt.Errorf("worker Qoder settings path is not absolute")
			return
		}
		contents, _, exitCode, err = eb.Exec(ctx, name, []string{"cat", "--", filePath}, codingCliExecTimeout)
		if err == nil && exitCode == 0 {
			return contents, filePath, 0, nil
		}
	} else {
		contents, _, exitCode, err = eb.Exec(ctx, name, shellCatFile(filePath), codingCliExecTimeout)
		if err == nil && exitCode == 0 {
			return contents, filePath, 0, nil
		}
	}
	// Compatibility: read the same spec path under the raw $HOME.
	rawHome, rerr := h.shellHome(ctx, eb, name)
	if rerr == nil && rawHome != "" {
		legacy := expandHomePath(spec.SettingsPath, rawHome)
		if legacy != filePath {
			if legacyContents, _, legacyExit, legacyErr := eb.Exec(ctx, name, shellCatFile(legacy), codingCliExecTimeout); legacyErr == nil && legacyExit == 0 {
				// The caller writes back to filePath, migrating the
				// legacy settings on the first update.
				return legacyContents, filePath, 0, nil
			}
		}
	}
	return contents, filePath, exitCode, err
}

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
	out, filePath, exit, err := h.readCliSettings(r.Context(), eb, name, spec)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker exec failed: "+err.Error())
		return
	}
	if exit != 0 {
		// Settings file absent — report an empty (not error) state so the
		// frontend can offer "create from defaults".
		writeCodingCliJSON(w, http.StatusOK, map[string]interface{}{
			"cli": spec.ID, "path": filePath, "exists": false, "settings": map[string]interface{}{},
		})
		return
	}
	var settings map[string]interface{}
	if err := json.Unmarshal([]byte(out), &settings); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker settings file is not valid JSON: "+err.Error())
		return
	}
	writeCodingCliJSON(w, http.StatusOK, redactCodingCliSecrets(map[string]interface{}{
		"cli": spec.ID, "path": filePath, "exists": true, "settings": settings,
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

	key := name + "/" + spec.ID
	h.tasksMu.Lock()
	if h.settingsBusy[key] {
		h.tasksMu.Unlock()
		httputil.WriteError(w, http.StatusConflict, "settings update already running; retry from the latest settings")
		return
	}
	if h.settingsBusy == nil {
		h.settingsBusy = make(map[string]bool)
	}
	h.settingsBusy[key] = true
	h.tasksMu.Unlock()
	defer func() {
		h.tasksMu.Lock()
		delete(h.settingsBusy, key)
		h.tasksMu.Unlock()
	}()

	// Read current (may be absent) first: the "***" sentinel needs it to
	// verify there is an existing value to keep.
	out, filePath, exit, err := h.readCliSettings(r.Context(), eb, name, spec)
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
	// readCliSettings already resolved the effective home, so this is the
	// exact path the next GET probes.
	writePath := filePath
	mergedJSON, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "encode settings: "+err.Error())
		return
	}
	if err := eb.WriteFile(r.Context(), name, writePath, string(mergedJSON)); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "write settings into worker: "+err.Error())
		return
	}
	h.auditCodingCliWrite(r.Context(), name, "/coding-cli/"+spec.ID+"/settings", caller)
	log.FromContext(r.Context()).Info("worker coding-cli settings updated",
		"worker", name, "cli", spec.ID, "actor", authzActor(caller))
	writeCodingCliJSON(w, http.StatusOK, redactCodingCliSecrets(map[string]interface{}{
		"cli": spec.ID, "path": filePath, "exists": true, "settings": merged,
	}))
}

// shellHome returns the container's raw $HOME (the pre-existing settings
// location). Used only for compatibility reads: settings written by older
// releases live there while the management API now writes to the effective
// home.
func (h *CodingCliHandler) shellHome(ctx context.Context, eb codingCliExecer, name string) (string, error) {
	out, _, exit, err := eb.Exec(ctx, name, []string{"sh", "-c", `printf %s "${HOME:-}"`}, 5*time.Second)
	if err != nil {
		return "", err
	}
	if exit != 0 {
		return "", fmt.Errorf("resolve worker $HOME: exec exit %d", exit)
	}
	return strings.TrimSpace(out), nil
}

// shellArgWithPath builds a `sh -c` invocation that receives the resolved
// path as a positional argument and quotes it inside the script, so spaces or
// shell metacharacters in a home directory never change the meaning of the
// command. (Interpolating the path into the script is what broke
// `/tmp/working dir`.)
func shellArgWithPath(script, path string) []string {
	return []string{"sh", "-c", script, "sh", path}
}

func shellTestFile(path string) []string     { return shellArgWithPath(`test -f "$1"`, path) }
func shellTestNonEmpty(path string) []string { return shellArgWithPath(`test -s "$1"`, path) }
func shellCatFile(path string) []string      { return shellArgWithPath(`cat -- "$1"`, path) }

// expandHomePath replaces the leading "~" of a spec path with home.
func expandHomePath(p, home string) string {
	if home == "" {
		return p
	}
	return strings.ReplaceAll(p, "~", home)
}

// settingsHome is the tolerant resolver used by the settings/auth paths: the
// effective home when the container can report one, the raw $HOME when it
// cannot, and "" when neither works (the caller then uses the spec path as
// written, which keeps older fixtures and non-shell execers working).
func (h *CodingCliHandler) settingsHome(ctx context.Context, eb codingCliExecer, name string) string {
	if home, err := h.workerHome(ctx, eb, name); err == nil && home != "" {
		return home
	}
	if home, err := h.shellHome(ctx, eb, name); err == nil {
		return home
	}
	return ""
}

// workerHome resolves the worker's $HOME via the container shell (the
// settings paths are ~-relative and must be absolute for WriteFile).
func (h *CodingCliHandler) workerHome(ctx context.Context, eb codingCliExecer, name string) (string, error) {
	// Effective home for coding-CLI data. QwenPaw only persists its three
	// mounted dirs (QWENPAW_WORKING_DIR=/app/working + .backups/.secret), so
	// settings/prefix must live under that dir, not $HOME. OpenClaw/Hermes
	// have no QWENPAW_WORKING_DIR; their $HOME is the MinIO-synced agent home.
	out, _, exit, err := eb.Exec(ctx, name, []string{"sh", "-c", `printf %s "${QWENPAW_WORKING_DIR:-$HOME}"`}, 5*time.Second)
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
// `npm install/uninstall -g --prefix $HOME/.coding-cli`. The prefix is in the
// persistent agent home, so a same-worker recreation keeps the CLI; settings
// and auth stay in the agent home as before. Nothing is baked into the worker
// image by default and no version is pinned: the tab/API passes a dist-tag
// (latest/beta) or an explicit version. Probe prefers the prefix binary.
func (h *CodingCliHandler) runInstall(name string, eb codingCliExecer, spec codingCliSpec, taskID, action, version string) {
	// Install into the persistent agent home (survives same-worker
	// recreation); no version pin — the tab/API passes a dist-tag
	// (latest/beta) or an explicit version.
	prefix := ""
	if home, err := h.workerHome(context.Background(), eb, name); err == nil {
		prefix = path.Join(home, ".coding-cli")
	}
	cmd := []string{"npm"}
	if action == "uninstall" {
		cmd = append(cmd, "uninstall", "-g")
		if prefix != "" {
			cmd = append(cmd, "--prefix", prefix)
		}
		cmd = append(cmd, spec.NpmPackage)
	} else {
		cmd = append(cmd, "install", "-g")
		if prefix != "" {
			cmd = append(cmd, "--prefix", prefix)
		}
		cmd = append(cmd, spec.NpmPackage+"@"+version)
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
				cur := current[k]
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
			child, _ := current[k].(map[string]interface{})
			if err := codingCliSentinelsAt(child, vv, path); err != nil {
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
// material in coding-CLI settings. Includes the worker-side
// CREDENTIAL_KEY_PATTERNS and authentication headers used by OpenCode.
func isCodingCliCredentialKey(key string) bool {
	k := strings.ToLower(key)
	switch k {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	}
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
