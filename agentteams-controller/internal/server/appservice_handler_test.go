package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newAppserviceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return scheme
}

func txnBody(t *testing.T, events []matrixEvent) *bytes.Buffer {
	t.Helper()
	body := transactionBody{Events: events}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewBuffer(b)
}

func mentionEvent(roomID, eventID, sender string, userIDs []string) matrixEvent {
	ev := matrixEvent{
		Type:    "m.room.message",
		RoomID:  roomID,
		EventID: eventID,
		Sender:  sender,
	}
	ev.Content.Mentions = &struct {
		UserIDs []string `json:"user_ids"`
	}{UserIDs: userIDs}
	return ev
}

func TestNewHTTPServerRegistersAppserviceTransactionRoute(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newAppserviceTestScheme(t)).Build()
	pushURL := "http://controller.example.com:8090"
	reg := matrix.RenderAppServiceRegistration(matrix.Config{
		AppServiceID:              "agentteams-controller",
		AppServiceToken:           "as-token",
		AppServiceHSToken:         "correct-token",
		AppServiceSenderLocalpart: "agentteams-controller",
		AppServicePushURL:         pushURL,
	})
	if reg.URL == nil {
		t.Fatal("registration URL is nil")
	}
	srv := NewHTTPServer(":0", ServerDeps{
		Client:       k8s,
		Namespace:    "default",
		AuthMw:       authpkg.NewMiddleware(nil, nil, nil, nil, ""),
		MatrixConfig: matrix.Config{AppServiceEnabled: true, AppServiceHSToken: "correct-token"},
	})

	req := httptest.NewRequest(http.MethodPut, *reg.URL+"/_matrix/app/v1/transactions/txn-from-mux", txnBody(t, nil))
	req.Header.Set("Authorization", "Bearer correct-token")
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200 from mounted AppService transaction route", rec.Code, rec.Body.String())
	}
}

// --- Standalone Worker Tests ---

func TestAppserviceWakesStandaloneWorkerFromOwnRoom(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	sleeping := "Sleeping"
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{State: &sleeping},
		Status: v1beta1.WorkerStatus{
			MatrixUserID: "@alpha-dev:example.com",
			RoomID:       "!worker-dm:example.com",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()

	handler := NewAppserviceHandler("test-hs-token", k8sClient, "default")
	handler.now = func() time.Time {
		t, _ := time.Parse(time.RFC3339, "2026-05-12T10:20:00Z")
		return t
	}

	body := txnBody(t, []matrixEvent{
		mentionEvent("!worker-dm:example.com", "$ev1", "@human:example.com", []string{"@alpha-dev:example.com"}),
	})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn1", body)
	req.Header.Set("Authorization", "Bearer test-hs-token")
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Spec.DesiredState() != "Running" {
		t.Fatalf("state=%q, want Running", updated.Spec.DesiredState())
	}
	if updated.Status.LastActiveAt != "2026-05-12T10:20:00Z" {
		t.Fatalf("lastActiveAt=%q, want updated", updated.Status.LastActiveAt)
	}
}

func TestAppserviceRejectsStandaloneFromWrongRoom(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	sleeping := "Sleeping"
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{State: &sleeping},
		Status: v1beta1.WorkerStatus{
			MatrixUserID: "@alpha-dev:example.com",
			RoomID:       "!worker-dm:example.com",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()

	handler := NewAppserviceHandler("test-hs-token", k8sClient, "default")

	body := txnBody(t, []matrixEvent{
		mentionEvent("!other-room:example.com", "$ev2", "@human:example.com", []string{"@alpha-dev:example.com"}),
	})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn2", body)
	req.Header.Set("Authorization", "Bearer test-hs-token")
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	// Should NOT have been woken — still Sleeping.
	if updated.Spec.DesiredState() != "Sleeping" {
		t.Fatalf("state=%q, want Sleeping (wrong room should be rejected)", updated.Spec.DesiredState())
	}
}

// --- Team Worker Tests ---

func TestAppserviceWakesTeamWorkerFromDMRoom(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	sleeping := "Sleeping"
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{State: &sleeping},
		Status: v1beta1.WorkerStatus{
			MatrixUserID: "@dev:example.com",
			RoomID:       "!dev-dm:example.com",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev", Role: "worker"},
			},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID: "!team-room:example.com",
			Members: []v1beta1.TeamMemberStatus{{
				Name:         "dev",
				Role:         "worker",
				MatrixUserID: "@dev:example.com",
				RoomID:       "!dev-dm:example.com",
			}},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}, &v1beta1.Team{}).
		WithObjects(worker, team).
		Build()

	handler := NewAppserviceHandler("test-hs-token", k8sClient, "default")
	handler.now = func() time.Time {
		t, _ := time.Parse(time.RFC3339, "2026-05-12T10:20:00Z")
		return t
	}

	// Mention from worker's DM room — should also be allowed.
	body := txnBody(t, []matrixEvent{
		mentionEvent("!dev-dm:example.com", "$ev4", "@human:example.com", []string{"@dev:example.com"}),
	})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn4", body)
	req.Header.Set("Authorization", "Bearer test-hs-token")
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Spec.DesiredState() != "Running" {
		t.Fatalf("state=%q, want Running", updated.Spec.DesiredState())
	}
}

func TestAppserviceWakesDecoupledTeamWorkerFromTeamRoom(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	sleeping := "Sleeping"
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{State: &sleeping},
		Status: v1beta1.WorkerStatus{
			MatrixUserID: "@dev:example.com",
			RoomID:       "!dev-dm:example.com",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev", Role: "worker"},
			},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID: "!team-room:example.com",
			Members: []v1beta1.TeamMemberStatus{{
				Name:         "dev",
				Role:         "worker",
				MatrixUserID: "@dev:example.com",
				RoomID:       "!dev-dm:example.com",
			}},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}, &v1beta1.Team{}).
		WithObjects(worker, team).
		Build()

	handler := NewAppserviceHandler("test-hs-token", k8sClient, "default")
	handler.now = func() time.Time {
		t, _ := time.Parse(time.RFC3339, "2026-05-12T10:20:00Z")
		return t
	}

	body := txnBody(t, []matrixEvent{
		mentionEvent("!team-room:example.com", "$ev-decoupled", "@human:example.com", []string{"@dev:example.com"}),
	})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn-decoupled", body)
	req.Header.Set("Authorization", "Bearer test-hs-token")
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Spec.DesiredState() != "Running" {
		t.Fatalf("state=%q, want Running", updated.Spec.DesiredState())
	}
	if updated.Status.LastActiveAt != "2026-05-12T10:20:00Z" {
		t.Fatalf("lastActiveAt=%q, want updated", updated.Status.LastActiveAt)
	}
}

func TestAppserviceRejectsCrossTeamMention(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	sleeping := "Sleeping"
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{State: &sleeping},
		Status: v1beta1.WorkerStatus{
			MatrixUserID: "@dev:example.com",
			RoomID:       "!dev-dm:example.com",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev", Role: "worker"},
			},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID: "!team-room:example.com",
			Members: []v1beta1.TeamMemberStatus{{
				Name:         "dev",
				Role:         "worker",
				MatrixUserID: "@dev:example.com",
				RoomID:       "!dev-dm:example.com",
			}},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}, &v1beta1.Team{}).
		WithObjects(worker, team).
		Build()

	handler := NewAppserviceHandler("test-hs-token", k8sClient, "default")

	// Mention from a DIFFERENT team's room — should be rejected.
	body := txnBody(t, []matrixEvent{
		mentionEvent("!other-team-room:example.com", "$ev5", "@human:example.com", []string{"@dev:example.com"}),
	})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn5", body)
	req.Header.Set("Authorization", "Bearer test-hs-token")
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	// Should NOT have been woken — cross-team mention rejected.
	if updated.Spec.DesiredState() != "Sleeping" {
		t.Fatalf("state=%q, want Sleeping (cross-team should be rejected)", updated.Spec.DesiredState())
	}
}

// --- Auth Tests ---

func TestAppserviceRejectsInvalidHSToken(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewAppserviceHandler("correct-token", k8sClient, "default")

	body := txnBody(t, []matrixEvent{
		mentionEvent("!room:example.com", "$ev", "@sender:example.com", []string{"@target:example.com"}),
	})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn", body)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestAppserviceAcceptsQueryParamToken(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewAppserviceHandler("correct-token", k8sClient, "default")

	body := txnBody(t, []matrixEvent{})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn?access_token=correct-token", body)
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with query param token, got %d", rec.Code)
	}
}

// --- Dedup Tests ---

func TestAppserviceDeduplicatesEvents(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	sleeping := "Sleeping"
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{State: &sleeping},
		Status: v1beta1.WorkerStatus{
			MatrixUserID: "@alpha-dev:example.com",
			RoomID:       "!worker-dm:example.com",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()

	handler := NewAppserviceHandler("test-hs-token", k8sClient, "default")
	handler.now = func() time.Time {
		t, _ := time.Parse(time.RFC3339, "2026-05-12T10:20:00Z")
		return t
	}

	// First request — should wake the worker.
	body1 := txnBody(t, []matrixEvent{
		mentionEvent("!worker-dm:example.com", "$same-event", "@human:example.com", []string{"@alpha-dev:example.com"}),
	})
	req1 := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn1", body1)
	req1.Header.Set("Authorization", "Bearer test-hs-token")
	rec1 := httptest.NewRecorder()
	handler.HandleTransactions(rec1, req1)

	var after1 v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &after1); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if after1.Spec.DesiredState() != "Running" {
		t.Fatalf("state=%q after first, want Running", after1.Spec.DesiredState())
	}

	// Reset worker back to Sleeping to detect if second request processes.
	after1.Spec.State = &sleeping
	if err := k8sClient.Update(context.Background(), &after1); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Second request with SAME event — should be deduped, worker stays Sleeping.
	body2 := txnBody(t, []matrixEvent{
		mentionEvent("!worker-dm:example.com", "$same-event", "@human:example.com", []string{"@alpha-dev:example.com"}),
	})
	req2 := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn2", body2)
	req2.Header.Set("Authorization", "Bearer test-hs-token")
	rec2 := httptest.NewRecorder()
	handler.HandleTransactions(rec2, req2)

	var after2 v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &after2); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if after2.Spec.DesiredState() != "Sleeping" {
		t.Fatalf("state=%q after dedup, want Sleeping (should have been skipped)", after2.Spec.DesiredState())
	}
}

// --- Non-Sleeping Ignore Tests ---

func TestAppserviceIgnoresRunningWorker(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	// Worker is Running — should NOT be modified.
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		// State nil → defaults to Running.
		Status: v1beta1.WorkerStatus{
			MatrixUserID: "@alpha-dev:example.com",
			RoomID:       "!worker-dm:example.com",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()

	handler := NewAppserviceHandler("test-hs-token", k8sClient, "default")

	body := txnBody(t, []matrixEvent{
		mentionEvent("!worker-dm:example.com", "$ev", "@human:example.com", []string{"@alpha-dev:example.com"}),
	})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn", body)
	req.Header.Set("Authorization", "Bearer test-hs-token")
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	// State should remain Running (nil).
	if updated.Spec.DesiredState() != "Running" {
		t.Fatalf("state=%q, want Running (should be unchanged)", updated.Spec.DesiredState())
	}
}

// --- Non-message Event Filtering ---

func TestAppserviceIgnoresNonMessageEvents(t *testing.T) {
	scheme := newAppserviceTestScheme(t)
	sleeping := "Sleeping"
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{State: &sleeping},
		Status: v1beta1.WorkerStatus{
			MatrixUserID: "@alpha-dev:example.com",
			RoomID:       "!worker-dm:example.com",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()

	handler := NewAppserviceHandler("test-hs-token", k8sClient, "default")

	// Send a non-message event type — should be ignored.
	ev := matrixEvent{
		Type:    "m.room.member",
		RoomID:  "!worker-dm:example.com",
		EventID: "$ev",
		Sender:  "@human:example.com",
	}
	body := txnBody(t, []matrixEvent{ev})
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn", body)
	req.Header.Set("Authorization", "Bearer test-hs-token")
	rec := httptest.NewRecorder()

	handler.HandleTransactions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Spec.DesiredState() != "Sleeping" {
		t.Fatalf("state=%q, want Sleeping (non-message events should be ignored)", updated.Spec.DesiredState())
	}
}
