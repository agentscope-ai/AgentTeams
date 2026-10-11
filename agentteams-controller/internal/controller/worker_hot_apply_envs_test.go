package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// envsRequest captures one dial the fake worker console received.
type envsRequest struct {
	method string
	path   string
	body   string
}

// newEnvsRecordingServer returns an httptest server that records every
// request and answers with the given status.
func newEnvsRecordingServer(t *testing.T, status int) (*httptest.Server, *[]envsRequest) {
	t.Helper()
	reqs := &[]envsRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, envsRequest{method: r.Method, path: r.URL.Path, body: string(b)})
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, reqs
}

func newEnvsTestWorker(annotations map[string]string) *v1beta1.Worker {
	return &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{
		Name: "alpha-dev", Namespace: "default", Annotations: annotations,
	}}
}

// newEnvsTestReconciler wires an embedded reconciler whose envs dial targets
// the httptest server.
func newEnvsTestReconciler(t *testing.T, url string, w *v1beta1.Worker) (*WorkerReconciler, types.NamespacedName) {
	t.Helper()
	scheme := newControllerTestScheme(t)
	key := types.NamespacedName{Name: w.Name, Namespace: w.Namespace}
	r := &WorkerReconciler{
		Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(w.DeepCopy()).Build(),
		KubeMode:   "embedded",
		envsURLFor: func(*v1beta1.Worker, v1beta1.WorkerSpec) string { return url },
	}
	return r, key
}

func envsPayload(t *testing.T, body string) map[string]string {
	t.Helper()
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body not a JSON object: %v (%s)", err, body)
	}
	return got
}

// T1: resolve merge — an explicit worker value beats the team default; an
// empty worker value inherits the team default; both-empty applies nothing.
func TestApplyLlmStreamTimeoutsHotResolvesMerge(t *testing.T) {
	cases := []struct {
		name      string
		spec      v1beta1.WorkerSpec
		team      MemberContext
		wantReqs  int
		wantFirst string
		wantIdle  string
	}{
		{
			name:      "worker value wins over team default",
			spec:      v1beta1.WorkerSpec{Runtime: "qwenpaw", LlmStreamFirstContentTimeout: "300", LlmStreamIdleTimeout: "120"},
			team:      MemberContext{TeamLlmStreamFirstContentTimeout: "999", TeamLlmStreamIdleTimeout: "999"},
			wantReqs:  1,
			wantFirst: "300",
			wantIdle:  "120",
		},
		{
			name:      "only team default is inherited",
			spec:      v1beta1.WorkerSpec{Runtime: "qwenpaw"},
			team:      MemberContext{TeamLlmStreamFirstContentTimeout: "300", TeamLlmStreamIdleTimeout: "120"},
			wantReqs:  1,
			wantFirst: "300",
			wantIdle:  "120",
		},
		{
			name:     "both empty applies nothing",
			spec:     v1beta1.WorkerSpec{Runtime: "qwenpaw"},
			team:     MemberContext{},
			wantReqs: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, reqs := newEnvsRecordingServer(t, http.StatusOK)
			w := newEnvsTestWorker(nil)
			r, key := newEnvsTestReconciler(t, srv.URL, w)

			r.applyLlmStreamTimeoutsHot(context.Background(), w, tc.spec, tc.team, &MemberState{ContainerState: "running"})

			if len(*reqs) != tc.wantReqs {
				t.Fatalf("requests = %d, want %d (%+v)", len(*reqs), tc.wantReqs, *reqs)
			}
			if tc.wantReqs == 0 {
				return
			}
			if (*reqs)[0].method != http.MethodPatch || (*reqs)[0].path != "/api/envs" {
				t.Fatalf("got %s %s, want PATCH /api/envs", (*reqs)[0].method, (*reqs)[0].path)
			}
			want := map[string]string{}
			if tc.wantFirst != "" {
				want[envLlmStreamFirstContentTimeout] = tc.wantFirst
			}
			if tc.wantIdle != "" {
				want[envLlmStreamIdleTimeout] = tc.wantIdle
			}
			if got := envsPayload(t, (*reqs)[0].body); !reflect.DeepEqual(got, want) {
				t.Fatalf("payload = %v, want %v", got, want)
			}
			var stored v1beta1.Worker
			if err := r.Client.Get(context.Background(), key, &stored); err != nil {
				t.Fatalf("get worker: %v", err)
			}
			wantAnnotation := formatLlmStreamTimeouts(tc.wantFirst, tc.wantIdle)
			if got := stored.Annotations[annotationLlmStreamTimeoutsApplied]; got != wantAnnotation {
				t.Fatalf("annotation = %q, want %q", got, wantAnnotation)
			}
		})
	}
}

// T2: a matching annotation (including the partial "-:idle" form) is a no-op
// that makes zero HTTP requests.
func TestApplyLlmStreamTimeoutsHotNoopWhenMatched(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		spec       v1beta1.WorkerSpec
	}{
		{
			name:       "both matched",
			annotation: "300:120",
			spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", LlmStreamFirstContentTimeout: "300", LlmStreamIdleTimeout: "120"},
		},
		{
			name:       "partial matched",
			annotation: "-:120",
			spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", LlmStreamIdleTimeout: "120"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, reqs := newEnvsRecordingServer(t, http.StatusOK)
			w := newEnvsTestWorker(map[string]string{annotationLlmStreamTimeoutsApplied: tc.annotation})
			r, _ := newEnvsTestReconciler(t, srv.URL, w)

			r.applyLlmStreamTimeoutsHot(context.Background(), w, tc.spec, MemberContext{}, &MemberState{ContainerState: "running"})

			if len(*reqs) != 0 {
				t.Fatalf("made %d requests, want 0 (annotation matched)", len(*reqs))
			}
		})
	}
}

// T3: a change PATCHes /api/envs with only the non-empty keys and records the
// "first:idle" annotation on success.
func TestApplyLlmStreamTimeoutsHotPatchesOnlySetKeys(t *testing.T) {
	srv, reqs := newEnvsRecordingServer(t, http.StatusOK)
	w := newEnvsTestWorker(nil)
	r, key := newEnvsTestReconciler(t, srv.URL, w)

	r.applyLlmStreamTimeoutsHot(context.Background(), w,
		v1beta1.WorkerSpec{Runtime: "qwenpaw", LlmStreamIdleTimeout: "60"},
		MemberContext{}, &MemberState{ContainerState: "running"})

	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1 (%+v)", len(*reqs), *reqs)
	}
	if (*reqs)[0].method != http.MethodPatch || (*reqs)[0].path != "/api/envs" {
		t.Fatalf("got %s %s, want PATCH /api/envs", (*reqs)[0].method, (*reqs)[0].path)
	}
	payload := envsPayload(t, (*reqs)[0].body)
	if len(payload) != 1 || payload[envLlmStreamIdleTimeout] != "60" {
		t.Fatalf("payload = %v, want only idle=60", payload)
	}
	if _, ok := payload[envLlmStreamFirstContentTimeout]; ok {
		t.Fatalf("unset first-content key must not be sent: %v", payload)
	}
	var stored v1beta1.Worker
	if err := r.Client.Get(context.Background(), key, &stored); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := stored.Annotations[annotationLlmStreamTimeoutsApplied]; got != "-:60" {
		t.Fatalf("annotation = %q, want %q", got, "-:60")
	}
}

// T4: clearing a previously applied pair (resolved now empty) resets each
// applied key (POST /api/envs/{key}/reset — known registry keys reject
// DELETE) and drops the annotation.
func TestApplyLlmStreamTimeoutsHotClears(t *testing.T) {
	srv, reqs := newEnvsRecordingServer(t, http.StatusOK)
	w := newEnvsTestWorker(map[string]string{annotationLlmStreamTimeoutsApplied: "300:120"})
	r, key := newEnvsTestReconciler(t, srv.URL, w)

	r.applyLlmStreamTimeoutsHot(context.Background(), w,
		v1beta1.WorkerSpec{Runtime: "qwenpaw"}, MemberContext{}, &MemberState{ContainerState: "running"})

	if len(*reqs) != 2 {
		t.Fatalf("requests = %d, want 2 resets (%+v)", len(*reqs), *reqs)
	}
	wantPaths := map[string]bool{
		"/api/envs/" + envLlmStreamFirstContentTimeout + "/reset": false,
		"/api/envs/" + envLlmStreamIdleTimeout + "/reset":         false,
	}
	for _, req := range *reqs {
		if req.method != http.MethodPost {
			t.Fatalf("got method %s, want POST (%+v)", req.method, req)
		}
		if _, ok := wantPaths[req.path]; !ok {
			t.Fatalf("unexpected reset path %q", req.path)
		}
		wantPaths[req.path] = true
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Fatalf("missing reset for %q", p)
		}
	}
	var stored v1beta1.Worker
	if err := r.Client.Get(context.Background(), key, &stored); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if _, ok := stored.Annotations[annotationLlmStreamTimeoutsApplied]; ok {
		t.Fatalf("annotation not cleared: %v", stored.Annotations)
	}
}

// T5: a failed dial (500) leaves the annotation unchanged for the next retry.
func TestApplyLlmStreamTimeoutsHotDialFails(t *testing.T) {
	srv, reqs := newEnvsRecordingServer(t, http.StatusInternalServerError)
	w := newEnvsTestWorker(map[string]string{annotationLlmStreamTimeoutsApplied: "111:222"})
	r, key := newEnvsTestReconciler(t, srv.URL, w)

	r.applyLlmStreamTimeoutsHot(context.Background(), w,
		v1beta1.WorkerSpec{Runtime: "qwenpaw", LlmStreamFirstContentTimeout: "300", LlmStreamIdleTimeout: "120"},
		MemberContext{}, &MemberState{ContainerState: "running"})

	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1 (%+v)", len(*reqs), *reqs)
	}
	var stored v1beta1.Worker
	if err := r.Client.Get(context.Background(), key, &stored); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := stored.Annotations[annotationLlmStreamTimeoutsApplied]; got != "111:222" {
		t.Fatalf("annotation = %q, want unchanged %q", got, "111:222")
	}
}

// T6 + guard: non-qwenpaw runtime (and non-embedded / non-running) make zero
// requests and never write the annotation.
func TestApplyLlmStreamTimeoutsHotSkips(t *testing.T) {
	srv, reqs := newEnvsRecordingServer(t, http.StatusOK)
	spec := v1beta1.WorkerSpec{Runtime: "openclaw", LlmStreamFirstContentTimeout: "300"}

	t.Run("non-qwenpaw runtime", func(t *testing.T) {
		w := newEnvsTestWorker(nil)
		r, key := newEnvsTestReconciler(t, srv.URL, w)
		r.applyLlmStreamTimeoutsHot(context.Background(), w, spec, MemberContext{}, &MemberState{ContainerState: "running"})
		var stored v1beta1.Worker
		if err := r.Client.Get(context.Background(), key, &stored); err != nil {
			t.Fatalf("get worker: %v", err)
		}
		if len(stored.Annotations) != 0 {
			t.Fatalf("annotation written for non-qwenpaw runtime: %v", stored.Annotations)
		}
	})

	t.Run("container not running", func(t *testing.T) {
		w := newEnvsTestWorker(nil)
		r, _ := newEnvsTestReconciler(t, srv.URL, w)
		qspec := v1beta1.WorkerSpec{Runtime: "qwenpaw", LlmStreamFirstContentTimeout: "300"}
		r.applyLlmStreamTimeoutsHot(context.Background(), w, qspec, MemberContext{}, &MemberState{ContainerState: "starting"})
	})

	t.Run("incluster mode", func(t *testing.T) {
		w := newEnvsTestWorker(nil)
		r, _ := newEnvsTestReconciler(t, srv.URL, w)
		r.KubeMode = "incluster"
		qspec := v1beta1.WorkerSpec{Runtime: "qwenpaw", LlmStreamFirstContentTimeout: "300"}
		r.applyLlmStreamTimeoutsHot(context.Background(), w, qspec, MemberContext{}, &MemberState{ContainerState: "running"})
	})

	if len(*reqs) != 0 {
		t.Fatalf("skipped cases made %d requests, want 0 (%+v)", len(*reqs), *reqs)
	}
}
