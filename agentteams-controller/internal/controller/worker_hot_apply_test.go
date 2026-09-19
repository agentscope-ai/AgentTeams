package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPatchSubagentModel(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := patchSubagentModel(context.Background(), srv.URL, "default", "qwen3.6-flash"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/api/agents/default/model-settings" {
		t.Fatalf("got %s %s, want PATCH /api/agents/default/model-settings", gotMethod, gotPath)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, gotBody)
	}
	slot, ok := payload["subagent_model"].(map[string]any)
	if !ok {
		t.Fatalf("subagent_model not an object: %s", gotBody)
	}
	if slot["provider_id"] != "agentteams-gateway" || slot["model"] != "qwen3.6-flash" {
		t.Fatalf("unexpected slot: %v", slot)
	}
}

// model="" must send an explicit null (clear), not omit the field — the
// console endpoint only applies fields present in the body, so omitting
// would leave the previous subagent model in place.
func TestPatchSubagentModelClearSendsNull(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := patchSubagentModel(context.Background(), srv.URL, "default", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, gotBody)
	}
	raw, ok := payload["subagent_model"]
	if !ok {
		t.Fatalf("subagent_model field missing from body: %s", gotBody)
	}
	if string(raw) != "null" {
		t.Fatalf("subagent_model = %s, want null", raw)
	}
}

func TestPatchSubagentModelNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	err := patchSubagentModel(context.Background(), srv.URL, "default", "x")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

// Regression guard (#1292 Part 3): a subagent model change must NOT change
// the container-recreation hash. For member-runtime-config runtimes (the
// qwenpaw production path) the hash is a whitelist that never included
// model fields; for the legacy path with explicit resources the field is
// zeroed before hashing, exactly like spec.Model.
func TestWorkerSpecHashIgnoresSubagentModel(t *testing.T) {
	base := v1beta1.WorkerSpec{Model: "qwen3.6-27b-fp8"}
	withSlot := base
	withSlot.SubagentModel = "qwen3.6-flash"

	for _, runtime := range []string{"qwenpaw", "openclaw"} {
		resources := &v1beta1.AgentResourceRequirements{}
		ha := hashAppliedWorkerSpecForRuntimeAndResources(base, runtime, resources)
		hb := hashAppliedWorkerSpecForRuntimeAndResources(withSlot, runtime, resources)
		if ha != hb {
			t.Errorf("runtime %s: hash changes with SubagentModel (would recreate the container)", runtime)
		}
	}
}

// applySubagentModelHot: running worker + pending change → dial + record
// annotation; second pass → no further dial.
func TestApplySubagentModelHotDialsOnce(t *testing.T) {
	dials := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	scheme := newControllerTestScheme(t)
	w := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(w.DeepCopy()).Build()
	r := &WorkerReconciler{
		Client:              k8sClient,
		KubeMode:            "embedded",
		subagentModelURLFor: func(*v1beta1.Worker, v1beta1.WorkerSpec) string { return srv.URL },
	}
	state := &MemberState{ContainerState: "running"}
	spec := v1beta1.WorkerSpec{SubagentModel: "qwen3.6-flash"}
	key := types.NamespacedName{Name: "alpha-dev", Namespace: "default"}

	r.applySubagentModelHot(context.Background(), w, spec, MemberContext{}, state)
	if dials != 1 {
		t.Fatalf("dials = %d after first pass, want 1", dials)
	}
	var got v1beta1.Worker
	if err := k8sClient.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.Annotations[annotationSubagentModelApplied] != "qwen3.6-flash" {
		t.Fatalf("annotation = %q, want %q", got.Annotations[annotationSubagentModelApplied], "qwen3.6-flash")
	}

	// Second pass with the recorded annotation: no new dial.
	w = &got
	r.applySubagentModelHot(context.Background(), w, spec, MemberContext{}, state)
	if dials != 1 {
		t.Fatalf("dials = %d after second pass, want 1 (annotation matched)", dials)
	}
}

// applySubagentModelHot skips when the worker is not running or in
// incluster mode — the dial is only possible for running embedded workers.
func TestApplySubagentModelHotSkips(t *testing.T) {
	dials := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	scheme := newControllerTestScheme(t)
	mkWorker := func() *v1beta1.Worker {
		return &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"}}
	}
	mkR := func(w *v1beta1.Worker, kubeMode string) *WorkerReconciler {
		return &WorkerReconciler{
			Client:              fake.NewClientBuilder().WithScheme(scheme).WithObjects(w.DeepCopy()).Build(),
			KubeMode:            kubeMode,
			subagentModelURLFor: func(*v1beta1.Worker, v1beta1.WorkerSpec) string { return srv.URL },
		}
	}
	spec := v1beta1.WorkerSpec{SubagentModel: "qwen3.6-flash"}

	w := mkWorker()
	r := mkR(w, "embedded")
	r.applySubagentModelHot(context.Background(), w, spec, MemberContext{}, &MemberState{ContainerState: "starting"})
	if dials != 0 {
		t.Fatalf("dialed while container not running: %d", dials)
	}

	w = mkWorker()
	r = mkR(w, "incluster")
	r.applySubagentModelHot(context.Background(), w, spec, MemberContext{}, &MemberState{ContainerState: "running"})
	if dials != 0 {
		t.Fatalf("dialed in incluster mode: %d", dials)
	}
}

// Team default resolution: explicit worker value wins over the team
// default; an empty worker value falls back to the team default.
func TestApplySubagentModelHotResolvesTeamDefault(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	scheme := newControllerTestScheme(t)
	w := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"}}
	r := &WorkerReconciler{
		Client:              fake.NewClientBuilder().WithScheme(scheme).WithObjects(w.DeepCopy()).Build(),
		KubeMode:            "embedded",
		subagentModelURLFor: func(*v1beta1.Worker, v1beta1.WorkerSpec) string { return srv.URL },
	}
	state := &MemberState{ContainerState: "running"}

	// Worker value empty → team default is dialed.
	r.applySubagentModelHot(context.Background(), w, v1beta1.WorkerSpec{},
		MemberContext{TeamSubagentModel: "team-default-model"}, state)
	if !strings.Contains(gotBody, `"model":"team-default-model"`) {
		t.Fatalf("team default not dialed: %s", gotBody)
	}

	// Explicit worker value beats the team default. (The first pass
	// recorded annotation "team-default-model" on w, so this mismatch
	// must trigger a second dial with the explicit value.)
	gotBody = ""
	r.applySubagentModelHot(context.Background(), w, v1beta1.WorkerSpec{SubagentModel: "worker-explicit"},
		MemberContext{TeamSubagentModel: "team-default-model"}, state)
	if !strings.Contains(gotBody, `"model":"worker-explicit"`) {
		t.Fatalf("worker explicit not dialed: %s", gotBody)
	}
}
