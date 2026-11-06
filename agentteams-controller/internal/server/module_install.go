package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	moduleArtifactTimeout = 60 * time.Second
	moduleArtifactMaxBody = 64 << 20 // 64 MiB
)

var moduleTokenRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validModuleToken(token string) bool {
	return moduleTokenRE.MatchString(token) && token != "." && token != ".."
}

// moduleInstallRequest is the install body shared by the three planes.
type moduleInstallRequest struct {
	Module   string `json:"module"`
	Source   string `json:"source"`
	Repo     string `json:"repo"`
	Ref      string `json:"ref"`
	Channel  string `json:"channel"`
	Version  string `json:"version"`
	Artifact string `json:"artifact"`
	SHA256   string `json:"sha256"`
}

// moduleInstaller owns the download/verify/upload pipeline for one request.
type moduleInstaller struct {
	rh              *ResourceHandler
	kubeMode        string
	containerPrefix string
	index           *MillforgeIndexHandler
	proxy           *workerPluginsProxy
	http            *http.Client
	workerBaseURL   func(name string, env map[string]string) string
	managerBaseURL  func(name string) string
}

func newModuleInstaller(rh *ResourceHandler, kubeMode, containerPrefix, resourcePrefix string) *moduleInstaller {
	wp := newWorkerPluginsProxy(rh, kubeMode, containerPrefix)
	return &moduleInstaller{
		rh:              rh,
		kubeMode:        kubeMode,
		containerPrefix: containerPrefix,
		index:           NewMillforgeIndexHandler(),
		proxy:           wp,
		http: &http.Client{
			Timeout: moduleArtifactTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 0 && req.URL.Host != via[0].URL.Host {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
		workerBaseURL: wp.defaultWorkerBaseURL,
		managerBaseURL: func(name string) string {
			return fmt.Sprintf("http://%s:18799", authpkg.ResourcePrefix(resourcePrefix).ManagerPodName(name))
		},
	}
}

// indexArtifact resolves the artifact checksum from the re-read index.
func indexArtifact(modules json.RawMessage, module, version, artifact string) (name, sha, pluginID, kind string, status int, msg string) {
	var mods map[string]json.RawMessage
	if err := json.Unmarshal(modules, &mods); err != nil {
		return "", "", "", "", http.StatusBadGateway, "millforge index: modules is not an object"
	}
	raw, ok := mods[module]
	if !ok {
		return "", "", "", "", http.StatusNotFound, "module not found in index"
	}
	var entry struct {
		Latest   string `json:"latest"`
		PluginID string `json:"pluginId"`
		Kind     string `json:"kind"`
		Versions map[string]struct {
			Artifacts []struct {
				Name   string `json:"name"`
				SHA256 string `json:"sha256"`
			} `json:"artifacts"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", "", "", "", http.StatusBadGateway, "millforge index: module entry malformed"
	}
	pluginID = entry.PluginID
	if pluginID == "" {
		pluginID = module
	}
	if !validModuleToken(pluginID) {
		return "", "", "", "", http.StatusBadGateway, "millforge index: invalid pluginId"
	}
	kind = entry.Kind
	if kind == "runner" {
		return "", "", "", "", http.StatusBadRequest, "runner modules are not installed as qwenpaw plugins"
	}
	if kind == "bundle" && pluginID == module {
		return "", "", "", "", http.StatusBadRequest, "bundle modules require an adapter artifact with a distinct pluginId"
	}
	if version == "" {
		version = entry.Latest
	}
	ver, ok := entry.Versions[version]
	if !ok {
		return "", "", "", "", http.StatusNotFound, "module version not found in index"
	}
	// Prefer the qwenpaw zip when the caller did not pin an artifact: a
	// release may also carry the runtime-neutral tar.gz and a stable-name
	// duplicate of the zip.
	selected := -1
	if artifact != "" {
		for i, a := range ver.Artifacts {
			if a.Name == artifact {
				selected = i
				break
			}
		}
	} else {
		for i, a := range ver.Artifacts {
			if strings.HasSuffix(a.Name, ".zip") {
				selected = i
				break
			}
		}
		if selected < 0 && len(ver.Artifacts) > 0 {
			selected = 0
		}
	}
	if selected < 0 {
		return "", "", "", "", http.StatusNotFound, "module artifact not found in index"
	}
	a := ver.Artifacts[selected]
	if a.SHA256 == "" {
		return "", "", "", "", http.StatusBadGateway, "millforge index: artifact checksum missing"
	}
	if !validModuleToken(version) || !validModuleToken(a.Name) {
		return "", "", "", "", http.StatusBadGateway, "millforge index: invalid version/artifact token"
	}
	if !strings.HasSuffix(a.Name, ".zip") {
		return "", "", "", "", http.StatusBadRequest, "install artifact must be a zip package"
	}
	return a.Name, a.SHA256, pluginID, kind, http.StatusOK, ""
}

// artifactURL composes the raw artifact URL for the selected source.
func artifactURL(source, repo, ref, module, version, name string) string {
	base := "https://raw.githubusercontent.com/" + repo + "/" + ref
	if source == "gitcode" {
		base = "https://raw.gitcode.com/" + repo + "/raw/" + ref
	}
	return base + "/releases/" + module + "/" + version + "/" + name
}

func (m *moduleInstaller) downloadArtifact(ctx context.Context, u, wantSHA string) ([]byte, int, string) {
	parsed, err := url.Parse(u)
	if err != nil || !millforgeIndexAllowedHosts[parsed.Host] {
		return nil, http.StatusBadGateway, "artifact host not allowed"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, http.StatusBadGateway, "artifact request failed"
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, "artifact download failed"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, http.StatusBadGateway, "artifact download failed"
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, moduleArtifactMaxBody+1))
	if err != nil {
		return nil, http.StatusBadGateway, "artifact read failed"
	}
	if len(body) > moduleArtifactMaxBody {
		return nil, http.StatusBadGateway, "artifact too large"
	}
	sum := sha256.Sum256(body)
	if !bytes.EqualFold([]byte(hex.EncodeToString(sum[:])), []byte(wantSHA)) {
		return nil, http.StatusBadGateway, "artifact checksum mismatch"
	}
	return body, http.StatusOK, ""
}

// uploadPlugin pushes the verified bytes to a qwenpaw app and hot-loads them.
func (m *moduleInstaller) uploadPlugin(ctx context.Context, base, filename, pluginID, version string, data []byte) (bool, int, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return false, http.StatusInternalServerError, "build upload form failed"
	}
	if _, err := part.Write(data); err != nil {
		return false, http.StatusInternalServerError, "build upload form failed"
	}
	if err := w.Close(); err != nil {
		return false, http.StatusInternalServerError, "build upload form failed"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/plugins/upload?force=true", &buf)
	if err != nil {
		return false, http.StatusInternalServerError, "build upload request failed"
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := m.http.Do(req)
	if err != nil {
		return false, http.StatusBadGateway, "qwenpaw plugin upload unreachable"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, http.StatusBadGateway, "qwenpaw plugin upload failed"
	}
	var out struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Loaded  bool   `json:"loaded"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, http.StatusBadGateway, "qwenpaw plugin upload returned an invalid response"
	}
	if !out.Loaded {
		return false, http.StatusBadGateway, "qwenpaw installed the plugin but did not load it"
	}
	if out.ID != pluginID || out.Version != version {
		return false, http.StatusBadGateway, "qwenpaw loaded a different plugin or version"
	}
	return true, http.StatusOK, ""
}

// Check the same manifest location QwenPaw loads, before any runtime code runs.
// A valid checksum binds bytes to the index, but not the package's identity.
func validateModuleArtifact(data []byte, pluginID, version string) error {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("module artifact is not a valid zip")
	}
	var roots, wrapped []*zip.File
	for _, f := range z.File {
		if f.Name == "plugin.json" {
			roots = append(roots, f)
		} else if parts := strings.Split(f.Name, "/"); len(parts) == 2 && parts[0] != "" && parts[1] == "plugin.json" {
			wrapped = append(wrapped, f)
		}
	}
	manifests := roots
	if len(manifests) == 0 {
		manifests = wrapped
	}
	if len(manifests) != 1 || manifests[0].UncompressedSize64 > 1<<20 {
		return fmt.Errorf("module artifact requires one bounded plugin manifest")
	}
	r, err := manifests[0].Open()
	if err != nil {
		return fmt.Errorf("module artifact manifest unreadable")
	}
	defer r.Close()
	body, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	var manifest struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err != nil || len(body) > 1<<20 || json.Unmarshal(body, &manifest) != nil {
		return fmt.Errorf("module artifact manifest invalid")
	}
	if manifest.ID != pluginID || manifest.Version != version {
		return fmt.Errorf("module artifact plugin identity/version does not match the index")
	}
	return nil
}

// previousVersion reads the target's installed version for the module.
func (m *moduleInstaller) previousVersion(ctx context.Context, base, module string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/plugins", nil)
	if err != nil {
		return ""
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var entries []map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&entries) != nil {
		return ""
	}
	for _, e := range entries {
		if id, _ := e["id"].(string); id == module {
			v, _ := e["version"].(string)
			return v
		}
	}
	return ""
}

// prepareArtifact validates the request, re-reads the index and returns the
// verified artifact bytes once per install operation (a team install
// downloads once and pushes the same bytes to every member).
func (m *moduleInstaller) prepareArtifact(ctx context.Context, module string, body moduleInstallRequest) (version, name, pluginID string, data []byte, status int, msg string) {
	if body.Source != "github" && body.Source != "gitcode" {
		return "", "", "", nil, http.StatusBadRequest, "source must be github or gitcode"
	}
	repo := body.Repo
	if repo == "" {
		repo = millforgeIndexDefaultRepo
	}
	if !millforgeRepoRE.MatchString(repo) {
		return "", "", "", nil, http.StatusBadRequest, "invalid repo"
	}
	ref := body.Ref
	if ref == "" {
		ref = millforgeIndexDefaultRef
	}
	if !millforgeRefRE.MatchString(ref) {
		return "", "", "", nil, http.StatusBadRequest, "invalid ref"
	}
	channel := body.Channel
	if channel == "" {
		channel = millforgeIndexDefaultChannel
	}
	if !validModuleToken(module) {
		return "", "", "", nil, http.StatusBadRequest, "invalid module token"
	}
	for _, token := range []string{body.Version, body.Artifact} {
		if token != "" && !validModuleToken(token) {
			return "", "", "", nil, http.StatusBadRequest, "invalid version/artifact token"
		}
	}

	doc, status, msg := m.index.fetchIndex(ctx, body.Source, repo, ref, channel)
	if doc == nil {
		return "", "", "", nil, status, msg
	}
	name, wantSHA, pluginID, _, status, msg := indexArtifact(doc.Modules, module, body.Version, body.Artifact)
	if name == "" {
		return "", "", "", nil, status, msg
	}
	if body.SHA256 != "" && !equalHex(body.SHA256, wantSHA) {
		return "", "", "", nil, http.StatusConflict, "sha256 does not match the index"
	}
	version = body.Version
	if version == "" {
		var mods map[string]json.RawMessage
		_ = json.Unmarshal(doc.Modules, &mods)
		var entry struct {
			Latest string `json:"latest"`
		}
		_ = json.Unmarshal(mods[module], &entry)
		version = entry.Latest
	}
	if !validModuleToken(version) {
		return "", "", "", nil, http.StatusBadGateway, "millforge index: invalid version"
	}

	data, status, msg = m.downloadArtifact(ctx, artifactURL(body.Source, repo, ref, module, version, name), wantSHA)
	if data == nil {
		return "", "", "", nil, status, msg
	}
	if err := validateModuleArtifact(data, pluginID, version); err != nil {
		return "", "", "", nil, http.StatusBadGateway, err.Error()
	}
	return version, name, pluginID, data, http.StatusOK, ""
}

// installPrepared pushes already-verified bytes to one target.
func (m *moduleInstaller) installPrepared(ctx context.Context, base, pluginID, version, name string, data []byte) (bool, string, int, string) {
	prev := m.previousVersion(ctx, base, pluginID)
	loaded, status, msg := m.uploadPlugin(ctx, base, name, pluginID, version, data)
	if status != http.StatusOK {
		return false, prev, status, msg
	}
	if !loaded {
		return false, prev, http.StatusBadGateway, "qwenpaw did not report the plugin as loaded"
	}
	return true, prev, http.StatusOK, ""
}

// installOne performs the shared single-target pipeline.
func (m *moduleInstaller) installOne(ctx context.Context, base, module string, body moduleInstallRequest) (bool, string, int, string) {
	version, name, pluginID, data, status, msg := m.prepareArtifact(ctx, module, body)
	if data == nil {
		return false, "", status, msg
	}
	return m.installPrepared(ctx, base, pluginID, version, name, data)
}

func equalHex(a, b string) bool {
	return len(a) == len(b) && bytes.EqualFold([]byte(a), []byte(b))
}

func decodeInstallBody(r *http.Request) (moduleInstallRequest, int, string) {
	var body moduleInstallRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		return body, http.StatusBadRequest, "invalid JSON body"
	}
	if body.Source == "" {
		return body, http.StatusBadRequest, "source is required"
	}
	return body, http.StatusOK, ""
}

// InstallWorkerPlugin handles POST /api/v1/workers/{name}/plugins/install.
func (h *ResourceHandler) InstallWorkerPlugin(kubeMode, containerPrefix, resourcePrefix string) http.HandlerFunc {
	m := newModuleInstaller(h, kubeMode, containerPrefix, resourcePrefix)
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" || !workerNamePattern.MatchString(name) {
			httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
			return
		}
		if m.kubeMode != "embedded" {
			httputil.WriteError(w, http.StatusServiceUnavailable, "worker plugin install requires embedded mode")
			return
		}
		var worker v1beta1.Worker
		if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
			if apierrors.IsNotFound(err) {
				httputil.WriteError(w, http.StatusNotFound, "worker not found")
				return
			}
			writeK8sError(w, "install worker plugin", err)
			return
		}
		teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
		if err != nil {
			writeK8sError(w, "install worker plugin", err)
			return
		}
		teamName := ""
		if teamObj != nil {
			teamName = teamObj.Name
		}
		if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
			(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) {
			allowed := caller.TeamMatches(teamName)
			if !allowed {
				allowed = caller.WorkerReadable(teamName, name)
			}
			if !allowed {
				httputil.WriteError(w, http.StatusNotFound, "worker not found")
				return
			}
		}
		if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
			httputil.WriteError(w, http.StatusBadRequest, "worker plugin install is only supported for qwenpaw workers")
			return
		}
		body, status, msg := decodeInstallBody(r)
		if status != http.StatusOK {
			httputil.WriteError(w, status, msg)
			return
		}
		module := body.Module
		if module == "" {
			httputil.WriteError(w, http.StatusBadRequest, "module is required")
			return
		}
		base := m.workerBaseURL(worker.Spec.EffectiveWorkerName(worker.Name), worker.Spec.Env)
		loaded, prev, status, msg := m.installOne(r.Context(), base, module, body)
		if status != http.StatusOK {
			httputil.WriteError(w, status, msg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"installed": true, "loaded": loaded, "previous_version": prev})
	}
}

// InstallManagerPlugin handles POST /api/v1/managers/{name}/plugins/install.
func (h *ResourceHandler) InstallManagerPlugin(kubeMode, resourcePrefix string) http.HandlerFunc {
	m := newModuleInstaller(h, kubeMode, resourcePrefix, resourcePrefix)
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" || !workerNamePattern.MatchString(name) {
			httputil.WriteError(w, http.StatusBadRequest, "manager name is required and must be a valid DNS label")
			return
		}
		if m.kubeMode != "embedded" {
			httputil.WriteError(w, http.StatusServiceUnavailable, "manager plugin install requires embedded mode")
			return
		}
		var manager v1beta1.Manager
		if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &manager); err != nil {
			if apierrors.IsNotFound(err) {
				httputil.WriteError(w, http.StatusNotFound, "manager not found")
				return
			}
			writeK8sError(w, "install manager plugin", err)
			return
		}
		if rt := manager.Spec.Runtime; rt != "" && rt != "qwenpaw" {
			httputil.WriteError(w, http.StatusBadRequest, "manager plugin install is only supported for qwenpaw managers")
			return
		}
		body, status, msg := decodeInstallBody(r)
		if status != http.StatusOK {
			httputil.WriteError(w, status, msg)
			return
		}
		module := body.Module
		if module == "" {
			httputil.WriteError(w, http.StatusBadRequest, "module is required")
			return
		}
		loaded, prev, status, msg := m.installOne(r.Context(), m.managerBaseURL(name), module, body)
		if status != http.StatusOK {
			httputil.WriteError(w, status, msg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"installed": true, "loaded": loaded, "previous_version": prev})
	}
}

// InstallTeamModule handles POST /api/v1/teams/{name}/modules/{module}/install.
func (h *ResourceHandler) InstallTeamModule(kubeMode, containerPrefix, resourcePrefix string) http.HandlerFunc {
	m := newModuleInstaller(h, kubeMode, containerPrefix, resourcePrefix)
	return func(w http.ResponseWriter, r *http.Request) {
		teamName := r.PathValue("name")
		module := r.PathValue("module")
		if teamName == "" || !workerNamePattern.MatchString(teamName) {
			httputil.WriteError(w, http.StatusBadRequest, "team name is required and must be a valid DNS label")
			return
		}
		if module == "" || !moduleTokenRE.MatchString(module) {
			httputil.WriteError(w, http.StatusBadRequest, "module is required and must be a valid token")
			return
		}
		if m.kubeMode != "embedded" {
			httputil.WriteError(w, http.StatusServiceUnavailable, "team module install requires embedded mode")
			return
		}
		var team v1beta1.Team
		if err := h.client.Get(r.Context(), client.ObjectKey{Name: teamName, Namespace: h.namespace}, &team); err != nil {
			if apierrors.IsNotFound(err) {
				httputil.WriteError(w, http.StatusNotFound, "team not found")
				return
			}
			writeK8sError(w, "install team module", err)
			return
		}
		if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
			(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
			!caller.TeamMatches(teamName) {
			httputil.WriteError(w, http.StatusNotFound, "team not found")
			return
		}
		body, status, msg := decodeInstallBody(r)
		if status != http.StatusOK {
			httputil.WriteError(w, status, msg)
			return
		}
		updated, failed, target, pluginID, status, msg := m.installTeam(r.Context(), &team, module, body)
		if status != http.StatusOK {
			resp := map[string]any{"error": msg, "updated": updated, "target_version": target}
			if failed != nil {
				resp["failed"] = failed
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		view, found, _ := m.rh.aggregateTeamModule(r, &team, module, pluginID, m.proxy)
		resp := map[string]any{"updated": updated, "target_version": target}
		if failed != nil {
			resp["failed"] = failed
		}
		if found {
			resp["module"] = module
			if view.Module != module {
				resp["pluginId"] = view.Module
			}
			resp["members"] = view.Members
			resp["versions"] = view.Versions
			resp["consistent"] = view.Consistent
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// installTeam rolls the module through the team's workerMembers in order and
// stops at the first failure.
func (m *moduleInstaller) installTeam(ctx context.Context, team *v1beta1.Team, module string, body moduleInstallRequest) ([]string, map[string]string, string, string, int, string) {
	updated := []string{}
	version, name, pluginID, data, status, msg := m.prepareArtifact(ctx, module, body)
	if data == nil {
		return updated, nil, body.Version, "", status, msg
	}
	for _, member := range team.Spec.WorkerMembers {
		var worker v1beta1.Worker
		if err := m.rh.client.Get(ctx, client.ObjectKey{Name: member.Name, Namespace: m.rh.namespace}, &worker); err != nil {
			return updated, map[string]string{"worker": member.Name, "error": "worker not found"}, version, pluginID, http.StatusBadGateway, "member worker not found"
		}
		if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
			return updated, map[string]string{"worker": member.Name, "error": "worker runtime is not qwenpaw"}, version, pluginID, http.StatusBadRequest, "worker runtime is not qwenpaw"
		}
		base := m.workerBaseURL(worker.Spec.EffectiveWorkerName(worker.Name), worker.Spec.Env)
		_, _, status, msg := m.installPrepared(ctx, base, pluginID, version, name, data)
		if status != http.StatusOK {
			return updated, map[string]string{"worker": member.Name, "error": msg}, version, pluginID, status, msg
		}
		updated = append(updated, member.Name)
	}
	return updated, nil, version, pluginID, http.StatusOK, ""
}
