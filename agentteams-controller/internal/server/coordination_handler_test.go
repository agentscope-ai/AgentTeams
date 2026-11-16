package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
	"time"
)

func coordinationFixture(t *testing.T) (*CoordinationHandler, *ossfake.Memory) {
	t.Helper()
	leader := workerCR("lead-cr", "lead-runtime")
	leader.Spec.Runtime = "qwenpaw"
	leader.Status.MatrixUserID = "@lead:test"
	worker := workerCR("worker-cr", "worker-runtime")
	worker.Spec.Runtime = "qwenpaw"
	worker.Status.MatrixUserID = "@worker:test"
	tm := teamWithWorkers("alpha", v1beta1.TeamWorkerRef{Name: "lead-cr", Role: "team_leader"}, v1beta1.TeamWorkerRef{Name: "worker-cr", Role: "worker"})
	manager := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: v1beta1.ManagerSpec{Runtime: "qwenpaw"}, Status: v1beta1.ManagerStatus{MatrixUserID: "@manager:test"}}
	k := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(tm, leader, worker, manager).Build()
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha/shared/projects/p1/meta.json", map[string]any{"project_id": "p1", "team_id": "alpha", "status": "active", "room_id": "!task:test", "reply_route": map[string]any{"target_user": "@manager:test"}, "requester_reply_route": map[string]any{"target_user": "@human:test", "target_session": "!source:test"}})
	putProject(store, "teams/alpha/shared/tasks/t1/meta.json", map[string]any{"task_id": "t1", "project_id": "p1", "status": "assigned", "assigned_to": "@worker:test", "notifyTxn": "delegate-1", "submission_id": "submission-1"})
	return NewCoordinationHandler(ServerDeps{Client: k, Namespace: "default", OSS: store, ContainerPrefix: "agentteams-worker-"}), store
}

func TestMatrixAssignmentCarriesMentionsAndCanonicalPrerequisites(t *testing.T) {
	e := coordinationEvent{Kind: "task_assigned", TaskID: "dev", DeliveryID: "d1", Recipient: "@worker:test", Actor: "@lead:test", RoomID: "!task:test"}
	p := map[string]any{"tasks": []any{map[string]any{"task_id": "analysis", "title": "需求分析"}, map[string]any{"task_id": "dev", "title": "开发 <CSV>", "depends_on": []any{"analysis"}}}}
	content := coordinationMatrixAssignment(e, p)
	if !strings.Contains(content["body"].(string), "前置的「需求分析」环节已验收通过") || !strings.Contains(content["formatted_body"].(string), "&lt;CSV&gt;") {
		t.Fatal("missing prerequisite context or unsafe HTML")
	}
	if content["m.mentions"].(map[string]any)["user_ids"].([]string)[0] != "@worker:test" {
		t.Fatal("assignment did not mention its registered Worker")
	}
	if content["org.agentteams.coordination"].(map[string]any)["session_id"] != "coordination-task:dev:d1" {
		t.Fatal("task execution lost its isolated session")
	}
}

func TestMatrixAssignmentTracksAckAndSubmissionWithoutWorkerExecutionHTTP(t *testing.T) {
	h, store := coordinationFixture(t)
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teamharness/coordination/approvals" {
			t.Error("Matrix dispatch called Worker execution API")
		}
		_, _ = w.Write([]byte(`{"approvals":[]}`))
	}))
	defer native.Close()
	h.workerURL = func(string, map[string]string) string { return native.URL }
	e, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	acks := 0
	h.projectEvent = func(context.Context, coordinationEvent, string, string, string, string, string) (string, error) {
		acks++
		return "$ack", nil
	}
	d := coordinationDelivery{Event: e, Transport: "matrix", Status: "running", NativeID: "matrix:$assignment", MatrixEventID: "$assignment"}
	h.advance(context.Background(), &d, func() error { return nil })
	if acks != 0 || d.Status != "running" {
		t.Fatal("send was mistaken for Worker ACK")
	}
	putProject(store, "teams/alpha/shared/tasks/t1/meta.json", map[string]any{"task_id": "t1", "project_id": "p1", "status": "in_progress", "assigned_to": "@worker:test", "notifyTxn": "delegate-1"})
	h.advance(context.Background(), &d, func() error { return nil })
	if acks != 1 || d.Status != "running" || d.AckEventID == "" {
		t.Fatal("ACK did not start execution/approval tracking")
	}
	putProject(store, "teams/alpha/shared/tasks/t1/meta.json", map[string]any{"task_id": "t1", "project_id": "p1", "status": "submitted", "assigned_to": "@worker:test", "notifyTxn": "delegate-1"})
	h.advance(context.Background(), &d, func() error { return nil })
	if acks != 1 || d.Status != "completed" {
		t.Fatal("submission repeated ACK or did not finish tracking")
	}
}

func TestCoordinationReceiptIsReadableOnlyByActorOrActualRecipient(t *testing.T) {
	h, store := coordinationFixture(t)
	e, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(coordinationDelivery{Event: e, Transport: "matrix", Status: "queued"})
	_ = store.PutObject(context.Background(), coordinationPending+e.ID+".json", data)
	for _, tc := range []struct {
		name string
		role string
		want int
	}{{"worker-cr", authpkg.RoleWorker, 200}, {"worker-runtime", authpkg.RoleWorker, 200}, {"lead-cr", authpkg.RoleTeamLeader, 200}, {"lead-cr", authpkg.RoleWorker, 404}, {"human", authpkg.RoleHuman, 404}} {
		req := withCaller(httptest.NewRequest("GET", "/coordination/events/"+e.ID, nil), &authpkg.CallerIdentity{Role: tc.role, Username: tc.name, Team: "alpha"})
		req.SetPathValue("id", e.ID)
		response := httptest.NewRecorder()
		h.GetEvent(response, req)
		if response.Code != tc.want {
			t.Fatalf("%s/%s got %d, want %d", tc.role, tc.name, response.Code, tc.want)
		}
	}
}

func TestMatrixAssignmentKeepsApprovalResolutionsVisibleAfterAck(t *testing.T) {
	h, store := coordinationFixture(t)
	e, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	homeserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var content map[string]any
		_ = json.NewDecoder(r.Body).Decode(&content)
		if content["org.agentteams.progress_only"] != true {
			t.Error("approval update could wake an Agent")
		}
		statuses = append(statuses, coordinationString(coordinationMap(content, "org.agentteams.approval"), "status"))
		_, _ = w.Write([]byte(`{"event_id":"$approval"}`))
	}))
	defer homeserver.Close()
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	_ = creds.Save(context.Background(), "worker-cr", &service.WorkerCredentials{MatrixToken: "test-token"})
	h.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Creds: creds})
	h.deps.MatrixConfig = matrix.Config{ServerURL: homeserver.URL}
	status := "pending"
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teamharness/coordination/approvals" {
			t.Error("unexpected Worker execution call")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"approvals": []any{map[string]any{"request_id": "approval-1", "status": status, "session_id": coordinationSession(e), "tool_name": "Bash"}}})
	}))
	defer worker.Close()
	h.workerURL = func(string, map[string]string) string { return worker.URL }
	putProject(store, "teams/alpha/shared/tasks/t1/meta.json", map[string]any{"task_id": "t1", "project_id": "p1", "status": "in_progress", "assigned_to": "@worker:test", "notifyTxn": "delegate-1"})
	d := coordinationDelivery{Event: e, Transport: "matrix", Status: "running", NativeID: "matrix:$assignment", AckEventID: "$ack"}
	h.advance(context.Background(), &d, func() error { return nil })
	status = "approved"
	h.advance(context.Background(), &d, func() error { return nil })
	h.advance(context.Background(), &d, func() error { return nil })
	if strings.Join(statuses, ",") != "pending,approved" || d.Status != "running" {
		t.Fatalf("approval lifecycle lost after ACK: %v, %s", statuses, d.Status)
	}
}

func TestCoordinationRoutesFromCanonicalTask(t *testing.T) {
	h, _ := coordinationFixture(t)
	e := coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1", Recipient: "http://untrusted", Text: "Execute the bounded spec."}
	got, err := h.resolve(context.Background(), e, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Recipient != "@worker:test" || got.TargetURL != "http://agentteams-worker-worker-runtime:8088" {
		t.Fatalf("wrong route: %s %s", got.Recipient, got.TargetURL)
	}
	if got.RoomID != "!task:test" {
		t.Fatalf("wrong room %s", got.RoomID)
	}
}

func TestCoordinationActorUsesRegisteredCredentialName(t *testing.T) {
	h, _ := coordinationFixture(t)
	e, err := h.resolve(context.Background(), coordinationEvent{Kind: "handoff", Recipient: "@lead:test", OriginEventID: "source-event", SourceRoute: map[string]any{"target_session": "!source:test", "target_user": "@human:test"}}, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	if err != nil {
		t.Fatal(err)
	}
	if e.ActorName != "default" {
		t.Fatalf("credential lookup used alias %s", e.ActorName)
	}
	e, err = h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-runtime", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if e.ActorName != "lead-cr" {
		t.Fatalf("credential lookup used runtime name %s", e.ActorName)
	}
}

func TestCoordinationRecoveryRetainsCanonicalProjectionIdentity(t *testing.T) {
	h, _ := coordinationFixture(t)
	native := httptest.NewServer(http.NotFoundHandler())
	defer native.Close()
	h.managerURL = func(string) string { return native.URL }
	h.projectEvent = func(context.Context, coordinationEvent, string, string, string, string, string) (string, error) {
		return "receipt", nil
	}
	e, err := h.resolve(context.Background(), coordinationEvent{Kind: "handoff", Recipient: "@lead:test", OriginEventID: "old", SourceRoute: map[string]any{"target_session": "!source:test", "target_user": "@human:test"}}, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	if err != nil {
		t.Fatal(err)
	}
	h.workerURL = func(string, map[string]string) string { return native.URL }
	e.ActorName = "manager"
	d := coordinationDelivery{Event: e, Status: "running", NativeID: "lost"}
	h.advance(context.Background(), &d, func() error { return nil })
	if d.Event.ActorName != "default" {
		t.Fatal("failure projection retained an obsolete actor alias")
	}
}

func TestCoordinationMissingNativeReceiptFailsWithoutReplay(t *testing.T) {
	h, _ := coordinationFixture(t)
	posts := 0
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer native.Close()
	h.workerURL = func(string, map[string]string) string { return native.URL }
	e, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	h.projectEvent = func(context.Context, coordinationEvent, string, string, string, string, string) (string, error) {
		return "matrix-receipt", nil
	}
	d := coordinationDelivery{Event: e, Status: "running", NativeID: "lost-receipt"}
	h.advance(context.Background(), &d, func() error { return nil })
	if d.Status != "failed" || !strings.Contains(d.Error, "receipt") || posts != 0 {
		t.Fatalf("missing receipt was not terminal: status=%s posts=%d error=%s", d.Status, posts, d.Error)
	}
}

func TestCoordinationRejectsUnsafeEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		event  coordinationEvent
		caller *authpkg.CallerIdentity
	}{
		{"wrong worker", coordinationEvent{Kind: "task_submitted", ProjectID: "p1", TaskID: "t1", SubmissionID: "submission-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "other", Team: "alpha"}},
		{"stale delegation", coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "old"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}},
		{"human", coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "admin", Team: "alpha"}},
		{"cross team", coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := coordinationFixture(t)
			if _, err := h.resolve(context.Background(), tc.event, tc.caller); err == nil {
				t.Fatal("unsafe event accepted")
			}
		})
	}
}

func TestCoordinationFinalUsesSavedRequester(t *testing.T) {
	h, s := coordinationFixture(t)
	key := "teams/alpha/shared/projects/p1/meta.json"
	b, _ := s.GetObject(context.Background(), key)
	var p map[string]any
	_ = json.Unmarshal(b, &p)
	p["status"] = "completed"
	putProject(s, key, p)
	s.PutObject(context.Background(), "teams/alpha/shared/projects/p1/result.md", []byte("Accepted canonical report"))
	got, err := h.resolve(context.Background(), coordinationEvent{Kind: "project_completed", ProjectID: "p1", Text: "Caller report", Recipient: "@other:test", RoomID: "!wrong:test"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Recipient != "@manager:test" || got.RequesterRoute["target_session"] != "!source:test" {
		t.Fatal("did not preserve requester route")
	}
	if got.FinalReport != "Accepted canonical report" {
		t.Fatal("final notification did not load the saved report")
	}
	meta := coordinationPayload(got)["channel_meta"].(map[string]any)
	if meta["teamharness_final_report"] != got.FinalReport {
		t.Fatal("canonical report missing from native notification")
	}
	if got.TargetURL != "http://agentteams-manager:18799" {
		t.Fatalf("manager url %s", got.TargetURL)
	}
}

func TestCoordinationClosureRequiresReadableExecutionEvidence(t *testing.T) {
	h, store := coordinationFixture(t)
	projectKey := "teams/alpha/shared/projects/p1/meta.json"
	putProject(store, projectKey, map[string]any{"project_id": "p1", "team_id": "alpha", "status": "active", "tasks": []any{
		map[string]any{"task_id": "main", "status": "planned"},
		map[string]any{"task_id": "missing-leaf", "parent_task_id": "main", "status": "completed"},
	}})
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
	event := coordinationEvent{Kind: "project_ready", ProjectID: "p1"}
	if _, err := h.resolve(context.Background(), event, caller); err == nil {
		t.Fatal("closure admitted without the accepted leaf's evidence")
	}
	putProject(store, "teams/alpha/shared/tasks/missing-leaf/meta.json", map[string]any{"summary": "accepted output"})
	if _, err := h.resolve(context.Background(), event, caller); err != nil {
		t.Fatalf("coordinating parent need not have an execution record: %v", err)
	}
}

func TestCoordinationDurableAdmissionRestartAndDuplicate(t *testing.T) {
	h, s := coordinationFixture(t)
	calls := 0
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			calls++
			putProject(s, "teams/alpha/shared/tasks/t1/meta.json", map[string]any{"task_id": "t1", "project_id": "p1", "status": "submitted", "assigned_to": "@worker:test", "notifyTxn": "delegate-1"})
			_, _ = w.Write([]byte(`{"task_id":"job-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"finished","result":{"status":"completed","output":[]}}`))
	}))
	defer native.Close()
	h.workerURL = func(string, map[string]string) string { return native.URL }
	event := coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1", Text: "Execute."}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
	admit := func(event coordinationEvent) int {
		data, _ := json.Marshal(event)
		req := withCaller(httptest.NewRequest("POST", "/api/v1/coordination/events", bytes.NewReader(data)), caller)
		w := httptest.NewRecorder()
		h.PostEvent(w, req)
		return w.Code
	}
	if got := admit(event); got != 202 {
		t.Fatalf("admit %d", got)
	}
	if got := admit(event); got != 202 {
		t.Fatalf("retry %d", got)
	}
	h.drain(context.Background())
	if calls != 1 {
		t.Fatalf("admissions %d", calls)
	}
	resolved, err := h.resolve(context.Background(), event, caller)
	if err != nil {
		t.Fatal(err)
	}
	key := coordinationPending + resolved.ID + ".json"
	data, _ := s.GetObject(context.Background(), key)
	var delivery coordinationDelivery
	_ = json.Unmarshal(data, &delivery)
	if delivery.NativeID != "job-1" {
		t.Fatal("native receipt was not durable")
	}
	// A new handler resumes the existing native run, never admits it again.
	resumed, _ := coordinationFixture(t)
	resumed.deps.OSS = s
	resumed.workerURL = h.workerURL
	resumed.projectEvent = func(context.Context, coordinationEvent, string, string, string, string, string) (string, error) {
		return "$projected", nil
	}
	delivery.NextAttempt = time.Time{}
	data, _ = json.Marshal(delivery)
	_ = s.PutObject(context.Background(), key, data)
	resumed.drain(context.Background())
	if calls != 1 {
		t.Fatal("restart admitted a duplicate execution")
	}
	archived, err := s.GetObject(context.Background(), coordinationDone+resolved.ID+".json")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(archived, &delivery)
	if delivery.Status != "completed" {
		t.Fatalf("receipt %s %s", delivery.Status, delivery.Error)
	}
	if got := admit(event); got != 202 {
		t.Fatalf("settled retry %d", got)
	}
	event.Text = "Replace the original payload"
	if got := admit(event); got != 409 {
		t.Fatalf("changed payload %d", got)
	}
}

func TestCoordinationUnknownAdmissionNeverReplays(t *testing.T) {
	h, s := coordinationFixture(t)
	h.projectEvent = func(context.Context, coordinationEvent, string, string, string, string, string) (string, error) {
		return "$failure", nil
	}
	event, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	d := coordinationDelivery{Event: event, Status: "dispatching", Created: time.Now()}
	data, _ := json.Marshal(d)
	_ = s.PutObject(context.Background(), coordinationPending+event.ID+".json", data)
	h.workerURL = func(string, map[string]string) string { t.Fatal("ambiguous admission was replayed"); return "" }
	h.drain(context.Background())
	data, err = s.GetObject(context.Background(), coordinationDone+event.ID+".json")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(data, &d)
	if d.Status != "failed" || d.FailureEventID != "$failure" {
		t.Fatal("ambiguous admission was not surfaced")
	}
}

func TestCoordinationFailureProjectionRetriesWithoutExecutingAgent(t *testing.T) {
	h, store := coordinationFixture(t)
	event, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	key := coordinationPending + event.ID + ".json"
	d := coordinationDelivery{Event: event, Status: "failed", Error: "native admission outcome unknown; operator recovery required", Created: time.Now()}
	initial, _ := json.Marshal(d)
	_ = store.PutObject(context.Background(), key, initial)
	h.workerURL = func(string, map[string]string) string { t.Fatal("failed delivery replayed"); return "" }
	projected := 0
	h.projectEvent = func(_ context.Context, _ coordinationEvent, _, _, room, txn, text string) (string, error) {
		projected++
		if room != "!task:test" || txn != event.ID+"-failure" || !strings.Contains(text, event.ID) {
			t.Fatal("failure projection lost its scope or identity")
		}
		if projected == 1 {
			return "", fmt.Errorf("Matrix temporarily unavailable")
		}
		return "$failure", nil
	}
	h.drain(context.Background())
	data, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatal("unprojected failure was prematurely archived")
	}
	_ = json.Unmarshal(data, &d)
	d.NextAttempt = time.Time{}
	data, _ = json.Marshal(d)
	_ = store.PutObject(context.Background(), key, data)
	h.drain(context.Background())
	data, err = store.GetObject(context.Background(), coordinationDone+event.ID+".json")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(data, &d)
	if d.Status != "failed" || d.FailureEventID != "$failure" || projected != 2 {
		t.Fatal("failure projection did not recover")
	}
}

func TestCoordinationReadyRequiresAllExecutionLeaves(t *testing.T) {
	for _, status := range []string{"planned", "submitted", "completed"} {
		t.Run(status, func(t *testing.T) {
			h, s := coordinationFixture(t)
			p := map[string]any{"project_id": "p1", "team_id": "alpha", "status": "active", "plan_type": "dag", "room_id": "!task:test", "tasks": []any{map[string]any{"task_id": "parent", "status": "planned"}, map[string]any{"task_id": "t1", "parent_task_id": "parent", "status": status}}}
			putProject(s, "teams/alpha/shared/projects/p1/meta.json", p)
			got, err := h.resolve(context.Background(), coordinationEvent{Kind: "project_ready", ProjectID: "p1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
			if status != "completed" {
				if err == nil {
					t.Fatal("unaccepted leaf allowed closure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Recipient != "@lead:test" || coordinationSession(got) != "coordination-close:p1" {
				t.Fatal("closure was not routed to the registered Leader")
			}
			if !strings.Contains(got.Text, "accepted_results") {
				t.Fatal("closure lacked accepted result context")
			}
		})
	}
}

func TestCoordinationPausedDeliveryDoesNotExpireByWallClock(t *testing.T) {
	h, store := coordinationFixture(t)
	projectKey := "teams/alpha/shared/projects/p1/meta.json"
	p, _ := h.read(context.Background(), projectKey)
	p["status"] = "paused"
	putProject(store, projectKey, p)
	event, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	d := coordinationDelivery{Event: event, Status: "queued", Created: time.Now().Add(-24 * time.Hour)}
	data, _ := json.Marshal(d)
	key := coordinationPending + event.ID + ".json"
	_ = store.PutObject(context.Background(), key, data)
	h.projectEvent = func(context.Context, coordinationEvent, string, string, string, string, string) (string, error) {
		t.Error("paused delivery was reported as a failure")
		return "$unexpected", nil
	}
	h.drain(context.Background())
	data, err = store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatal("paused delivery disappeared from the durable queue")
	}
	_ = json.Unmarshal(data, &d)
	if d.Status != "queued" || d.NativeID != "" || d.Error != "" {
		t.Fatalf("paused receipt: %+v", d)
	}
}

func TestCoordinationUsesRuntimeExecutionBudget(t *testing.T) {
	if _, forced := coordinationPayload(coordinationEvent{Kind: "task_assigned"})["timeout"]; forced {
		t.Fatal("transport imposed a short execution deadline instead of the runtime budget")
	}
}

func TestCoordinationPlanHintsUseCanonicalState(t *testing.T) {
	h, s := coordinationFixture(t)
	key := "teams/alpha/shared/projects/p1/meta.json"
	b, _ := s.GetObject(context.Background(), key)
	var p map[string]any
	_ = json.Unmarshal(b, &p)
	p["plan_type"] = "loop"
	p["dispatch_mode"] = "automatic"
	putProject(s, key, p)
	e, err := h.resolve(context.Background(), coordinationEvent{Kind: "task_assigned", ProjectID: "p1", TaskID: "t1", DeliveryID: "delegate-1", PlanType: "dag", DispatchMode: "manual"}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	meta := coordinationPayload(e)["channel_meta"].(map[string]any)["teamharness_coordination"].(map[string]any)
	if meta["plan_type"] != "loop" || meta["dispatch_mode"] != "automatic" {
		t.Fatal("plan hints did not use canonical state")
	}
	e.Kind = "task_submitted"
	input := coordinationPayload(e)["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(input, "manual DAG or Loop") {
		t.Fatal("Loop received automatic finite continuation instructions")
	}
}
