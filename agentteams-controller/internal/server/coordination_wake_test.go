package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

type coordinationScanStore struct {
	*ossfake.Memory
	scanned chan struct{}
}

func TestCoordinationAssignmentCommitWakesPreparedReceipt(t *testing.T) {
	t.Setenv("AGENTTEAMS_COORDINATION_TRANSPORT", "matrix")
	h, memory := coordinationFixture(t)
	key := "teams/alpha/shared/tasks/t1/meta.json"
	task := map[string]any{"task_id": "t1", "project_id": "p1", "status": "prepared", "assigned_to": "@worker:test", "notifyTxn": "delegate-1"}
	putProject(memory, key, task)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
	event, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(coordinationDelivery{Event: event, Transport: "matrix", Status: "queued"})
	_ = memory.PutObject(context.Background(), coordinationPending+event.ID+".json", data)
	h.step(context.Background(), coordinationPending+event.ID+".json")
	data, _ = memory.GetObject(context.Background(), coordinationPending+event.ID+".json")
	var receipt coordinationDelivery
	_ = json.Unmarshal(data, &receipt)
	if receipt.Status != "queued" || !receipt.NextAttempt.After(time.Now()) {
		t.Fatal("prepared assignment was dispatched")
	}
	sent := make(chan struct{}, 1)
	homeserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, _ := memory.GetObject(r.Context(), key)
		var committed map[string]any
		_ = json.Unmarshal(current, &committed)
		if committed["status"] != "assigned" {
			t.Error("dispatched before assignment commit")
		}
		sent <- struct{}{}
		_, _ = w.Write([]byte(`{"event_id":"$assignment"}`))
	}))
	defer homeserver.Close()
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	_ = creds.Save(context.Background(), "lead-cr", &service.WorkerCredentials{MatrixToken: "test-token"})
	h.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Creds: creds})
	h.deps.MatrixConfig = matrix.Config{ServerURL: homeserver.URL}
	store := &coordinationScanStore{Memory: memory, scanned: make(chan struct{}, 1)}
	h.deps.OSS = store
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-store.scanned:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	projectHandler := NewProjectHandler(h.deps.Client, "default", store)
	projectHandler.taskAssigned = h.wakeRunner
	task["status"] = "assigned"
	task["eventId"] = "api-" + event.ID
	response := putTaskMetaRequest(t, projectHandler, "t1", taskMetaETag(t, memory, key), taskMetaBody(task), caller)
	if response.Code != 200 {
		t.Fatalf("commit: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-sent:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("committed assignment waited for recovery polling")
	}
}

func TestCoordinationAssignmentWakePreservesLeaseAndErrorBackoff(t *testing.T) {
	for _, guard := range []string{"lease", "delivery error"} {
		t.Run(guard, func(t *testing.T) {
			h, store := coordinationFixture(t)
			event, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
			if err != nil {
				t.Fatal(err)
			}
			delivery := coordinationDelivery{Event: event, Transport: "matrix", Status: "queued", NextAttempt: time.Now().Add(time.Minute)}
			if guard == "lease" {
				delivery.LeaseUntil = time.Now().Add(time.Minute)
			} else {
				delivery.Error = "Matrix coordination notification unavailable"
			}
			data, _ := json.Marshal(delivery)
			key := coordinationPending + event.ID + ".json"
			_ = store.PutObject(context.Background(), key, data)
			h.step(context.Background(), key)
			got, _ := store.GetObject(context.Background(), key)
			if !bytes.Equal(got, data) {
				t.Fatal("wake bypassed an active lease or error backoff")
			}
		})
	}
}

func (s *coordinationScanStore) ListObjectsRecursive(ctx context.Context, prefix, cursor string, limit int) ([]oss.ObjectInfo, string, error) {
	entries, next, err := s.Memory.ListObjectsRecursive(ctx, prefix, cursor, limit)
	if prefix == coordinationPending {
		select {
		case s.scanned <- struct{}{}:
		default:
		}
	}
	return entries, next, err
}

func TestCoordinationAdmissionWakesIdleRunnerWithoutPollingDelay(t *testing.T) {
	t.Setenv("AGENTTEAMS_COORDINATION_TRANSPORT", "matrix")
	h, memory := coordinationFixture(t)
	projectKey := "teams/alpha/shared/projects/p1/meta.json"
	data, _ := memory.GetObject(context.Background(), projectKey)
	var project map[string]any
	_ = json.Unmarshal(data, &project)
	project["status"] = "completed"
	putProject(memory, projectKey, project)
	_ = memory.PutObject(context.Background(), "teams/alpha/shared/projects/p1/result.md", []byte("Accepted final report"))
	store := &coordinationScanStore{Memory: memory, scanned: make(chan struct{}, 1)}
	h.deps.OSS = store
	var calls atomic.Int32
	sent := make(chan struct{}, 2)
	h.projectEvent = func(_ context.Context, _ coordinationEvent, _, _, _, _, _ string) (string, error) {
		calls.Add(1)
		sent <- struct{}{}
		return "$final", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-store.scanned: // The initial empty scan cannot see the later admission.
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	event := coordinationEvent{Kind: "project_completed", ProjectID: "p1"}
	body, _ := json.Marshal(event)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
	for range 2 { // An admission retry must still project just one final report.
		req := withCaller(httptest.NewRequest("POST", "/api/v1/coordination/events", bytes.NewReader(body)), caller)
		response := httptest.NewRecorder()
		h.PostEvent(response, req)
		if response.Code != 202 {
			t.Fatalf("admission: %d %s", response.Code, response.Body.String())
		}
	}
	select {
	case <-sent:
	case <-time.After(1500 * time.Millisecond): // Recovery polling is two seconds.
		t.Fatal("durable admission waited for the recovery poll")
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate projection: %d", calls.Load())
	}
}
