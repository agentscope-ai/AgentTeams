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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func taskCreateFixture(t *testing.T) (*ProjectHandler, *ossfake.Memory) {
	t.Helper()
	coordination, store := coordinationFixture(t)
	var team v1beta1.Team
	if err := coordination.deps.Client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "alpha"}, &team); err != nil {
		t.Fatal(err)
	}
	team.Status.LeaderDMRoomID = "!leader-dm:test"
	if err := coordination.deps.Client.Update(t.Context(), &team); err != nil {
		t.Fatal(err)
	}
	var leader v1beta1.Worker
	if err := coordination.deps.Client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "lead-cr"}, &leader); err != nil {
		t.Fatal(err)
	}
	leader.Status.RoomID = "!manager-leader:test"
	if err := coordination.deps.Client.Update(t.Context(), &leader); err != nil {
		t.Fatal(err)
	}
	h := NewProjectHandler(coordination.deps.Client, "default", store)
	h.coordination = coordination
	taskCreateSourceServer(t, h, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "room_id": "!source:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "Original acceptance criteria"}})
	})
	return h, store
}

func callTaskCreate(h *ProjectHandler, caller *authpkg.CallerIdentity, source, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
	r.Header.Set("X-AgentTeams-Origin-Event-ID", source)
	r.Header.Set("X-AgentTeams-Requester-User-ID", "@human:test")
	r.Header.Set("X-AgentTeams-Requester-Room-ID", "!source:test")
	w := httptest.NewRecorder()
	h.CreateTask(w, withCaller(r, caller))
	return w
}

func taskCreateSourceServer(t *testing.T, h *ProjectHandler, handler http.HandlerFunc) {
	t.Helper()
	last := "$source"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_ = json.NewEncoder(w).Encode(map[string]any{"chunk": []any{map[string]any{"event_id": last, "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "Latest input"}}}})
			return
		}
		last = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	if err := creds.Save(t.Context(), "default", &service.WorkerCredentials{MatrixToken: "dispatch-manager-token"}); err != nil {
		t.Fatal(err)
	}
	h.coordination.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Creds: creds})
	h.coordination.deps.MatrixConfig = matrix.Config{ServerURL: server.URL}
}

func TestTaskCreatePreservesHumanRequirementOmittedByDispatchSummary(t *testing.T) {
	h, store := taskCreateFixture(t)
	original := "Implement CSV summary. Empty input succeeds; output amounts have two decimal places and zero is 0.00."
	taskCreateSourceServer(t, h, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/_matrix/client/v3/rooms/!source:test/event/$original" || r.Header.Get("Authorization") != "Bearer dispatch-manager-token" {
			t.Errorf("wrong source request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$original", "room_id": "!source:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": original}})
	})
	w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$original", `{"title":"CSV","spec":"Implement CSV summary using Python standard library.","leader":"lead-cr"}`)
	var result struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !strings.Contains(result.Task.Spec, original) || !strings.Contains(result.Task.Spec, "Python standard library") {
		t.Fatalf("lost source requirements or useful dispatch context: %d %s", w.Code, w.Body.String())
	}
	data, _ := store.GetObject(t.Context(), coordinationPending+result.Notification.EventID+".json")
	var delivery coordinationDelivery
	if json.Unmarshal(data, &delivery) != nil || len(delivery.Event.Tasks) != 1 || delivery.Event.Tasks[0].Spec != result.Task.Spec || delivery.Event.Text != result.Task.Spec {
		t.Fatalf("source snapshot not included in actual handoff: %s", data)
	}
}

func TestTaskCreateNeedsNoModelCopyOfCapturedRequest(t *testing.T) {
	h, _ := taskCreateFixture(t)
	w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$original", `{"title":"CSV","leader":"lead-cr"}`)
	var result struct {
		Task taskResource `json:"task"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Task.Spec != "Original acceptance criteria" {
		t.Fatalf("captured request still requires a model copy: %d %s", w.Code, w.Body.String())
	}
}

func TestTaskCreateRejectsUnverifiedSourceBeforeRegistration(t *testing.T) {
	for _, kind := range []string{"unavailable", "wrong_sender", "wrong_event", "wrong_room", "redacted", "not_message"} {
		t.Run(kind, func(t *testing.T) {
			h, store := taskCreateFixture(t)
			taskCreateSourceServer(t, h, func(w http.ResponseWriter, r *http.Request) {
				if kind == "unavailable" {
					w.WriteHeader(503)
					return
				}
				e := map[string]any{"event_id": "$original", "room_id": "!source:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "Original requirement"}}
				switch kind {
				case "wrong_sender":
					e["sender"] = "@other:test"
				case "wrong_event":
					e["event_id"] = "$other"
				case "wrong_room":
					e["room_id"] = "!other:test"
				case "redacted":
					e["content"] = map[string]any{}
				case "not_message":
					e["type"] = "m.room.member"
				}
				_ = json.NewEncoder(w).Encode(e)
			})
			w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$original", `{"title":"CSV","spec":"Agent summary","leader":"lead-cr"}`)
			want := 403
			if kind == "unavailable" {
				want = 503
			}
			if w.Code != want {
				t.Fatalf("unverified source admitted: %d %s", w.Code, w.Body.String())
			}
			entries, _, err := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unverified source dispatched: %v %v", entries, err)
			}
			tasks, _, err := store.ListObjectsRecursive(t.Context(), "teams/alpha/shared/tasks/", "", 100)
			if err != nil || len(tasks) != 1 { // Only the fixture's pre-existing t1.
				t.Fatalf("unverified source registered a task: %v %v", tasks, err)
			}
		})
	}
}

func TestTaskCreateUsesOriginalContextAndSingleDurableHandoff(t *testing.T) {
	h, store := taskCreateFixture(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}
	body := `{"title":"CSV analysis","spec":"Original acceptance criteria","leader":"lead-cr"}`
	a, b, c := callTaskCreate(h, caller, "$request-1", body), callTaskCreate(h, caller, "$request-1", body), callTaskCreate(h, caller, "$request-2", body)
	if a.Code != 201 || b.Code != 200 || c.Code != 201 {
		t.Fatalf("create/retry/fresh request: %d %s / %d %s / %d %s", a.Code, a.Body.String(), b.Code, b.Body.String(), c.Code, c.Body.String())
	}
	var first, retry, fresh struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
	}
	_ = json.Unmarshal(a.Body.Bytes(), &first)
	_ = json.Unmarshal(b.Body.Bytes(), &retry)
	_ = json.Unmarshal(c.Body.Bytes(), &fresh)
	if first.Task.ID == "" || first.Task.Kind != "main" || first.Task.Status != "planned" || first.Task.Spec != "Original acceptance criteria" || first.Task.ID != retry.Task.ID || first.Task.ID == fresh.Task.ID || first.Notification.EventID != retry.Notification.EventID || first.Notification.Recipient != "lead-cr" {
		t.Fatalf("invalid main request receipt: %s / %s / %s", a.Body.String(), b.Body.String(), c.Body.String())
	}
	data, err := store.GetObject(t.Context(), "teams/alpha/shared/tasks/"+first.Task.ID+"/meta.json")
	var main map[string]any
	if err != nil || json.Unmarshal(data, &main) != nil || coordinationString(main, "source_room_id") != "!source:test" || coordinationString(coordinationMap(main, "reply_route"), "target_user") != "@manager:test" {
		t.Fatalf("lost original source/dispatch identity: %s %v", data, err)
	}
	data, err = store.GetObject(t.Context(), coordinationPending+first.Notification.EventID+".json")
	var handoff coordinationDelivery
	if err != nil || json.Unmarshal(data, &handoff) != nil || handoff.Event.RoomID != "!manager-leader:test" || coordinationString(handoff.Event.SourceRoute, "target_session") != "!source:test" || len(handoff.Event.Tasks) != 1 || handoff.Event.Tasks[0].TaskID != first.Task.ID {
		t.Fatalf("wrong handoff room or request identity: %s %v", data, err)
	}
	if handoff.Event.Protocol != "task/v1" {
		t.Fatalf("registered task handoff lost protocol: %s", data)
	}
	changed := callTaskCreate(h, caller, "$request-1", `{"title":"Changed request","spec":"Different criteria","leader":"lead-cr"}`)
	if changed.Code != 409 {
		t.Fatalf("same source overwritten: %d %s", changed.Code, changed.Body.String())
	}
}

func TestTaskCreateRejectsWorkerMissingSourceAndAgentSuppliedRoutes(t *testing.T) {
	h, _ := taskCreateFixture(t)
	for _, tc := range []struct {
		role, source, body string
		want               int
	}{
		{authpkg.RoleWorker, "$event", `{"title":"T","spec":"S","leader":"lead-cr"}`, 403},
		{authpkg.RoleManager, "", `{"title":"T","spec":"S","leader":"lead-cr"}`, 400},
		{authpkg.RoleManager, "$event", `{"title":"T","spec":"S","leader":"lead-cr","sourceRoom":"!forged:test"}`, 400},
	} {
		w := callTaskCreate(h, &authpkg.CallerIdentity{Role: tc.role, Username: "default"}, tc.source, tc.body)
		if w.Code != tc.want {
			t.Fatalf("invalid creation admitted: %d want %d %s", w.Code, tc.want, w.Body.String())
		}
	}
}

type taskCreateQueueFailure struct {
	*ossfake.Memory
	fail bool
}

func (s *taskCreateQueueFailure) PutObjectIfAbsent(ctx context.Context, key string, data []byte) error {
	if s.fail && strings.HasPrefix(key, coordinationPending) {
		return errors.New("fixture queue outage")
	}
	return s.Memory.PutObjectIfAbsent(ctx, key, data)
}

func TestTaskCreateRecoversHandoffAfterRegistrationQueueGap(t *testing.T) {
	h, store := taskCreateFixture(t)
	fault := &taskCreateQueueFailure{Memory: store, fail: true}
	h.oss = fault
	h.coordination.deps.OSS = fault
	body := `{"title":"T","spec":"Original request","leader":"lead-cr"}`
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}
	first := callTaskCreate(h, caller, "$gap", body)
	if first.Code != 503 {
		t.Fatalf("queue outage not reported: %d %s", first.Code, first.Body.String())
	}
	fault.fail = false
	h.recoverTaskNotifications(t.Context())
	retried := callTaskCreate(h, caller, "$gap", body)
	if retried.Code != 200 {
		t.Fatalf("handoff recovery failed: %d %s", retried.Code, retried.Body.String())
	}
	entries, _, err := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("duplicate or missing recovered handoff: %v %v", entries, err)
	}
}

func TestTaskCreateProtectsOriginalSourceBeforePlanning(t *testing.T) {
	h, store := taskCreateFixture(t)
	w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$source", `{"title":"T","spec":"Original request","leader":"lead-cr"}`)
	var result struct {
		Task taskResource `json:"task"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Body.String())
	}
	key := "teams/alpha/shared/tasks/" + result.Task.ID + "/meta.json"
	data, _ := store.GetObject(t.Context(), key)
	for _, field := range []string{"task_origin", "requester_reply_route", "source_room_id", "spec"} {
		var task map[string]any
		_ = json.Unmarshal(data, &task)
		delete(task, field)
		body, _ := json.Marshal(task)
		r := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/"+result.Task.ID+"/meta", strings.NewReader(string(body)))
		r.SetPathValue("id", result.Task.ID)
		r.Header.Set("If-Match", contentETag(data))
		response := httptest.NewRecorder()
		h.PutTaskMeta(response, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}))
		if response.Code != 403 {
			t.Fatalf("original %s replaced before planning: %d %s", field, response.Code, response.Body.String())
		}
	}
}

func TestCreatedTaskPublicViewExposesSourceIdentityAndNotificationReferences(t *testing.T) {
	h, _ := taskCreateFixture(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}
	w := callTaskCreate(h, caller, "$origin", `{"title":"T","spec":"Original request","leader":"lead-cr"}`)
	var created struct {
		Task taskResource `json:"task"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatal(w.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+created.Task.ID+"?team=alpha", nil)
	r.SetPathValue("id", created.Task.ID)
	result := httptest.NewRecorder()
	h.GetTaskResource(result, withCaller(r, caller))
	var view struct {
		Task taskResource `json:"task"`
	}
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &view) != nil || view.Task.OriginEventID != "$origin" || view.Task.Notifications["handoff"].EventID == "" || view.Task.Notifications["handoff"].Status != "queued" {
		t.Fatalf("missing framework-readable public origin/receipt: %d %s", result.Code, result.Body.String())
	}
	if strings.Contains(result.Body.String(), "!source:test") || strings.Contains(result.Body.String(), "task_notifications") || strings.Contains(result.Body.String(), "shared/tasks") {
		t.Fatalf("public resource exposed internal notification storage: %s", result.Body.String())
	}
}

func TestTaskCreateDeliversThroughManagerLeaderRoom(t *testing.T) {
	h, store := taskCreateFixture(t)
	sent := 0
	taskCreateSourceServer(t, h, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dispatch-manager-token" {
			t.Error("wrong dispatch identity")
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$request", "room_id": "!source:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "Original request"}})
			return
		}
		// The private Leader/Admin room intentionally excludes Manager membership.
		if !strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!manager-leader:test/send/m.room.message/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		sent++
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$handoff"})
	})
	w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$request", `{"title":"CSV","leader":"lead-cr"}`)
	var result struct {
		Notification taskNotificationStatus `json:"notification"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	data, err := store.GetObject(t.Context(), coordinationPending+result.Notification.EventID+".json")
	var delivery coordinationDelivery
	if err != nil || json.Unmarshal(data, &delivery) != nil {
		t.Fatalf("missing handoff: %s %v", data, err)
	}
	h.coordination.advance(t.Context(), &delivery, func() error { return nil })
	if sent != 1 || delivery.MatrixEventID != "$handoff" || delivery.Status != "running" {
		t.Fatalf("handoff failed: sent=%d status=%s error=%s room=%s", sent, delivery.Status, delivery.Error, delivery.Event.RoomID)
	}
}

func TestTaskCreateRequiresReadyManagerLeaderRoom(t *testing.T) {
	h, store := taskCreateFixture(t)
	var leader v1beta1.Worker
	if err := h.coordination.deps.Client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "lead-cr"}, &leader); err != nil {
		t.Fatal(err)
	}
	leader.Status.RoomID = ""
	if err := h.coordination.deps.Client.Update(t.Context(), &leader); err != nil {
		t.Fatal(err)
	}
	w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$request", `{"title":"CSV","leader":"lead-cr"}`)
	entries, _ := store.ListObjects(t.Context(), coordinationPending)
	if w.Code != http.StatusConflict || len(entries) != 0 {
		t.Fatalf("unready route registered handoff: %d %s", w.Code, w.Body.String())
	}
}

func TestTaskCreateCapturesRelatedHumanCorrectionsWithoutModelCopy(t *testing.T) {
	h, _ := taskCreateFixture(t)
	taskCreateSourceServer(t, h, func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body := "Use CSV output."
		if id == "$correction" {
			body = "Change the output to JSON; keep the other requirements."
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": id, "room_id": "!source:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": body}})
	})
	w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$original", `{"title":"Report","leader":"lead-cr","relatedSources":["$correction"]}`)
	var result struct {
		Task taskResource `json:"task"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !strings.Contains(result.Task.Spec, "Use CSV output.") || !strings.Contains(result.Task.Spec, "Change the output to JSON") || strings.Contains(result.Task.Spec, "cannot override the original") {
		t.Fatalf("correction snapshot lost or unable to supersede original: %d %s", w.Code, w.Body.String())
	}
}

func TestTaskCreateRejectsInvalidRelatedSourcesBeforeRegistration(t *testing.T) {
	for _, related := range []string{`["$original"]`, `["$correction","$correction"]`, `["not-an-event"]`, `["$foreign"]`} {
		t.Run(related, func(t *testing.T) {
			h, store := taskCreateFixture(t)
			before, _ := store.ListObjects(t.Context(), "teams/alpha/shared/tasks/")
			taskCreateSourceServer(t, h, func(w http.ResponseWriter, r *http.Request) {
				id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				sender := "@human:test"
				if id == "$foreign" {
					sender = "@another:test"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"event_id": id, "room_id": "!source:test", "sender": sender, "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "requirements"}})
			})
			w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$original", `{"title":"Report","leader":"lead-cr","relatedSources":`+related+`}`)
			if w.Code != 400 && w.Code != 403 {
				t.Fatalf("invalid correction accepted: %d %s", w.Code, w.Body.String())
			}
			roots, _ := store.ListObjects(t.Context(), "teams/alpha/shared/tasks/")
			if len(roots) != len(before) {
				t.Fatalf("invalid correction created task objects: %v", roots)
			}
		})
	}
}

func TestTaskCreateDefersNewHumanInputBeforeAnyRegistration(t *testing.T) {
	h, store := taskCreateFixture(t)
	before, _ := store.ListObjects(t.Context(), "teams/alpha/shared/tasks/")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event := func(id, body string) map[string]any {
			return map[string]any{"event_id": id, "room_id": "!source:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": body}}
		}
		if strings.HasSuffix(r.URL.Path, "/messages") {
			if r.URL.Query().Get("dir") != "b" || !strings.Contains(r.URL.Query().Get("filter"), "@human:test") {
				t.Error("frontier must use the authenticated requester's reverse timeline")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"chunk": []any{event("$new", "Cancel the pending request; do not create a Task."), event("$original", "Build a report.")}})
		} else {
			_ = json.NewEncoder(w).Encode(event("$original", "Build a report."))
		}
	}))
	defer server.Close()
	h.coordination.deps.MatrixConfig.ServerURL = server.URL
	w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$original", `{"title":"Report","leader":"lead-cr"}`)
	var result struct {
		Outcome string              `json:"outcome"`
		NoTask  bool                `json:"noTaskRegistered"`
		Inputs  []map[string]string `json:"inputs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Outcome != "input_changed" || !result.NoTask || len(result.Inputs) != 1 || result.Inputs[0]["eventId"] != "$new" || !strings.Contains(result.Inputs[0]["body"], "Cancel") {
		t.Fatalf("stale intent reached registration: %d %s", w.Code, w.Body.String())
	}
	after, _ := store.ListObjects(t.Context(), "teams/alpha/shared/tasks/")
	if len(before) != len(after) {
		t.Fatalf("stale intent created task objects: %v", after)
	}
}

func TestTaskCreateFreshnessReadFailureDoesNotRegister(t *testing.T) {
	h, store := taskCreateFixture(t)
	before, _ := store.ListObjects(t.Context(), "teams/alpha/shared/tasks/")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$original", "room_id": "!source:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "Build a report."}})
	}))
	defer server.Close()
	h.coordination.deps.MatrixConfig.ServerURL = server.URL
	w := callTaskCreate(h, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}, "$original", `{"title":"Report","leader":"lead-cr"}`)
	after, _ := store.ListObjects(t.Context(), "teams/alpha/shared/tasks/")
	if w.Code != 503 || len(before) != len(after) {
		t.Fatalf("unavailable freshness admitted creation: %d %s", w.Code, w.Body.String())
	}
}
