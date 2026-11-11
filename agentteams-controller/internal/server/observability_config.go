package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/observability"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const observabilitySecretName = observability.SecretName

func (h *ProjectHandler) readObservabilityConfig(ctx context.Context) (map[string]any, *corev1.Secret, error) {
	return observability.Read(ctx, h.configReader, h.namespace)
}

func observabilityView(config map[string]any) map[string]any {
	view := map[string]any{"backend": "langfuse", "agentloop": map[string]any{"region": "", "workspace": "", "project": "", "logstore": ""}, "enabled": false, "captureContent": true, "mode": "collector", "langfuseUrl": "", "publicUrl": "", "projectId": "", "publicKey": "", "collectorEndpoint": "", "revision": "environment"}
	for key, value := range config {
		if key != "secretKey" && key != "collectorHeaders" {
			view[key] = value
		}
	}
	view["secretKeyConfigured"] = str(config["secretKey"]) != ""
	view["collectorHeadersConfigured"] = str(config["collectorHeaders"]) != ""
	view["configured"] = str(config["revision"]) != ""
	view["source"] = "controller"
	return view
}

// Observability configuration is an administrator operation. Manager/Leader
// tokens query traces, but never read or change project query credentials.
func observabilityAdmin(w http.ResponseWriter, r *http.Request) bool {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, 401, "authentication required")
		return false
	}
	if caller.Role != authpkg.RoleAdmin {
		httputil.WriteError(w, 403, "only administrators can configure observability")
		return false
	}
	return true
}

func (h *ProjectHandler) GetObservabilityConfig(w http.ResponseWriter, r *http.Request) {
	if traceReader(w, r) == nil {
		return
	}
	config, _, err := h.readObservabilityConfig(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, "cannot read observability configuration")
		return
	}
	httputil.WriteJSON(w, 200, observabilityView(config))
}

func (h *ProjectHandler) PutObservabilityConfig(w http.ResponseWriter, r *http.Request) {
	if !observabilityAdmin(w, r) {
		return
	}
	previous, secret, err := h.readObservabilityConfig(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, "cannot read observability configuration")
		return
	}
	var input map[string]any
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&input) != nil || input == nil {
		httputil.WriteError(w, 400, "configuration must be a JSON object")
		return
	}
	if secret != nil && str(input["revision"]) != str(previous["revision"]) {
		httputil.WriteError(w, 409, "observability configuration changed; reload before saving")
		return
	}
	mergeObservability(previous, input)
	if err := validateObservability(previous); err != nil {
		httputil.WriteError(w, 400, err.Error())
		return
	}
	previous["revision"] = uuid.NewString()
	data, _ := json.Marshal(previous)
	if secret == nil {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: observabilitySecretName, Namespace: h.namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"config.json": data}}
		err = h.client.Create(r.Context(), secret)
	} else {
		secret.Data["config.json"] = data
		err = h.client.Update(r.Context(), secret)
	}
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		httputil.WriteError(w, 409, "observability configuration changed; reload before saving")
		return
	}
	if err != nil {
		httputil.WriteError(w, 503, "cannot save observability configuration")
		return
	}
	httputil.WriteJSON(w, 200, observabilityView(previous))
}

func mergeObservability(previous, input map[string]any) {
	for _, key := range []string{"backend", "agentloop", "enabled", "captureContent", "mode", "langfuseUrl", "publicUrl", "projectId", "publicKey", "secretKey", "collectorEndpoint", "collectorHeaders"} {
		value, exists := input[key]
		if !exists {
			continue
		}
		if text, ok := value.(string); ok {
			value = strings.TrimSpace(text)
		}
		if (key == "secretKey" || key == "collectorHeaders") && value == "" {
			continue
		}
		previous[key] = value
	}
}

func collectorHeaders(value string) (http.Header, error) {
	headers := http.Header{}
	for _, entry := range strings.Split(value, ",") {
		if entry == "" {
			continue
		}
		key, encoded, ok := strings.Cut(entry, "=")
		decoded, err := url.PathUnescape(encoded)
		if !ok || err != nil || strings.TrimSpace(key) == "" || strings.ContainsAny(key+decoded, "\r\n\x00") {
			return nil, fmt.Errorf("invalid Collector headers")
		}
		headers.Set(strings.TrimSpace(key), decoded)
	}
	return headers, nil
}

func (h *ProjectHandler) TestObservabilityConfig(w http.ResponseWriter, r *http.Request) {
	if !observabilityAdmin(w, r) {
		return
	}
	config, _, err := h.readObservabilityConfig(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, "cannot read observability configuration")
		return
	}
	var input struct {
		Settings map[string]any `json:"settings"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&input) != nil || input.Settings == nil {
		httputil.WriteError(w, 400, "settings required")
		return
	}
	mergeObservability(config, input.Settings)
	if err := validateObservability(config); err != nil {
		httputil.WriteError(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	projectID := str(config["projectId"])
	if config["backend"] == "langfuse" {
		backend := langfuseQuery{LangfuseURL: strings.TrimRight(str(config["langfuseUrl"]), "/"), PublicKey: str(config["publicKey"]), SecretKey: str(config["secretKey"])}
		var projects struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := backend.get(ctx, "projects", &projects); err != nil {
			httputil.WriteError(w, 502, err.Error())
			return
		}
		if len(projects.Data) == 0 {
			httputil.WriteError(w, 502, "Langfuse project authentication returned no project")
			return
		}
		if projectID == "" {
			projectID = projects.Data[0].ID
		}
		validProject := false
		for _, project := range projects.Data {
			if project.ID == projectID {
				validProject = true
			}
		}
		if !validProject {
			httputil.WriteError(w, 400, "Langfuse credentials do not match the selected project")
			return
		}
	} else {
		source, _ := config["agentloop"].(map[string]any)
		projectID = str(source["project"])
	}
	headers, err := collectorHeaders(str(config["collectorHeaders"]))
	if err != nil {
		httputil.WriteError(w, 400, err.Error())
		return
	}
	request, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(str(config["collectorEndpoint"]), "/")+"/v1/traces", bytes.NewBufferString(`{"resourceSpans":[]}`))
	if err != nil {
		httputil.WriteError(w, 400, "invalid Collector endpoint")
		return
	}
	request.Header = headers
	request.Header.Set("Content-Type", "application/json")
	connection := http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := connection.Do(request)
	if err != nil {
		httputil.WriteError(w, 502, "Collector connection failed")
		return
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		httputil.WriteError(w, 502, "Collector rejected the connection test")
		return
	}
	message := "Controller query access and Collector endpoint verified; run a real Worker task to verify collection"
	if config["backend"] == "agentloop" {
		message = "Collector endpoint verified; AgentLoop query access is checked by the Dashboard"
	}
	httputil.WriteJSON(w, 200, map[string]any{"projectId": projectID, "source": "controller", "message": message})
}

func (h *ProjectHandler) ApplyObservabilityConfig(w http.ResponseWriter, r *http.Request) {
	if !observabilityAdmin(w, r) {
		return
	}
	var input struct {
		Workers  []string `json:"workers"`
		Revision string   `json:"revision"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&input) != nil || len(input.Workers) < 1 || len(input.Workers) > 50 {
		httputil.WriteError(w, 400, "select 1 to 50 Workers")
		return
	}
	config, secret, err := h.readObservabilityConfig(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, "cannot read observability configuration")
		return
	}
	if secret == nil || input.Revision != str(config["revision"]) {
		httputil.WriteError(w, 409, "configuration changed; reload before applying")
		return
	}
	results := make([]map[string]any, 0, len(input.Workers))
	seen := map[string]bool{}
	for _, name := range input.Workers {
		if seen[name] {
			continue
		}
		seen[name] = true
		result := map[string]any{"name": name, "success": false}
		worker := &v1beta1.Worker{}
		if err := h.client.Get(r.Context(), client.ObjectKey{Namespace: h.namespace, Name: name}, worker); err != nil {
			result["message"] = "Worker not found"
		} else if (worker.Spec.Runtime != "qwenpaw" && worker.Spec.Runtime != "hermes") || !worker.Spec.DesiredContainerMan() {
			result["message"] = "requires a managed QwenPaw or Hermes Worker"
		} else {
			base := worker.DeepCopy()
			if worker.Spec.Env == nil {
				worker.Spec.Env = map[string]string{}
			}
			for key, value := range observability.WorkerEnv(config) {
				worker.Spec.Env[key] = value
			}
			if err := h.client.Patch(r.Context(), worker, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				result["message"] = "Worker update failed; reload before retrying"
			} else {
				result["success"], result["revision"], result["message"] = true, input.Revision, "配置已下发；托管容器将在重建后生效。"
			}
		}
		results = append(results, result)
	}
	httputil.WriteJSON(w, 200, map[string]any{"results": results})
}

func validateObservability(config map[string]any) error {
	if config["mode"] != "collector" {
		return fmt.Errorf("use collector mode")
	}
	if config["backend"] != "langfuse" && config["backend"] != "agentloop" {
		return fmt.Errorf("invalid tracing backend")
	}
	for _, key := range []string{"enabled", "captureContent"} {
		if _, ok := config[key].(bool); !ok {
			return fmt.Errorf("invalid collection switch")
		}
	}
	for _, key := range []string{"langfuseUrl", "publicUrl", "projectId", "publicKey", "secretKey", "collectorEndpoint", "collectorHeaders"} {
		if value, exists := config[key]; exists {
			text, ok := value.(string)
			if !ok || len(text) > 8192 || strings.ContainsAny(text, "\r\n\x00") {
				return fmt.Errorf("invalid configuration field: %s", key)
			}
		}
	}
	for _, key := range []string{"langfuseUrl", "publicUrl", "collectorEndpoint"} {
		if value := str(config[key]); value != "" {
			u, err := url.Parse(value)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("invalid service URL: %s", key)
			}
		}
	}
	if _, err := collectorHeaders(str(config["collectorHeaders"])); err != nil {
		return err
	}
	if config["enabled"] == true {
		if str(config["collectorEndpoint"]) == "" {
			return fmt.Errorf("Collector endpoint required")
		}
		if config["backend"] == "langfuse" && (str(config["langfuseUrl"]) == "" || str(config["publicKey"]) == "" || str(config["secretKey"]) == "") {
			return fmt.Errorf("Langfuse query settings required")
		}
	}
	return nil
}

func (h *ProjectHandler) langfuseConfiguration(ctx context.Context) (langfuseQuery, error) {
	config, secret, err := h.readObservabilityConfig(ctx)
	if err != nil {
		return langfuseQuery{}, fmt.Errorf("cannot read observability configuration")
	}
	if secret == nil {
		return langfuseQuery{}, fmt.Errorf("configure observability using the Controller API")
	}
	if config["backend"] != "langfuse" {
		return langfuseQuery{}, fmt.Errorf("task tracing currently requires Langfuse")
	}
	result := langfuseQuery{LangfuseURL: strings.TrimRight(str(config["langfuseUrl"]), "/"), PublicKey: str(config["publicKey"]), SecretKey: str(config["secretKey"])}
	if result.LangfuseURL == "" || result.PublicKey == "" || result.SecretKey == "" {
		return result, fmt.Errorf("Langfuse query settings not configured")
	}
	return result, nil
}
