package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestManagerReadyUpdatesManagerWithoutWorker(t *testing.T) {
	m := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(newLifecycleTestScheme(t)).WithStatusSubresource(m).WithObjects(m).Build()
	h := NewLifecycleHandler(c, nil, "default")
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/managers/default/ready", nil)
		r.SetPathValue("name", "default")
		w := httptest.NewRecorder()
		h.ManagerReady(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("ready: %d %s", w.Code, w.Body.String())
		}
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), m); err != nil {
		t.Fatal(err)
	}
	if !m.Status.Ready {
		t.Fatal("Manager did not become ready")
	}
}

func TestStoppedManagerCannotReportReady(t *testing.T) {
	state := "Stopped"
	m := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1beta1.ManagerSpec{State: &state}}
	c := fake.NewClientBuilder().WithScheme(newLifecycleTestScheme(t)).WithStatusSubresource(m).WithObjects(m).Build()
	h := NewLifecycleHandler(c, nil, "default")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/managers/default/ready", nil)
	r.SetPathValue("name", "default")
	w := httptest.NewRecorder()
	h.ManagerReady(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("ready: %d", w.Code)
	}
}
