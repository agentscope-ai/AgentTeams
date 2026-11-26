package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func TestFinalArtifactUsesOriginalManagerMediaAndAttachmentRelation(t *testing.T) {
	h, store, leader := taskCompleteFixture(t)
	w := callTaskComplete(h, leader, "Final report")
	if w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	var main struct {
		Completion taskCompletion `json:"task_completion"`
	}
	_ = json.Unmarshal(data, &main)
	event, err := h.coordination.resolve(t.Context(), coordinationEvent{Kind: "main_task_completed", MainTaskID: "main"}, leader)
	if err != nil {
		t.Fatal(err)
	}
	uploads, sends := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer original-manager-token" {
			t.Error("file not sent as original Manager")
		}
		switch {
		case r.URL.Path == "/_matrix/media/v3/upload":
			uploads++
			body, _ := io.ReadAll(r.Body)
			if string(body) != "Verified report\n" || r.URL.Query().Get("filename") != "report.md" {
				t.Errorf("wrong uploaded file: %s %s", r.URL, body)
			}
			_, _ = w.Write([]byte(`{"content_uri":"mxc://test/file-one"}`))
		case strings.Contains(r.URL.Path, "/rooms/!source:test/send/m.room.message/file-txn"):
			sends++
			var content map[string]any
			_ = json.NewDecoder(r.Body).Decode(&content)
			if content["msgtype"] != "m.file" || content["url"] != "mxc://test/file-one" || coordinationString(coordinationMap(content, "m.relates_to"), "event_id") != "$report" || content["org.agentteams.progress_only"] != true {
				t.Errorf("invalid native attachment event: %+v", content)
			}
			_, _ = w.Write([]byte(`{"event_id":"$artifact"}`))
		default:
			t.Errorf("unexpected file route: %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	if err := creds.Save(t.Context(), "default", &service.WorkerCredentials{MatrixToken: "original-manager-token"}); err != nil {
		t.Fatal(err)
	}
	h.coordination.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Creds: creds})
	h.coordination.deps.MatrixConfig = matrix.Config{ServerURL: server.URL}
	id, err := h.coordination.projectTaskArtifact(t.Context(), event, main.Completion.Artifacts[0], "$report", "file-txn")
	if err != nil || id != "$artifact" || uploads != 1 || sends != 1 {
		t.Fatalf("native artifact not projected: id=%s err=%v uploads=%d sends=%d", id, err, uploads, sends)
	}
}

func TestFinalArtifactsResumePartialProgressAndDoNotRepeatReport(t *testing.T) {
	h, store, leader := taskCompleteFixture(t)
	w := callTaskComplete(h, leader, "Final report")
	var result struct {
		Notification taskNotificationStatus `json:"notification"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	key := coordinationPending + result.Notification.EventID + ".json"
	data, _ := store.GetObject(t.Context(), key)
	var delivery coordinationDelivery
	_ = json.Unmarshal(data, &delivery)
	// Two stable file identities exercise partial progress independent of content.
	first := delivery.Event.Artifacts[0]
	second := first
	second.ArtifactID = "another-output"
	var main map[string]any
	data, _ = store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	_ = json.Unmarshal(data, &main)
	completion := coordinationMap(main, "task_completion")
	completion["artifacts"] = []taskFinalArtifact{first, second}
	putTask(store, "teams/alpha/shared/tasks/main/meta.json", main)
	delivery.Event.Artifacts = []taskFinalArtifact{first, second}
	delivery.UserEventID = "$already-reported"
	counts := map[string]int{}
	fail := true
	h.coordination.projectArtifact = func(_ context.Context, _ coordinationEvent, file taskFinalArtifact, parent, txn string) (string, error) {
		counts[file.ArtifactID]++
		if file.ArtifactID == second.ArtifactID && fail {
			return "", io.ErrUnexpectedEOF
		}
		return "$" + file.ArtifactID, nil
	}
	persisted := 0
	persist := func() error { persisted++; return nil }
	h.coordination.advance(t.Context(), &delivery, persist)
	if delivery.Status == "completed" || delivery.ArtifactEvents[first.ArtifactID] == "" || persisted != 1 {
		t.Fatalf("partial artifact failure misreported: %+v persists=%d", delivery, persisted)
	}
	fail = false
	h.coordination.advance(t.Context(), &delivery, persist)
	if delivery.Status != "completed" || counts[first.ArtifactID] != 1 || counts[second.ArtifactID] != 2 {
		t.Fatalf("artifact resume repeated sent file: %+v counts=%v", delivery, counts)
	}
	for _, caller := range []*authpkg.CallerIdentity{{Role: authpkg.RoleManager, Username: "default"}, {Role: authpkg.RoleManager, Username: "unrelated"}} {
		saved, _ := json.Marshal(delivery)
		if err := store.PutObject(t.Context(), key, saved); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/coordination/events/"+result.Notification.EventID, nil)
		r.SetPathValue("id", result.Notification.EventID)
		ww := httptest.NewRecorder()
		h.coordination.GetEvent(ww, withCaller(r, caller))
		want := 404
		if caller.Username == "default" {
			want = 200
		}
		if ww.Code != want {
			t.Fatalf("Manager receipt access: %d want %d %s", ww.Code, want, ww.Body.String())
		}
	}
}

func TestStageArtifactIsARealFileInTheTaskRoomAndResumesWithoutDuplication(t *testing.T) {
	h, store, _ := taskCompleteFixture(t)
	stage, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	if err != nil {
		t.Fatal(err)
	}
	stage["execution"] = map[string]any{"checkpoint": map[string]any{"delivery": "delivered", "reply_event": "$stage-result"}}
	putTask(store, "teams/alpha/shared/tasks/t1/meta.json", stage)
	main := map[string]any{"task_plan": map[string]any{"request": map[string]any{"overview": "Published task overview"}}, "tasks": []any{map[string]any{"task_id": "t1", "status": "completed"}}}
	e := coordinationEvent{Kind: "main_task_ready", MainTaskID: "main", StorageTeam: "alpha", RoomID: "!task:test", ID: "outgoing-one"}
	uploads, sends := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stage-worker-token" {
			t.Error("stage artifact was not sent by its actual owner")
		}
		if r.URL.Path == "/_matrix/media/v3/upload" {
			uploads++
			bytes, _ := io.ReadAll(r.Body)
			if string(bytes) != "Verified report\n" {
				t.Errorf("wrong artifact bytes: %q", bytes)
			}
			_, _ = w.Write([]byte(`{"content_uri":"mxc://test/stage-file"}`))
			return
		}
		if !strings.Contains(r.URL.Path, "/rooms/!task:test/send/m.room.message/") {
			t.Errorf("stage file did not reach task room: %s", r.URL.Path)
		}
		sends++
		var content map[string]any
		_ = json.NewDecoder(r.Body).Decode(&content)
		if content["msgtype"] != "m.file" || content["body"] != "report.md" || content["filename"] != "report.md" || content["url"] != "mxc://test/stage-file" || coordinationString(coordinationMap(content, "m.relates_to"), "event_id") != "$stage-result" {
			t.Errorf("not a real clickable stage attachment: %v", content)
		}
		_, _ = w.Write([]byte(`{"event_id":"$stage-file"}`))
	}))
	defer server.Close()
	creds := &service.FileCredentialStore{Dir: t.TempDir()}
	if err := creds.Save(t.Context(), "worker-cr", &service.WorkerCredentials{MatrixToken: "stage-worker-token"}); err != nil {
		t.Fatal(err)
	}
	h.coordination.deps.Provisioner = service.NewProvisioner(service.ProvisionerConfig{Creds: creds})
	h.coordination.deps.MatrixConfig = matrix.Config{ServerURL: server.URL}
	delivery := coordinationDelivery{}
	snapshots := 0
	persist := func() error { snapshots++; return nil }
	for i := 0; i < 2; i++ {
		if err := h.coordination.projectStageArtifacts(t.Context(), e, main, &delivery, persist); err != nil {
			t.Fatal(err)
		}
	}
	if uploads != 1 || sends != 1 || snapshots != 1 {
		t.Fatalf("stage retry duplicated attachment or did not persist receipt: uploads=%d sends=%d snapshots=%d", uploads, sends, snapshots)
	}
}

func TestStageFileTransactionIsStableAcrossFanoutNotifications(t *testing.T) {
	h, store, _ := taskCompleteFixture(t)
	stage, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	if err != nil {
		t.Fatal(err)
	}
	stage["execution"] = map[string]any{"checkpoint": map[string]any{"delivery": "delivered", "reply_event": "$result"}}
	putTask(store, "teams/alpha/shared/tasks/t1/meta.json", stage)
	main := map[string]any{"task_plan": map[string]any{"request": map[string]any{"overview": "Planned"}}, "tasks": []any{map[string]any{"task_id": "a", "depends_on": []string{"t1"}}, map[string]any{"task_id": "b", "depends_on": []string{"t1"}}}}
	transactions := map[string]int{}
	h.coordination.projectArtifact = func(_ context.Context, e coordinationEvent, f taskFinalArtifact, parent, txn string) (string, error) {
		transactions[txn]++
		return "$same-file", nil
	}
	for _, target := range []string{"a", "b"} {
		e := coordinationEvent{Kind: "task_assigned", MainTaskID: "main", StorageTeam: "alpha", RoomID: "!task:test", TaskID: target, ID: target}
		if err := h.coordination.projectStageArtifacts(t.Context(), e, main, &coordinationDelivery{}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(transactions) != 1 {
		t.Fatalf("parallel handoffs would duplicate the same room attachment: %v", transactions)
	}
}
