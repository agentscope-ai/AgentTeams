package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

func TestStatusPusherPushOnce(t *testing.T) {
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "w1",
			Namespace:   "default",
			Annotations: map[string]string{provisionGenerationAnnotation: "3"},
		},
		Status: v1beta1.WorkerStatus{
			Phase:          "Running",
			ContainerState: "running",
			MatrixUserID:   "@w1:matrix.local",
			RoomID:         "!r:matrix.local",
			AgentStatus:    "idle",
			LastRunAt:      "2026-11-13T00:00:00Z",
		},
	}
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()

	var gotBody []byte
	var gotHeaders http.Header
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"updated":1,"discarded":0}`))
	}))
	defer svc.Close()

	fixed := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	p := NewStatusPusher(k8s, "default", svc.URL, "internal-token")
	p.Now = func() time.Time { return fixed }
	if err := p.PushOnce(context.Background()); err != nil {
		t.Fatalf("PushOnce: %v", err)
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer internal-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := gotHeaders.Get(internalTimestampHeader); got != fixed.Format(time.RFC3339) {
		t.Fatalf("timestamp = %q, want %q", got, fixed.Format(time.RFC3339))
	}
	if gotHeaders.Get(internalRequestIDHeader) == "" {
		t.Fatalf("request id missing")
	}
	var batch statusBatch
	if err := json.Unmarshal(gotBody, &batch); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	if len(batch.Workers) != 1 {
		t.Fatalf("workers = %d, want 1", len(batch.Workers))
	}
	item := batch.Workers[0]
	if item.Name != "w1" || item.Generation != 3 || item.Phase != "Running" || item.MatrixUserID == "" || item.LastRunAt == "" {
		t.Fatalf("item = %+v", item)
	}
}

func TestStatusPusherEmptyAndErrors(t *testing.T) {
	scheme := newProjectTestScheme(t)

	var calls int32
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer svc.Close()

	// Empty namespace: no request at all.
	empty := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewStatusPusher(empty, "default", svc.URL, "tok")
	if err := p.PushOnce(context.Background()); err != nil {
		t.Fatalf("empty PushOnce: %v", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("empty list still posted a batch")
	}

	// A non-2xx response is an error (the caller logs it; nothing else).
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "w1", Namespace: "default"}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	p = NewStatusPusher(k8s, "default", svc.URL, "tok")
	if err := p.PushOnce(context.Background()); err == nil {
		t.Fatalf("500 response did not surface an error")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}
