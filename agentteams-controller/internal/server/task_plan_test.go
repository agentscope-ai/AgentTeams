package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type planTestMatrix struct {
	matrix.Client
	rooms        map[string]matrix.CreateRoomRequest
	creates      int
	lostResponse bool
	foreignRoom  bool
	joined       map[string]bool
	joinBlocked  string
}

func (m *planTestMatrix) CreateRoom(_ context.Context, request matrix.CreateRoomRequest) (*matrix.RoomInfo, error) {
	_, exists := m.rooms[request.RoomAliasName]
	if !exists {
		m.creates++
		m.rooms[request.RoomAliasName] = request
	}
	if m.lostResponse {
		m.lostResponse = false
		return nil, errors.New("injected lost create response")
	}
	return &matrix.RoomInfo{RoomID: "!planned:test", Created: !exists}, nil
}
func (m *planTestMatrix) GetRoomState(context.Context, string, string, string, string) (map[string]any, error) {
	if m.foreignRoom {
		return map[string]any{"taskId": "foreign"}, nil
	}
	for _, room := range m.rooms {
		return room.InitialState[0].Content, nil
	}
	return nil, errors.New("room missing")
}
func (m *planTestMatrix) ListRoomMembersWithToken(context.Context, string, string) ([]matrix.RoomMember, error) {
	members := []matrix.RoomMember{}
	for _, room := range m.rooms {
		for _, user := range room.Invite {
			state := "invite"
			if m.joined[user] {
				state = "join"
			}
			members = append(members, matrix.RoomMember{UserID: user, Membership: state})
		}
	}
	return members, nil
}
func (m *planTestMatrix) UserID(name string) string { return "@" + name + ":test" }
func (m *planTestMatrix) Login(_ context.Context, name, password string) (string, error) {
	return name + "-token", nil
}
func (m *planTestMatrix) JoinRoom(_ context.Context, room, token string) error {
	if token == m.joinBlocked {
		return errors.New("injected join failure")
	}
	if m.joined == nil {
		m.joined = map[string]bool{}
	}
	m.joined[m.UserID(strings.TrimSuffix(token, "-token"))] = true
	return nil
}

func taskPlanFixture(t *testing.T) (*ProjectHandler, *ossfake.Memory, *authpkg.CallerIdentity, *planTestMatrix) {
	t.Helper()
	coordination, store := coordinationFixture(t)
	mx := &planTestMatrix{rooms: map[string]matrix.CreateRoomRequest{}}
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	for worker, token := range map[string]string{"lead-cr": "leader-token", "worker-cr": "worker-token", "second-cr": "second-token"} {
		if err := creds.Save(t.Context(), worker, &service.WorkerCredentials{MatrixToken: token}); err != nil {
			t.Fatal(err)
		}
	}
	coordination.deps.MatrixClient = mx
	coordination.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Matrix: mx, Creds: creds, MatrixDomain: "test"})
	h := NewProjectHandler(coordination.deps.Client, "default", store)
	h.coordination = coordination
	coordination.taskNotifications = h
	h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
	h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return "instance-1", "qwenpaw", nil
	}
	worker := workerCR("second-cr", "second-runtime")
	worker.Spec.Runtime = "qwenpaw"
	worker.Status.MatrixUserID = "@second:test"
	if err := h.client.Create(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	var team v1beta1.Team
	if err := h.client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "alpha"}, &team); err != nil {
		t.Fatal(err)
	}
	team.Spec.WorkerMembers = append(team.Spec.WorkerMembers, v1beta1.TeamWorkerRef{Name: worker.Name, Role: "worker"})
	if err := h.client.Update(t.Context(), &team); err != nil {
		t.Fatal(err)
	}
	putTask(store, "teams/alpha/shared/tasks/main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "team_id": "alpha", "title": "Original title", "status": "planned", "spec": "Original request and acceptance criteria", "assigned_to": "@lead:test", "tasks": []any{},
		"reply_route": map[string]any{"target_user": "@manager:test"}, "requester_reply_route": map[string]any{"target_user": "@human:test", "target_session": "!source:test"}})
	return h, store, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}, mx
}

func planRequest() taskPlanRequest {
	return taskPlanRequest{Nodes: []taskPlanNode{
		{ID: "A", Title: "Parser A", AssignedTo: "worker-cr", Spec: "Produce A", DependsOn: []string{}},
		{ID: "B", Title: "Parser B", AssignedTo: "second-cr", Spec: "Produce B", DependsOn: []string{}},
		{ID: "C", Title: "Integrate", AssignedTo: "worker-cr", Spec: "Consume accepted A and B", DependsOn: []string{"B", "A"}},
	}}
}
func callPlan(h *ProjectHandler, caller *authpkg.CallerIdentity, input taskPlanRequest) *httptest.ResponseRecorder {
	body, _ := json.Marshal(input)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/main/plan", strings.NewReader(string(body)))
	r.SetPathValue("id", "main")
	w := httptest.NewRecorder()
	h.PlanTask(w, withCaller(r, caller))
	return w
}

func TestTaskPlanWaitsForAssignedWorkersToJoinBeforeDispatch(t *testing.T) {
	h, store, caller, mx := taskPlanFixture(t)
	mx.joinBlocked = "worker-token"
	first := callPlan(h, caller, planRequest())
	var result struct {
		Progress string `json:"progress"`
	}
	entries, _, err := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &result) != nil || result.Progress != "pending" || err != nil || len(entries) != 0 {
		t.Fatalf("worker invitation incorrectly permitted dispatch: %d %s events=%v err=%v", first.Code, first.Body.String(), entries, err)
	}
	mx.joinBlocked = ""
	retry := callPlan(h, caller, planRequest())
	if retry.Code != 200 || json.Unmarshal(retry.Body.Bytes(), &result) != nil || result.Progress != "ready" || !mx.joined["@worker:test"] || !mx.joined["@second:test"] || mx.creates != 1 {
		t.Fatalf("the same room did not safely resume dispatch: %d %s joined=%v rooms=%d", retry.Code, retry.Body.String(), mx.joined, mx.creates)
	}
}

func TestTaskPlanStoresWholeDAGAndDispatchesOnlyReadyNodes(t *testing.T) {
	h, store, caller, mx := taskPlanFixture(t)
	first := callPlan(h, caller, planRequest())
	if first.Code != 200 {
		t.Fatalf("plan: %d %s", first.Code, first.Body.String())
	}
	var response struct {
		Task     taskResource `json:"task"`
		Progress string       `json:"progress"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &response)
	if response.Task.PlanID == "" || response.Progress != "ready" || response.Task.Status != "assigned" || len(response.Task.Nodes) != 3 {
		t.Fatalf("invalid plan receipt: %s", first.Body.String())
	}
	for _, node := range response.Task.Nodes {
		if node.TaskID == node.ID || node.TaskID != taskPlanExecutionID("alpha", "main", node.ID) {
			t.Fatalf("local step not scoped to main: %+v", node)
		}
		data, err := store.GetObject(t.Context(), "teams/alpha/shared/tasks/"+node.TaskID+"/meta.json")
		if node.ID == "C" {
			if err == nil || node.Status != "planned" {
				t.Fatalf("dependent node dispatched early: %s", data)
			}
			continue
		}
		var child map[string]any
		if err != nil || json.Unmarshal(data, &child) != nil || child["status"] != "assigned" || child["task_plan_id"] != response.Task.PlanID || child["main_task_id"] != "main" || child["room_id"] != "!planned:test" || !strings.Contains(child["spec"].(string), "Original request and acceptance criteria") {
			t.Fatalf("invalid assignment: %s %v", data, err)
		}
	}
	input := planRequest()
	input.Nodes[0], input.Nodes[2] = input.Nodes[2], input.Nodes[0]
	retry := callPlan(h, caller, input)
	if retry.Code != 200 || mx.creates != 1 {
		t.Fatalf("retry duplicated room or plan: %d %s rooms=%d", retry.Code, retry.Body.String(), mx.creates)
	}
	for _, room := range mx.rooms {
		if room.Name != "TASK：main" {
			t.Fatalf("task room lost its visible task identity: %s", room.Name)
		}
	}
	entries, _, _ := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if len(entries) != 2 {
		t.Fatalf("assignment retries duplicated events: %+v", entries)
	}
	input.Nodes[0].Spec = "Changed work"
	if changed := callPlan(h, caller, input); changed.Code != 409 {
		t.Fatalf("changed plan accepted: %d %s", changed.Code, changed.Body.String())
	}
}

func TestTaskPlanRejectsInvalidGraphAndAssigneesBeforeSideEffects(t *testing.T) {
	for _, scenario := range []string{"empty", "duplicate", "omitted-dependencies", "missing-dependency", "cycle", "self-dependency", "foreign-worker", "leader-execution", "worker-caller", "paused"} {
		t.Run(scenario, func(t *testing.T) {
			h, _, caller, mx := taskPlanFixture(t)
			input := planRequest()
			want := 400
			switch scenario {
			case "empty":
				input.Nodes = nil
			case "duplicate":
				input.Nodes[1].ID = "A"
			case "omitted-dependencies":
				input.Nodes[2].DependsOn = nil
			case "missing-dependency":
				input.Nodes[2].DependsOn = []string{"absent"}
			case "cycle":
				input.Nodes[0].DependsOn = []string{"C"}
			case "self-dependency":
				input.Nodes[0].DependsOn = []string{"A"}
			case "foreign-worker":
				input.Nodes[0].AssignedTo = "unknown"
				want = 403
			case "leader-execution":
				input.Nodes[0].AssignedTo = "lead-cr"
				want = 403
			case "worker-caller":
				caller.Role, caller.Username = authpkg.RoleWorker, "worker-cr"
				want = 403
			case "paused":
				main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
				main["decision_status"] = "frozen"
				putTask(h.oss.(*ossfake.Memory), "teams/alpha/shared/tasks/main/meta.json", main)
				want = 409
			}
			w := callPlan(h, caller, input)
			if w.Code != want || mx.creates != 0 {
				t.Fatalf("invalid plan side effects: %d %s rooms=%d", w.Code, w.Body.String(), mx.creates)
			}
			main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
			if main["task_plan"] != nil {
				t.Fatal("invalid plan committed")
			}
		})
	}
}

func TestTaskPlanRecoversLostRoomCreateResponseWithoutDuplicateRoom(t *testing.T) {
	h, _, caller, mx := taskPlanFixture(t)
	mx.lostResponse = true
	w := callPlan(h, caller, planRequest())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"progress":"pending"`) {
		t.Fatalf("missing durable preparation receipt: %d %s", w.Code, w.Body.String())
	}
	h.recoverTaskNotifications(t.Context())
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if main["status"] != "assigned" || mx.creates != 1 {
		t.Fatalf("room recovery duplicated or lost assignment: %+v rooms=%d", main, mx.creates)
	}
}

type planActivationFailure struct {
	*ossfake.Memory
	fail bool
}

func (s *planActivationFailure) PutObjectIfMatchStrict(ctx context.Context, key string, body []byte, etag string) error {
	var task map[string]any
	_ = json.Unmarshal(body, &task)
	if s.fail && task["task_kind"] == "execution" && task["status"] == "assigned" {
		return errors.New("injected activation outage")
	}
	return s.Memory.PutObjectIfMatchStrict(ctx, key, body, etag)
}
func TestTaskPlanRecoversQueueAdmissionBeforeActivation(t *testing.T) {
	h, store, caller, mx := taskPlanFixture(t)
	fault := &planActivationFailure{Memory: store, fail: true}
	h.oss = fault
	w := callPlan(h, caller, planRequest())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"progress":"pending"`) {
		t.Fatalf("activation outage not retained: %d %s", w.Code, w.Body.String())
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	if child["status"] != "prepared" {
		t.Fatalf("uncertain assignment activated: %+v", child)
	}
	fault.fail = false
	h.recoverTaskNotifications(t.Context())
	h.recoverTaskNotifications(t.Context())
	child, _ = h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	entries, _, _ := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if child["status"] != "assigned" || len(entries) != 2 || mx.creates != 1 {
		t.Fatalf("activation recovery duplicated intent: %+v events=%d rooms=%d", child, len(entries), mx.creates)
	}
}

func submitPlannedCandidate(t *testing.T, h *ProjectHandler, store *ossfake.Memory, id, worker, content string, resultStatuses ...string) string {
	t.Helper()
	key := "teams/alpha/shared/tasks/" + id + "/meta.json"
	child, err := h.coordination.read(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	// Model the native execution claim boundary, not an LLM business run.
	child["execution"] = map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"}
	putTask(store, key, child)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: worker, Team: "alpha"}
	r := httptest.NewRequest(http.MethodPost, "/begin", nil)
	r.SetPathValue("id", id)
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.BeginTask(w, withCaller(r, caller))
	if w.Code != 200 {
		t.Fatalf("begin %s: %d %s", id, w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodPost, "/files?name=output.txt&kind=output", strings.NewReader(content))
	r.SetPathValue("id", id)
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w = httptest.NewRecorder()
	h.PublishTaskFile(w, withCaller(r, caller))
	if w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	var file publishedTaskFile
	_ = json.Unmarshal(w.Body.Bytes(), &file)
	status := "SUCCESS"
	if len(resultStatuses) > 0 {
		status = resultStatuses[0]
	}
	body, _ := json.Marshal(taskSubmitRequest{Summary: "Candidate " + id, Artifacts: []string{file.ArtifactID}, Status: status})
	r = httptest.NewRequest(http.MethodPost, "/submit", strings.NewReader(string(body)))
	r.SetPathValue("id", id)
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w = httptest.NewRecorder()
	h.SubmitTask(w, withCaller(r, caller))
	if w.Code != 200 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Task taskResource `json:"task"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return result.Task.SubmissionID
}

func acceptPlannedCandidate(t *testing.T, h *ProjectHandler, caller *authpkg.CallerIdentity, id, submission string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(taskReviewRequest{SubmissionID: submission, Note: "Independent business validation passed"})
	r := httptest.NewRequest(http.MethodPost, "/accept", strings.NewReader(string(body)))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h.ReviewTask(true)(w, withCaller(r, caller))
	if w.Code != 200 {
		t.Fatalf("accept %s: %d %s", id, w.Code, w.Body.String())
	}
	return w
}

func TestTaskPlanSubmissionAutomaticallyDispatchesDependenciesAndCompletes(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	if w := callPlan(h, leader, planRequest()); w.Code != 200 {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
	a, b, c := taskPlanExecutionID("alpha", "main", "A"), taskPlanExecutionID("alpha", "main", "B"), taskPlanExecutionID("alpha", "main", "C")
	sa := submitPlannedCandidate(t, h, store, a, "worker-cr", "Accepted A bytes")
	if _, err := store.GetObject(t.Context(), "teams/alpha/shared/tasks/"+c+"/meta.json"); err == nil {
		t.Fatal("C dispatched before B delivery")
	}
	sb := submitPlannedCandidate(t, h, store, b, "second-cr", "Accepted B bytes")
	child, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+c+"/meta.json")
	if err != nil || child["status"] != "assigned" {
		t.Fatalf("C was not automatically assigned: %+v %v", child, err)
	}
	request := httptest.NewRequest(http.MethodGet, "/inputs", nil)
	inputs, err := h.taskBeginInputs(request, "teams/alpha/shared/tasks/"+c+"/meta.json", child)
	if err != nil || len(inputs) != 2 {
		t.Fatalf("accepted input context: %+v %v", inputs, err)
	}
	submissions := map[string]string{a: sa, b: sb}
	for _, input := range inputs {
		if input.SubmissionID != submissions[input.TaskID] || len(input.Files) != 1 || input.Files[0].ArtifactID == "" {
			t.Fatalf("dependency lost accepted identity: %+v", input)
		}
	}
	submitPlannedCandidate(t, h, store, c, "worker-cr", "Integrated C bytes")
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	ready := coordinationMap(coordinationMap(main, "task_notifications"), "ready")
	if ready["recipient"] != "lead-cr" {
		t.Fatalf("missing final Leader notification: %+v", ready)
	}

	completed := callTaskComplete(h, leader, "Accepted final results attached")
	var final struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
	}
	_ = json.Unmarshal(completed.Body.Bytes(), &final)
	if completed.Code != 200 || final.Task.Status != "completed" || len(final.Task.Artifacts) != 3 || final.Notification.Recipient != "default" {
		t.Fatalf("full protocol completion failed: %d %s", completed.Code, completed.Body.String())
	}
}

func TestTaskPlanFrozenRecoveryDoesNotActivateQueuedExecutions(t *testing.T) {
	h, store, caller, _ := taskPlanFixture(t)
	fault := &planActivationFailure{Memory: store, fail: true}
	h.oss = fault
	if w := callPlan(h, caller, planRequest()); w.Code != 200 {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
	key := "teams/alpha/shared/tasks/main/meta.json"
	main, _ := h.coordination.read(t.Context(), key)
	main["decision_status"] = "frozen"
	putTask(store, key, main)
	fault.fail = false
	h.recoverTaskNotifications(t.Context())
	id := taskPlanExecutionID("alpha", "main", "A")
	child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	if child["status"] != "prepared" {
		t.Fatalf("freeze bypassed during recovery: %+v", child)
	}
	main["decision_status"] = "open"
	putTask(store, key, main)
	h.recoverTaskNotifications(t.Context())
	child, _ = h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	if child["status"] != "assigned" {
		t.Fatalf("unfrozen assignment did not recover: %+v", child)
	}
}

func TestTaskPlanDoesNotUseForeignRoomOrAgentSuppliedRoutes(t *testing.T) {
	h, store, caller, mx := taskPlanFixture(t)
	mx.foreignRoom = true
	w := callPlan(h, caller, planRequest())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"progress":"pending"`) {
		t.Fatalf("foreign room accepted: %d %s", w.Code, w.Body.String())
	}
	entries, _, _ := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if len(entries) != 0 {
		t.Fatal("foreign room received an assignment intent")
	}
	body, _ := json.Marshal(map[string]any{"nodes": planRequest().Nodes, "roomId": "!foreign:test"})
	r := httptest.NewRequest(http.MethodPost, "/plan", strings.NewReader(string(body)))
	r.SetPathValue("id", "main")
	w = httptest.NewRecorder()
	h.PlanTask(w, withCaller(r, caller))
	if w.Code != 400 {
		t.Fatalf("Agent supplied room accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestTaskPlanPreservesCanonicalAssigneesAndDeniesLegacyPlanMutation(t *testing.T) {
	h, store, caller, _ := taskPlanFixture(t)
	if w := callPlan(h, caller, planRequest()); w.Code != 200 {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
	input := planRequest()
	input.Nodes[0].AssignedTo = "worker-runtime"
	input.Nodes[1].AssignedTo = "@second:test"
	if w := callPlan(h, caller, input); w.Code != 200 {
		t.Fatalf("canonical retry changed plan: %d %s", w.Code, w.Body.String())
	}
	key := "teams/alpha/shared/tasks/main/meta.json"
	before, _ := store.GetObject(t.Context(), key)
	var main map[string]any
	_ = json.Unmarshal(before, &main)
	main["tasks"].([]any)[0].(map[string]any)["spec"] = "Changed assigned work"
	body, _ := json.Marshal(main)
	r := httptest.NewRequest(http.MethodPut, "/meta", strings.NewReader(string(body)))
	r.SetPathValue("id", "main")
	r.Header.Set("If-Match", contentETag(before))
	w := httptest.NewRecorder()
	h.PutTaskMeta(w, withCaller(r, caller))
	if w.Code != 403 {
		t.Fatalf("legacy write changed plan: %d %s", w.Code, w.Body.String())
	}
	after, _ := store.GetObject(t.Context(), key)
	if string(before) != string(after) {
		t.Fatal("legacy write changed authoritative plan")
	}
}

func TestTaskPlanKeepsTeamAdministratorAndEligibleHumanMembership(t *testing.T) {
	for _, mode := range []string{"global-fallback", "named-admin", "accessible-human"} {
		t.Run(mode, func(t *testing.T) {
			h, _, leader, mx := taskPlanFixture(t)
			expected := "@alice:test"
			if mode == "global-fallback" {
				h.coordination.deps.MatrixConfig.AdminUser = "admin"
				expected = "@admin:test"
			} else {
				human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Status: v1beta1.HumanStatus{MatrixUserID: "@alice:test"}}
				if mode == "accessible-human" {
					human.Spec.AccessibleTeams = []string{"alpha"}
				}
				if err := h.client.Create(t.Context(), human); err != nil {
					t.Fatal(err)
				}
				if mode == "named-admin" {
					var team v1beta1.Team
					_ = h.client.Get(t.Context(), client.ObjectKey{Name: "alpha", Namespace: "default"}, &team)
					team.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "alice"}
					if err := h.client.Update(t.Context(), &team); err != nil {
						t.Fatal(err)
					}
				}
			}
			outsider := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "outsider", Namespace: "default"}, Status: v1beta1.HumanStatus{MatrixUserID: "@outsider:test"}}
			if err := h.client.Create(t.Context(), outsider); err != nil {
				t.Fatal(err)
			}
			w := callPlan(h, leader, planRequest())
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"progress":"ready"`) || !mx.joined[expected] {
				t.Fatalf("supervision membership missing: %d %s joined=%v", w.Code, w.Body.String(), mx.joined)
			}
			for _, room := range mx.rooms {
				if mode != "accessible-human" && room.PowerLevels[expected] != 100 {
					t.Fatalf("administrator lost ownership: %+v", room.PowerLevels)
				}
				for _, id := range room.Invite {
					if id == "@outsider:test" {
						t.Fatal("unrelated Human was invited")
					}
				}
			}
		})
	}
}

func TestTaskPlanRecoversPreparedAssignmentAfterQueueOutage(t *testing.T) {
	h, store, leader, mx := taskPlanFixture(t)
	fault := &taskCreateQueueFailure{Memory: store, fail: true}
	h.coordination.deps.OSS = fault
	w := callPlan(h, leader, planRequest())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"progress":"pending"`) {
		t.Fatalf("queue failure lost plan: %d %s", w.Code, w.Body.String())
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	if child["status"] != "prepared" {
		t.Fatalf("failed admission activated execution: %+v", child)
	}
	fault.fail = false
	h.recoverTaskNotifications(t.Context())
	h.recoverTaskNotifications(t.Context())
	child, _ = h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	entries, _, _ := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if child["status"] != "assigned" || len(entries) != 2 || mx.creates != 1 {
		t.Fatalf("queue recovery duplicated assignment or room: %+v events=%d rooms=%d", child, len(entries), mx.creates)
	}
}

func (m *planTestMatrix) SetRoomState(_ context.Context, _, _, _ string, content map[string]any, _ string) error {
	for key, room := range m.rooms {
		room.InitialState[0].Content = content
		m.rooms[key] = room
	}
	return nil
}
func (m *planTestMatrix) InviteToRoomWithToken(_ context.Context, _, user, _ string) error {
	for key, room := range m.rooms {
		room.Invite = append(room.Invite, user)
		m.rooms[key] = room
	}
	return nil
}
func TestPlanReusesIntakeRoomAndInvitesExecutionMembers(t *testing.T) {
	h, store, caller, mx := taskPlanFixture(t)
	body, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	var main map[string]any
	_ = json.Unmarshal(body, &main)
	room, err := h.coordination.ensureTaskPlanRoom(t.Context(), main, taskPlan{ActorName: "lead-cr"})
	if err != nil {
		t.Fatal(err)
	}
	main["room_id"] = room
	putTask(store, "teams/alpha/shared/tasks/main/meta.json", main)
	w := callPlan(h, caller, planRequest())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"progress":"ready"`) || mx.creates != 1 || !mx.joined["@worker:test"] || !mx.joined["@second:test"] {
		t.Fatalf("plan in intake room: %d %s rooms=%d joined=%v", w.Code, w.Body.String(), mx.creates, mx.joined)
	}
}

func TestTaskPlanPublishesOverviewBeforeDispatchAndRetriesWithoutDuplicating(t *testing.T) {
	h, store, caller, _ := taskPlanFixture(t)
	sends := 0
	reject := true
	matrix := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pending, _, _ := store.ListObjectsRecursive(r.Context(), coordinationPending, "", 100)
		if len(pending) != 0 && sends == 0 {
			t.Error("assignment queued before the overview")
		}
		var content map[string]any
		_ = json.NewDecoder(r.Body).Decode(&content)
		run := coordinationMap(content, "org.agentteams.run")
		if !strings.Contains(coordinationString(content, "body"), "交付独立页面") || run["run_id"] != "codex:planning:one" || content["org.agentteams.progress_only"] != true {
			t.Errorf("overview lost its business content or planning run: %v", content)
		}
		if reject {
			w.WriteHeader(503)
			return
		}
		sends++
		_, _ = w.Write([]byte(`{"event_id":"$overview"}`))
	}))
	defer matrix.Close()
	h.coordination.deps.MatrixConfig.ServerURL = matrix.URL
	input, _ := json.Marshal(planRequest())
	var request map[string]any
	_ = json.Unmarshal(input, &request)
	request["overview"] = "当前任务：交付独立页面。要求：中文。验收：可打开且测试通过。分工：A、B 独立开发，C 集成。"
	call := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/api/v1/tasks/main/plan", strings.NewReader(string(body)))
		r.SetPathValue("id", "main")
		r.Header.Set("X-AgentTeams-Run-ID", "codex:planning:one")
		w := httptest.NewRecorder()
		h.PlanTask(w, withCaller(r, caller))
		return w
	}
	first := call()
	pending, _, _ := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"progress":"pending"`) || len(pending) != 0 {
		t.Fatalf("overview failure must block dispatch: %d %s %v", first.Code, first.Body.String(), pending)
	}
	reject = false
	next := call()
	if next.Code != 200 || !strings.Contains(next.Body.String(), `"progress":"ready"`) {
		t.Fatalf("retry failed: %d %s", next.Code, next.Body.String())
	}
	_ = call()
	if sends != 1 {
		t.Fatalf("overview repeated after its confirmed delivery: %d", sends)
	}
}

func TestTaskHandoffWaitsForTheActualStageResultMessage(t *testing.T) {
	h, store, _, _ := taskPlanFixture(t)
	main := map[string]any{"task_plan": map[string]any{"request": map[string]any{"overview": "verified plan"}}, "tasks": []any{map[string]any{"task_id": "dev", "status": "completed"}, map[string]any{"task_id": "test", "depends_on": []string{"dev"}, "status": "assigned"}}}
	for _, kind := range []string{"task_assigned", "main_task_ready", "task_submitted"} {
		t.Run(kind, func(t *testing.T) {
			e := coordinationEvent{Kind: kind, MainTaskID: "main", StorageTeam: "alpha", TaskID: "test"}
			if kind == "task_submitted" {
				e.TaskID = "dev"
			}
			putTask(store, "teams/alpha/shared/tasks/dev/meta.json", map[string]any{"execution": map[string]any{"checkpoint": map[string]any{"delivery": "pending"}}})
			if pending, err := h.coordination.taskResultPublicationPending(t.Context(), e, main, nil); err != nil || !pending {
				t.Fatalf("handoff overtook stage result: %v %v", pending, err)
			}
			putTask(store, "teams/alpha/shared/tasks/dev/meta.json", map[string]any{"execution": map[string]any{"checkpoint": map[string]any{"delivery": "delivered", "reply_event": "$real-result"}}})
			if pending, err := h.coordination.taskResultPublicationPending(t.Context(), e, main, nil); err != nil || pending {
				t.Fatalf("confirmed result did not release handoff: %v %v", pending, err)
			}
		})
	}
}

func TestPlanningWakeUsesANonChatProtocolEvent(t *testing.T) {
	for _, runtime := range []string{"codex", "qwenpaw", "claudecode", "deepseek-harness"} {
		t.Run(runtime, func(t *testing.T) {
			h, _, _, _ := taskPlanFixture(t)
			worker, err := h.coordination.worker(t.Context(), "lead-cr")
			if err != nil {
				t.Fatal(err)
			}
			worker.Spec.Runtime = runtime
			if err := h.client.Update(t.Context(), worker); err != nil {
				t.Fatal(err)
			}
			sends := 0
			mx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends++
				if !strings.Contains(r.URL.Path, "/send/org.agentteams.coordination/") {
					t.Errorf("internal wake became a Matrix chat message: %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"event_id":"$internal-control"}`))
			}))
			defer mx.Close()
			h.coordination.deps.MatrixConfig.ServerURL = mx.URL
			event := coordinationEvent{Kind: "handoff", Protocol: "task/v1", Actor: "@lead:test", Recipient: "@lead:test", ActorName: "lead-cr", ActorRole: authpkg.RoleTeamLeader, ID: "0123456789abcdef0123456789abcdef", RoomID: "!planned:test"}
			content := coordinationMatrixNotification(event, coordinationPayload(event))
			if _, err := h.coordination.sendMatrixContent(t.Context(), event.ActorRole, event.ActorName, event.RoomID, "once", content, false); err != nil || sends != 1 {
				t.Fatalf("control send: %v sends=%d", err, sends)
			}
		})
	}
}
