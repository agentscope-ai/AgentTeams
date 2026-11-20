package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

type internalStaticAuth struct {
	id    *authpkg.CallerIdentity
	token string
	err   error
}

func (a *internalStaticAuth) Authenticate(ctx context.Context, token string) (*authpkg.CallerIdentity, error) {
	if a.err != nil {
		return nil, a.err
	}
	if token != a.token {
		return nil, errors.New("invalid token")
	}
	return a.id, nil
}

func internalTestServer(t *testing.T, objs ...client.Object) (*HTTPServer, client.Client) {
	t.Helper()
	scheme := newProjectTestScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	k8s := builder.Build()
	mw := authpkg.NewMiddleware(&internalStaticAuth{
		token: "public-token",
		id: &authpkg.CallerIdentity{
			MatrixUserID:         "@admin:matrix.local",
			HumanPermissionLevel: 1,
			Role:                 authpkg.RoleAdmin,
			Username:             "admin",
			Teams:                []string{"team-a"},
			Capabilities:         []string{"external_sources"},
			AccessibleWorkers:    []string{"w1"},
		},
	}, nil, authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{
		Client:               k8s,
		Namespace:            "default",
		KubeMode:             "embedded",
		ContainerPrefix:      "agentteams-worker-",
		AuthMw:               mw,
		InternalToken:        "internal-token",
		ControllerName:       "ctl-a",
		DefaultWorkerRuntime: "qwenpaw",
		DefaultModel:         "m-default",
	})
	return srv, k8s
}

func internalRequest(t *testing.T, srv *HTTPServer, method, path, body, token, timestamp, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if timestamp == "" {
		timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	r.Header.Set("X-AT-Timestamp", timestamp)
	if requestID == "" {
		requestID = "req-1"
	}
	if requestID != "-" { // "-" keeps the header absent on purpose
		r.Header.Set("X-AT-Request-Id", requestID)
	}
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, r)
	return rec
}

func TestInternalGuard(t *testing.T) {
	srv, _ := internalTestServer(t)
	cases := []struct {
		name      string
		token     string
		timestamp string
		requestID string
		want      int
	}{
		{"missing token", "", "", "", http.StatusUnauthorized},
		{"wrong token", "nope", "", "", http.StatusUnauthorized},
		{"missing timestamp", "internal-token", "not-a-time", "", http.StatusUnauthorized},
		{"stale timestamp", "internal-token", time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339), "", http.StatusUnauthorized},
		{"missing request id", "internal-token", "", "-", http.StatusBadRequest},
		{"ok", "internal-token", "", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := internalRequest(t, srv, http.MethodPost, "/internal/v1/token-review",
				`{"token":"public-token"}`, tc.token, tc.timestamp, tc.requestID)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestInternalTokenReview(t *testing.T) {
	srv, _ := internalTestServer(t)
	rec := internalRequest(t, srv, http.MethodPost, "/internal/v1/token-review", `{"token":"public-token"}`, "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var id internalIdentity
	if err := json.Unmarshal(rec.Body.Bytes(), &id); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if id.Role != authpkg.RoleAdmin || id.Username != "admin" || id.MatrixUserID == "" || len(id.Teams) != 1 {
		t.Fatalf("identity = %+v", id)
	}

	bad := internalRequest(t, srv, http.MethodPost, "/internal/v1/token-review", `{"token":"wrong"}`, "internal-token", "", "")
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d", bad.Code)
	}
	empty := internalRequest(t, srv, http.MethodPost, "/internal/v1/token-review", `{}`, "internal-token", "", "")
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty token status = %d", empty.Code)
	}
}

const wireSpecJSON = `{"workerName":"Worker One","model":"m1","modelProvider":"apig","runtime":"qwenpaw","containerManaged":true,"state":"Running","env":{"A":"b"},"soul":"s","skills":["s1"]}`

func TestInternalProvisionCreatesAndOrders(t *testing.T) {
	srv, k8s := internalTestServer(t)
	body := func(gen int64, model string) string {
		return `{"name":"w1","generation":` + strconv.FormatInt(gen, 10) + `,"spec":` + strings.Replace(wireSpecJSON, `"m1"`, `"`+model+`"`, 1) + `}`
	}

	rec := internalRequest(t, srv, http.MethodPost, "/internal/v1/workers/w1/provision", body(1, "m1"), "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("provision create = %d %s", rec.Code, rec.Body.String())
	}
	var w v1beta1.Worker
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w1"}, &w); err != nil {
		t.Fatalf("get created worker: %v", err)
	}
	if w.Spec.Model != "m1" || w.Spec.Runtime != "qwenpaw" || w.Spec.WorkerName != "Worker One" {
		t.Fatalf("spec = %+v", w.Spec)
	}
	if w.Spec.ContainerManaged == nil || !*w.Spec.ContainerManaged || w.Spec.State == nil || *w.Spec.State != "Running" {
		t.Fatalf("containerManaged/state = %+v", w.Spec)
	}
	if w.Annotations[provisionGenerationAnnotation] != "1" {
		t.Fatalf("generation annotation = %q", w.Annotations[provisionGenerationAnnotation])
	}
	if w.Labels[v1beta1.LabelController] != "ctl-a" {
		t.Fatalf("controller label = %q", w.Labels[v1beta1.LabelController])
	}
	if _, ok := w.Annotations[modelGrantAnnotation]; !ok {
		t.Fatalf("implicit model grant annotation missing: %v", w.Annotations)
	}

	// Same generation redelivery (even with a different model) is a no-op.
	rec = internalRequest(t, srv, http.MethodPost, "/internal/v1/workers/w1/provision", body(1, "m2"), "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("redelivery = %d", rec.Code)
	}
	_ = k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w1"}, &w)
	if w.Spec.Model != "m1" {
		t.Fatalf("redelivery overwrote spec: model = %q", w.Spec.Model)
	}

	// Older generation after a newer one never applies.
	rec = internalRequest(t, srv, http.MethodPost, "/internal/v1/workers/w1/provision", body(3, "m3"), "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("gen3 = %d", rec.Code)
	}
	rec = internalRequest(t, srv, http.MethodPost, "/internal/v1/workers/w1/provision", body(2, "m2"), "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("gen2 after gen3 = %d", rec.Code)
	}
	_ = k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w1"}, &w)
	if w.Spec.Model != "m3" || w.Annotations[provisionGenerationAnnotation] != "3" {
		t.Fatalf("out-of-order provision applied: model=%q gen=%q", w.Spec.Model, w.Annotations[provisionGenerationAnnotation])
	}
}

func TestInternalProvisionDefaultsAndValidation(t *testing.T) {
	srv, k8s := internalTestServer(t)
	// Empty spec: runtime and model defaults, containerManaged true.
	rec := internalRequest(t, srv, http.MethodPost, "/internal/v1/workers/w2/provision", `{"name":"w2","generation":1,"spec":{}}`, "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("defaults = %d %s", rec.Code, rec.Body.String())
	}
	var w v1beta1.Worker
	_ = k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w2"}, &w)
	if w.Spec.Runtime != "qwenpaw" || w.Spec.Model != "m-default" || w.Spec.ContainerManaged == nil || !*w.Spec.ContainerManaged {
		t.Fatalf("defaults not applied: %+v", w.Spec)
	}

	bad := []struct {
		name string
		body string
	}{
		{"zero generation", `{"name":"w3","generation":0,"spec":{}}`},
		{"name mismatch", `{"name":"other","generation":1,"spec":{}}`},
		{"bad spec", `{"name":"w3","generation":1,"spec":{"skills":"not-an-array"}}`},
		{"copaw", `{"name":"w3","generation":1,"spec":{"runtime":"copaw"}}`},
	}
	for _, tc := range bad {
		rec := internalRequest(t, srv, http.MethodPost, "/internal/v1/workers/w3/provision", tc.body, "internal-token", "", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

func TestInternalDeprovisionAndExecution(t *testing.T) {
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "w4",
			Namespace:   "default",
			Annotations: map[string]string{provisionGenerationAnnotation: "7"},
		},
		Spec: v1beta1.WorkerSpec{Model: "m", Runtime: "qwenpaw"},
		Status: v1beta1.WorkerStatus{
			Phase:            "Running",
			ContainerState:   "running",
			MatrixUserID:     "@w4:matrix.local",
			RoomID:           "!r:matrix.local",
			AgentStatus:      "idle",
			RunningTaskCount: intPtr(0),
			LastRunAt:        "2026-11-13T00:00:00Z",
		},
	}
	srv, k8s := internalTestServer(t, worker)

	rec := internalRequest(t, srv, http.MethodGet, "/internal/v1/workers/w4/execution", "", "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("execution = %d %s", rec.Code, rec.Body.String())
	}
	var got executionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode execution: %v", err)
	}
	if got.Name != "w4" || got.Generation != 7 || got.Phase != "Running" || got.MatrixUserID == "" || got.RunningTaskCount == nil {
		t.Fatalf("execution = %+v", got)
	}

	missing := internalRequest(t, srv, http.MethodGet, "/internal/v1/workers/nope/execution", "", "internal-token", "", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing execution = %d", missing.Code)
	}

	del := internalRequest(t, srv, http.MethodPost, "/internal/v1/workers/w4/deprovision", `{"name":"w4","generation":7}`, "internal-token", "", "")
	if del.Code != http.StatusOK {
		t.Fatalf("deprovision = %d %s", del.Code, del.Body.String())
	}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w4"}, &v1beta1.Worker{}); err == nil {
		t.Fatalf("worker still exists after deprovision")
	}
	again := internalRequest(t, srv, http.MethodPost, "/internal/v1/workers/w4/deprovision", `{"name":"w4","generation":7}`, "internal-token", "", "")
	if again.Code != http.StatusOK {
		t.Fatalf("idempotent deprovision = %d", again.Code)
	}
}

// TestInternalProvisionIncarnation pins delete→create: the same generation in
// a new life must apply (the old annotation cannot block it), and a sync
// event (no spec) must not touch the stored spec.
func TestInternalProvisionIncarnation(t *testing.T) {
	srv, k8s := internalTestServer(t)
	body := func(gen int64, inc, model string, withSpec bool) string {
		spec := ""
		if withSpec {
			spec = `,"spec":{"model":"` + model + `","runtime":"qwenpaw"}`
		}
		return `{"name":"w-inc","generation":` + strconv.FormatInt(gen, 10) +
			`,"incarnation":"` + inc + `"` + spec + `}`
	}
	post := func(body string, path string) int {
		return internalRequest(t, srv, http.MethodPost, path, body, "internal-token", "", "").Code
	}

	if got := post(body(1, "life-A", "m1", true), "/internal/v1/workers/w-inc/provision"); got != http.StatusOK {
		t.Fatalf("create = %d", got)
	}
	// New life, same generation, different incarnation: must apply.
	if got := post(body(1, "life-B", "m2", true), "/internal/v1/workers/w-inc/provision"); got != http.StatusOK {
		t.Fatalf("recreate = %d", got)
	}
	var w v1beta1.Worker
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w-inc"}, &w); err != nil {
		t.Fatalf("get: %v", err)
	}
	if w.Spec.Model != "m2" || w.Annotations[provisionIncarnationAnnotation] != "life-B" {
		t.Fatalf("new life not applied: model=%q inc=%q", w.Spec.Model, w.Annotations[provisionIncarnationAnnotation])
	}

	// Generation-only sync: annotations move, spec stays.
	if got := post(body(2, "life-B", "", false), "/internal/v1/workers/w-inc/provision"); got != http.StatusOK {
		t.Fatalf("sync = %d", got)
	}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w-inc"}, &w); err != nil {
		t.Fatalf("get after sync: %v", err)
	}
	if w.Spec.Model != "m2" || w.Annotations[provisionGenerationAnnotation] != "2" {
		t.Fatalf("sync changed the spec or generation: model=%q gen=%q", w.Spec.Model, w.Annotations[provisionGenerationAnnotation])
	}
}

// TestInternalDeprovisionStaleIncarnation: a delayed deprovision from a
// previous life must not delete the current CR.
func TestInternalDeprovisionStaleIncarnation(t *testing.T) {
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "w-stale",
			Namespace: "default",
			Annotations: map[string]string{
				provisionGenerationAnnotation:  "1",
				provisionIncarnationAnnotation: "life-B",
			},
		},
	}
	srv, k8s := internalTestServer(t, worker)
	path := "/internal/v1/workers/w-stale/deprovision"

	// Stale life: no-op, worker survives.
	rec := internalRequest(t, srv, http.MethodPost, path, `{"name":"w-stale","generation":1,"incarnation":"life-A"}`, "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stale deprovision = %d", rec.Code)
	}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w-stale"}, &v1beta1.Worker{}); err != nil {
		t.Fatalf("stale deprovision deleted the current CR: %v", err)
	}

	// Current life: deletes.
	rec = internalRequest(t, srv, http.MethodPost, path, `{"name":"w-stale","generation":1,"incarnation":"life-B"}`, "internal-token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("current deprovision = %d", rec.Code)
	}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "w-stale"}, &v1beta1.Worker{}); err == nil {
		t.Fatalf("current deprovision did not delete the CR")
	}
}

func TestInternalRoutesDisabledWithoutToken(t *testing.T) {
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", AuthMw: authpkg.NewMiddleware(nil, nil, authpkg.NewAuthorizer(), k8s, "default")})
	rec := internalRequest(t, srv, http.MethodPost, "/internal/v1/token-review", `{"token":"x"}`, "internal-token", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled internal surface = %d, want 404", rec.Code)
	}
}

func intPtr(v int) *int { return &v }
