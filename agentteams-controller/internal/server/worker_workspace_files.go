package server

// Worker knowledge base file inspection and management
// (/api/v1/workers/{name}/workspace-files/...).
//
// Each worker's qwenpaw app (QwenPaw >= 2.1) exposes workspace file
// endpoints on :8088 (0.0.0.0 listen; no auth in worker context because no
// console user is registered): /api/workspace/tree (paginated directory
// listing), /api/workspace/file-metadata, /api/workspace/file-content (bounded
// UTF-8 chunk reads and ETag-guarded writes), and /api/workspace/file-download
// (bounded stream). The Controller proxies those four subpaths so L2 humans
// and the workbench plugin can inspect — and, where the Human CR grants it,
// update — a worker's knowledge base (MEMORY.md, memory/**, digest/**)
// without reaching into the docker network directly.
//
// Embedded mode only: the worker app is reachable by container name inside
// the shared docker network. The effective container name prefix comes from
// configuration (AGENTTEAMS_PROXY_CONTAINER_PREFIX, or derived from
// AGENTTEAMS_RESOURCE_PREFIX when auto-prefixing is enabled; empty when
// auto-prefixing is disabled), and the port is the effective console port
// resolved through the same system-wins env chain used at container
// creation (service.EffectiveWorkerConsolePort — a conflicting spec.env
// value is discarded, so the container always listens on 8088). In kube
// mode there is no stable in-cluster DNS name for the worker pod, so the
// endpoints return 503.
//
// Two independent path boundaries:
//
//   - The upstream app hardens path resolution (no absolute paths, no
//     ".." segments, no NUL bytes, no symlink escape outside the workspace
//     root) and hides dot entries in directory listings — but a directly
//     requested path still resolves, so dot directories (.copaw/agent.json
//     carries the worker's Matrix token and MinIO credentials) remain
//     reachable upstream.
//
//   - This handler therefore enforces its own allowlist on top: only
//     MEMORY.md, memory/** and digest/** are addressable. The allowlist is
//     a prefix allowlist on exact root names (memory, digest) plus the
//     single top-level file MEMORY.md — never a denylist — so any other
//     workspace content (SOUL.md, PROFILE.md, TODO.md, .copaw/, .qwenpaw/,
//     checkpoints/, skills/, ...) is rejected before the request reaches
//     the worker.
//
// The upstream root=workspace parameter (the agent's own storage root, as
// opposed to root=project, the primary bound project directory) is pinned
// server-side and is never part of the client-facing query surface.
//
// Fixed-path forwarding only (tree / file-metadata / file-content GET+PUT /
// file-download, plus their whitelisted queries) — never a generic reverse
// proxy — so the attack surface is limited to four QwenPaw endpoints.
//
// Write scope (PUT file-content, introduced with the team-scoped write
// access): admin/manager (L1) may write any worker's knowledge base; an L2
// human may write only workers in their own teams, and only when their
// Human CR carries workspaceFileAccess="readwrite" — write is an explicit
// opt-in. An empty/missing value means "read" (never "readwrite"), so a
// controller upgrade cannot silently grant pre-existing L2 humans the new
// ability to modify worker knowledge files; L1 opts a user into writing by
// setting the field to "readwrite". Team leaders stay read-only on this
// API. The proxy enforces optimistic concurrency for
// existing files (If-Match is mandatory — the worker auto-appends to its
// memory files, so a skipped ETag check is a lost update) and caps the
// write body at 1 MiB, mirroring the read chunk cap. Every write is
// audit-logged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// workspaceFilesProxyTimeout bounds each upstream call.
	workspaceFilesProxyTimeout = 5 * time.Second

	// kbPathMaxSegments bounds the depth of addressable knowledge paths
	// (memory/2026-08-31/topic.md is three segments; four leaves margin
	// for one more nesting level without opening the full workspace tree).
	kbPathMaxSegments = 4

	// kbSegmentMaxBytes matches the upstream per-segment limit
	// (QwenPaw workspace_files._validate_segment).
	kbSegmentMaxBytes = 255

	// kbMaxPageLimit mirrors the upstream tree pagination cap (QwenPaw
	// workspace_files.MAX_PAGE_SIZE).
	kbMaxPageLimit = 500

	// kbMaxFileLimit mirrors the upstream file-content chunk cap (QwenPaw
	// workspace_files.MAX_CHUNK_SIZE).
	kbMaxFileLimit = 1024 * 1024
)

// workspaceFileSubpaths is the fixed whitelist of forwardable QwenPaw
// endpoints. file-content is served for both GET (chunked read) and PUT
// (ETag-guarded write); everything else the upstream app exposes
// (file-upload multipart, running-config, ...) stays deliberately absent.
var workspaceFileSubpaths = map[string]bool{
	"tree":          true,
	"file-metadata": true,
	"file-content":  true,
	"file-download": true,
}

// The write subpath is not a map-checked path value: the PUT route is the
// fixed literal /api/v1/workers/{name}/workspace-files/file-content
// registered in http.go (no {sub} capture), so the route table is the single
// source of truth and proxyWorkspaceFileWrite reads no sub path value.
//
// kbFileRoots are the top-level single files addressable by the
// file-metadata / file-content subpaths.
var kbFileRoots = []string{"MEMORY.md"}

// kbDirRoots are the knowledge base directories addressable by all three
// subpaths (tree on the directory or any of its subpaths; the file
// subpaths on files below it). Matching is on the exact first segment, so
// memories/ and memoryX/ are not prefixes of memory/.
var kbDirRoots = []string{"memory", "digest"}

// validateKbPath enforces the knowledge base allowlist (see the package
// comment). forFile selects the file subpaths (file-metadata /
// file-content); the tree subpath takes directory paths.
func validateKbPath(path string, forFile bool) error {
	if path == "" {
		return errors.New("path is required (memory/ or digest/)")
	}
	if strings.ContainsRune(path, '\\') || strings.ContainsRune(path, 0) || strings.HasPrefix(path, "/") {
		return errors.New("path must be a relative POSIX path")
	}
	segments := strings.Split(path, "/")
	if len(segments) > kbPathMaxSegments {
		return errors.New("path is too deep")
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("path contains an invalid segment")
		}
		if strings.HasPrefix(seg, ".") {
			return errors.New("hidden paths are not accessible")
		}
		if len(seg) > kbSegmentMaxBytes {
			return errors.New("path segment is too long")
		}
	}
	first := segments[0]
	if forFile {
		for _, root := range kbFileRoots {
			if first == root {
				// File roots are single top-level files (MEMORY.md) —
				// exactly one segment. Rejecting deeper paths keeps the
				// allowlist in line with the documented contract (MEMORY.md
				// is a file, not a directory); nested files must live under
				// the memory/ or digest/ directory roots.
				if len(segments) != 1 {
					return errors.New("file roots are single top-level files (e.g. MEMORY.md); use memory/ or digest/ for nested paths")
				}
				return nil
			}
		}
	}
	for _, root := range kbDirRoots {
		if first == root {
			return nil
		}
	}
	return errors.New("path is not in the knowledge base allowlist")
}

// validateWorkspaceFilesQuery enforces the strict per-subpath query
// whitelist and returns the upstream query string with root=workspace
// pinned. Unknown or duplicate parameters are rejected rather than
// silently dropped so client mistakes surface immediately (the same
// semantics the checkpoint proxy enforces).
func validateWorkspaceFilesQuery(sub string, q url.Values) (string, error) {
	var allowed map[string]bool
	switch sub {
	case "tree":
		allowed = map[string]bool{"path": true, "cursor": true, "limit": true}
	case "file-metadata":
		allowed = map[string]bool{"path": true}
	case "file-content":
		allowed = map[string]bool{"path": true, "offset": true, "limit": true}
	case "file-download":
		allowed = map[string]bool{"path": true}
	}
	for key, vals := range q {
		if !allowed[key] {
			return "", fmt.Errorf("unsupported query parameter: %s", key)
		}
		if len(vals) > 1 {
			return "", fmt.Errorf("duplicate query parameter: %s", key)
		}
	}
	if err := validateKbPath(q.Get("path"), sub != "tree"); err != nil {
		return "", err
	}
	if sub == "tree" || sub == "file-content" {
		if raw := q.Get("limit"); raw != "" {
			maxLimit := kbMaxFileLimit
			if sub == "tree" {
				maxLimit = kbMaxPageLimit
			}
			limit, err := strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > maxLimit {
				return "", fmt.Errorf("limit must be an integer between 1 and %d", maxLimit)
			}
		}
	}
	if sub == "file-content" {
		if raw := q.Get("offset"); raw != "" {
			offset, err := strconv.Atoi(raw)
			if err != nil || offset < 0 {
				return "", errors.New("offset must be a non-negative integer")
			}
		}
	}
	// Rebuild the upstream query from the validated values only, in a
	// fixed order, and pin the root. The client's raw query string is
	// never forwarded verbatim.
	up := url.Values{}
	up.Set("path", q.Get("path"))
	if raw := q.Get("cursor"); raw != "" {
		up.Set("cursor", raw)
	}
	if raw := q.Get("limit"); raw != "" {
		up.Set("limit", raw)
	}
	if raw := q.Get("offset"); raw != "" {
		up.Set("offset", raw)
	}
	up.Set("root", "workspace")
	return up.Encode(), nil
}

// WorkspaceFilesHandler proxies worker knowledge base read endpoints.
type WorkspaceFilesHandler struct {
	client    client.Client
	namespace string
	kubeMode  string
	http      *http.Client
	// containerPrefix is the effective worker container name prefix — the
	// same value the docker backend uses for container naming (derived from
	// AGENTTEAMS_PROXY_CONTAINER_PREFIX / AGENTTEAMS_RESOURCE_PREFIX /
	// auto-prefix; empty when auto-prefixing is disabled).
	containerPrefix string
	// workerBaseURL resolves a worker name to its qwenpaw app base URL from
	// the effective prefix and the worker's env. Injectable for tests.
	workerBaseURL func(name string, env map[string]string) string
}

// NewWorkspaceFilesHandler creates the handler with the default
// embedded-mode worker address resolution. containerPrefix must be the
// effective prefix from controller configuration (see
// config.ContainerPrefix).
func NewWorkspaceFilesHandler(c client.Client, namespace, kubeMode, containerPrefix string) *WorkspaceFilesHandler {
	h := &WorkspaceFilesHandler{
		client:          c,
		namespace:       namespace,
		kubeMode:        kubeMode,
		http:            &http.Client{Timeout: workspaceFilesProxyTimeout},
		containerPrefix: containerPrefix,
	}
	h.workerBaseURL = h.defaultWorkerBaseURL
	return h
}

// defaultWorkerBaseURL resolves a worker's qwenpaw app base URL from the
// effective container prefix and the effective console port. The port goes
// through service.EffectiveWorkerConsolePort — the same system-wins env
// chain used at container creation — so the proxy can never target a port
// the container does not listen on (a conflicting spec.env value is
// discarded before the container is created, so the raw spec.env must not
// be read here).
func (h *WorkspaceFilesHandler) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", h.containerPrefix, name, port)
}

// proxyWorkspaceFiles handles GET /api/v1/workers/{name}/workspace-files/{sub}.
// Scoped callers (team leaders / L2 humans) may only inspect workers in the
// teams they control — mirrors GET /api/v1/workers/{name} and the
// checkpoint proxy (W8: 404, not 403, so worker existence cannot be
// probed).
func (h *WorkspaceFilesHandler) proxyWorkspaceFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sub := r.PathValue("sub")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	if !workspaceFileSubpaths[sub] {
		httputil.WriteError(w, http.StatusBadRequest, "unsupported workspace file subpath")
		return
	}
	// Kube-mode check runs before any worker lookup: the endpoints are
	// entirely unavailable in kube mode, and a uniform 503 (rather than a
	// per-worker 404 vs 503 split) avoids leaking worker existence.
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker workspace file inspection requires embedded mode")
		return
	}

	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "get worker workspace files", err)
		return
	}
	// Resolve the owning team for the scoped-caller check (same chain as
	// ResourceHandler.GetWorker and the checkpoint proxy: standalone
	// workers hide as 404 for scoped callers).
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker workspace files", err)
		return
	}
	// Note: findTeamMember's second return value is the member (worker)
	// name, not the team name — the scoped check must compare against the
	// Team CR name (see ResourceHandler.GetWorker).
	teamName := ""
	if teamObj != nil {
		teamName = teamObj.Name
	}
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
		!caller.TeamMatches(teamName) {
		httputil.WriteError(w, http.StatusNotFound, "worker not found")
		return
	}

	// runtime-aware: workspace file inspection is qwenpaw-specific. This
	// runs after the team-scope check above so an out-of-scope caller sees
	// 404 (existence hidden) regardless of the worker's runtime.
	if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
		httputil.WriteError(w, http.StatusBadRequest, "workspace files are only supported for qwenpaw workers")
		return
	}

	query, err := validateWorkspaceFilesQuery(sub, r.URL.Query())
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	target := h.workerBaseURL(name, worker.Spec.Env) + "/api/workspace/" + sub
	if query != "" {
		target += "?" + query
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build workspace files request: "+err.Error())
		return
	}
	resp, err := h.http.Do(req)
	if err != nil {
		// Connection refused (worker stopped), DNS failure, timeout.
		httputil.WriteError(w, http.StatusBadGateway, "worker workspace API unreachable")
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if sub == "file-download" {
			// Stream the file verbatim with its attachment headers —
			// the client saves it under the worker's file name.
			for _, h := range []string{"Content-Disposition", "Content-Length", "ETag", "Accept-Ranges", "Content-Type"} {
				if v := resp.Header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusRequestedRangeNotSatisfiable:
		// Pass through verbatim: invalid cursor/offset (400), file not
		// found — which is also the pre-2.1 router-missing signal, see the
		// documentation's MEMORY.md probe heuristic (404), file changed
		// while being read (409), or offset beyond end of file (416).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("workspace files API error (status %d): %s", resp.StatusCode, string(body)))
	}
}

// upstreamKBFileExists probes the worker's file-metadata endpoint to decide
// the If-Match policy before a write. exists=true means the file is present
// (a write must carry a matching If-Match); exists=false means the file is
// absent (a write must NOT carry If-Match — upstream treats an ETag on a
// missing file as a conflict).
func (h *WorkspaceFilesHandler) upstreamKBFileExists(ctx context.Context, baseURL, path string) (bool, error) {
	target := baseURL + "/api/workspace/file-metadata?path=" + url.QueryEscape(path) + "&root=workspace"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("file-metadata probe returned status %d", resp.StatusCode)
	}
}

// proxyWorkspaceFileWrite handles PUT
// /api/v1/workers/{name}/workspace-files/file-content.
//
// Write scope: admin/manager (L1) may write any worker's knowledge base; an
// L2 human may write only workers in their own teams while their Human CR
// carries workspaceFileAccess="readwrite" (explicit opt-in — an
// empty/missing value means "read"). Team leaders stay read-only on this
// API. Out-of-scope workers hide as 404 (same W8 anti-probing rule as the
// read path).
//
// Concurrency: the proxy first probes file-metadata; for an existing file
// the If-Match header is mandatory (the worker auto-appends to its memory
// files, so a write without an ETag check is a lost update), and for a new
// file it must be absent (upstream rejects an ETag on a missing file). The
// write body is capped at kbMaxFileLimit (1 MiB, the read chunk cap).
func (h *WorkspaceFilesHandler) proxyWorkspaceFileWrite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	// The registered route is the fixed literal
	// /api/v1/workers/{name}/workspace-files/file-content (see http.go) — the
	// PUT route has no {sub} capture, so no sub path value is read here.
	// (Review P1: the previous code read r.PathValue("sub"), which was always
	// "" on the registered route, so every authorized write was rejected with
	// 400 "unsupported workspace file write subpath" before the worker lookup.)
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker workspace file inspection requires embedded mode")
		return
	}

	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "write worker workspace file", err)
		return
	}
	// Same team-scope chain as the read path: findTeamMember's second return
	// value is the member name, not the team name — the scope check must
	// compare against the Team CR name.
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "write worker workspace file", err)
		return
	}
	teamName := ""
	if teamObj != nil {
		teamName = teamObj.Name
	}
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, http.StatusForbidden, "caller identity required")
		return
	}
	if caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman {
		if !caller.TeamMatches(teamName) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
	}
	// runtime-aware: workspace file inspection is qwenpaw-specific. This
	// runs after the team-scope check above so an out-of-scope caller sees
	// 404 (existence hidden) regardless of the worker's runtime.
	if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
		httputil.WriteError(w, http.StatusBadRequest, "workspace files are only supported for qwenpaw workers")
		return
	}

	// Write-role boundary (see the function comment).
	switch caller.Role {
	case authpkg.RoleAdmin, authpkg.RoleManager:
		// full access
	case authpkg.RoleTeamLeader:
		httputil.WriteError(w, http.StatusForbidden, "team leaders can read workspace files but not write them")
		return
	case authpkg.RoleHuman:
		var human v1beta1.Human
		if err := h.client.Get(r.Context(), client.ObjectKey{Name: caller.Username, Namespace: h.namespace}, &human); err != nil {
			httputil.WriteError(w, http.StatusForbidden, "workspace file access cannot be verified for this user")
			return
		}
		// Write is an explicit opt-in: an empty or missing
		// workspaceFileAccess means "read". Defaulting to readwrite would
		// silently grant every pre-existing L2 human a new ability to
		// modify worker knowledge files on controller upgrade.
		if !strings.EqualFold(human.Spec.WorkspaceFileAccess, "readwrite") {
			httputil.WriteError(w, http.StatusForbidden, "workspace file write access requires workspaceFileAccess=readwrite (explicit opt-in)")
			return
		}
	default:
		httputil.WriteError(w, http.StatusForbidden, "workspace file write denied for this caller role")
		return
	}

	q := r.URL.Query()
	if len(q) != 1 || len(q["path"]) != 1 || q.Get("path") == "" {
		httputil.WriteError(w, http.StatusBadRequest, "the only supported query parameter is path (exactly once)")
		return
	}
	path := q.Get("path")
	if err := validateKbPath(path, true); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Body: {"content": string}, capped at the 1 MiB write limit.
	raw, err := io.ReadAll(io.LimitReader(r.Body, kbMaxFileLimit+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	if len(raw) > kbMaxFileLimit {
		httputil.WriteError(w, http.StatusBadRequest, "content exceeds the 1 MiB write limit")
		return
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be a JSON object with a content string field")
		return
	}
	if payload.Content == "" {
		// An empty write would truncate the worker's knowledge file —
		// the write API is for updating knowledge, not blanking it.
		httputil.WriteError(w, http.StatusBadRequest, "content must be a non-empty string")
		return
	}

	// If-Match policy: probe existence first (the worker app is the source
	// of truth for what is on disk).
	baseURL := h.workerBaseURL(name, worker.Spec.Env)
	ifMatch := r.Header.Get("If-Match")
	exists, err := h.upstreamKBFileExists(r.Context(), baseURL, path)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "probe worker workspace file: "+err.Error())
		return
	}
	if exists && ifMatch == "" {
		httputil.WriteError(w, http.StatusBadRequest, "If-Match header is required to update an existing file")
		return
	}
	if !exists && ifMatch != "" {
		httputil.WriteError(w, http.StatusBadRequest, "If-Match must not be sent when creating a new file")
		return
	}

	target := baseURL + "/api/workspace/file-content?path=" + url.QueryEscape(path) + "&root=workspace"
	body, _ := json.Marshal(map[string]string{"content": payload.Content})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, target, strings.NewReader(string(body)))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build workspace file write request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker workspace API unreachable")
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		bodyOut, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bodyOut)
		if logger := log.FromContext(r.Context()).WithName("workspace-files"); logger.Enabled() {
			logger.Info("knowledge base file written",
				"worker", name, "path", path, "caller", caller.Username,
				"role", caller.Role, "bytes", len(payload.Content), "create", !exists)
		}
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		// Pass through verbatim: invalid path (400), file vanished between
		// probe and write (404), ETag mismatch — file changed on disk (409),
		// or content rejected upstream (422).
		bodyOut, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(bodyOut)
	default:
		bodyOut, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("workspace files API error (status %d): %s", resp.StatusCode, string(bodyOut)))
	}
}
