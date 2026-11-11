package server

import (
	"encoding/json"
	"fmt"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestObservabilityConfigControllerOwnsSecretsAndRevision(t *testing.T) {
	h := tracingFixture(t)
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}
	body := `{"backend":"langfuse","enabled":true,"captureContent":true,"mode":"collector","langfuseUrl":"http://langfuse:3000","publicKey":"pk","secretKey":"private-query-secret","collectorEndpoint":"http://collector:4318","collectorHeaders":"Authorization=Bearer%20ingest"}`
	write := func(input string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.PutObservabilityConfig(rec, withCaller(httptest.NewRequest("PUT", "/api/v1/observability/config", strings.NewReader(input)), admin))
		return rec
	}
	rec := write(body)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "private-query-secret") || strings.Contains(rec.Body.String(), "Bearer") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	json.Unmarshal(rec.Body.Bytes(), &view)
	view["secretKey"] = ""
	update, _ := json.Marshal(view)
	if result := write(string(update)); result.Code != 200 {
		t.Fatalf("%d %s", result.Code, result.Body.String())
	}
	config, err := h.langfuseConfiguration(t.Context())
	if err != nil || config.SecretKey != "private-query-secret" {
		t.Fatalf("configuration unavailable after save: %v", err)
	}
	if result := write(body); result.Code != 409 {
		t.Fatalf("stale write accepted: %d", result.Code)
	}
	for _, role := range []string{authpkg.RoleManager, authpkg.RoleTeamLeader, authpkg.RoleWorker} {
		rec := httptest.NewRecorder()
		h.PutObservabilityConfig(rec, withCaller(httptest.NewRequest("PUT", "/api/v1/observability/config", strings.NewReader(body)), &authpkg.CallerIdentity{Role: role}))
		if rec.Code != 403 {
			t.Fatalf("role=%s code=%d", role, rec.Code)
		}
	}
	r := httptest.NewRequest("GET", "/api/v1/observability/config", nil)
	rec = httptest.NewRecorder()
	h.GetObservabilityConfig(rec, withCaller(r, admin))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "private-query-secret") {
		t.Fatal("configuration API exposed query credentials")
	}
}

func TestObservabilityConnectionUsesStoredSecretsAndNoSyntheticSpans(t *testing.T) {
	h := tracingFixture(t)
	requests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/api/public/projects" {
			user, password, _ := r.BasicAuth()
			if user != "pk" || password != "private-query-secret" {
				t.Error("query credentials were not read from Controller data")
			}
			w.Write([]byte(`{"data":[{"id":"p"}]}`))
		} else {
			if r.Header.Get("Authorization") != "Bearer ingest" {
				t.Error("Collector did not use stored ingestion credentials")
			}
			var body struct {
				ResourceSpans []any `json:"resourceSpans"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.ResourceSpans) != 0 {
				t.Error("connection test created synthetic spans")
			}
			w.Write([]byte(`{}`))
		}
	}))
	defer backend.Close()
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}
	settings := map[string]any{"backend": "langfuse", "mode": "collector", "enabled": true, "captureContent": true, "langfuseUrl": backend.URL, "collectorEndpoint": backend.URL, "publicKey": "pk", "secretKey": "private-query-secret", "collectorHeaders": "Authorization=Bearer%20ingest"}
	data, _ := json.Marshal(settings)
	rec := httptest.NewRecorder()
	h.PutObservabilityConfig(rec, withCaller(httptest.NewRequest("PUT", "/api/v1/observability/config", strings.NewReader(string(data))), admin))
	if rec.Code != 200 {
		t.Fatalf("save status=%d", rec.Code)
	}
	settings["secretKey"], settings["collectorHeaders"] = "", ""
	data, _ = json.Marshal(map[string]any{"settings": settings})
	rec = httptest.NewRecorder()
	h.TestObservabilityConfig(rec, withCaller(httptest.NewRequest("POST", "/api/v1/observability/test", strings.NewReader(string(data))), admin))
	if rec.Code != 200 || requests != 2 || strings.Contains(rec.Body.String(), "private-query-secret") {
		t.Fatalf("test status=%d requests=%d", rec.Code, requests)
	}
}

func TestObservabilityApplyPreservesWorkerEnvAndRejectsStaleRevision(t *testing.T) {
	h := tracingFixture(t)
	saveTraceBackend(t, h, "http://langfuse")
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}
	worker := &v1beta1.Worker{}
	key := client.ObjectKey{Namespace: "default", Name: "worker-cr"}
	if err := h.client.Get(t.Context(), key, worker); err != nil {
		t.Fatal(err)
	}
	worker.Spec.Env = map[string]string{"KEEP": "yes"}
	if err := h.client.Update(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"stale", "test"} {
		body := fmt.Sprintf(`{"workers":["worker-cr"],"revision":%q}`, revision)
		rec := httptest.NewRecorder()
		h.ApplyObservabilityConfig(rec, withCaller(httptest.NewRequest("POST", "/api/v1/observability/apply", strings.NewReader(body)), admin))
		if revision == "stale" {
			if rec.Code != 409 {
				t.Fatal("stale configuration applied")
			}
			continue
		}
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"success":true`) {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	if err := h.client.Get(t.Context(), key, worker); err != nil {
		t.Fatal(err)
	}
	if worker.Spec.Env["KEEP"] != "yes" || worker.Spec.Env["AGENTTEAMS_OTEL_CONFIG_REVISION"] != "test" {
		t.Fatal("Worker env was not merged from Controller configuration")
	}
	encoded, _ := json.Marshal(worker.Spec.Env)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("query credentials reached Worker env")
	}
}
