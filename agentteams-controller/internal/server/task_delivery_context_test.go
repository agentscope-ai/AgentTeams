package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestTaskDeliveryContextListsOnlyConfirmedOwnOriginalRoomDeliveries(t *testing.T) {
	h, store := coordinationFixture(t)
	for _, test := range []struct {
		id, room, recipient, report, status string
		completeFile                        bool
	}{
		{strings.Repeat("1", 32), "!source:test", "default", "Actually sent report", "completed", true},
		{strings.Repeat("2", 32), "!other:test", "default", "Other room", "completed", true},
		{strings.Repeat("3", 32), "!source:test", "another", "Another dispatcher", "completed", true},
		{strings.Repeat("4", 32), "!source:test", "default", "Pending", "queued", true},
		{strings.Repeat("5", 32), "!source:test", "default", "Partial attachments", "completed", false},
		{strings.Repeat("6", 32), "!source:test", "default", "", "completed", true},
	} {
		d := coordinationDelivery{Event: coordinationEvent{ID: test.id, Kind: "main_task_completed", MainTaskID: "main", RecipientKind: "manager", RecipientName: test.recipient, Recipient: "@manager:test", RequesterRoute: map[string]any{"target_session": test.room}, Artifacts: []taskFinalArtifact{{TaskID: "child", publishedTaskFile: publishedTaskFile{ArtifactID: "a1", Name: "app.py", Version: "v1"}}}}, Status: test.status, ReportSnapshot: test.report, UserEventID: "$report", ArtifactEvents: map[string]string{}}
		if test.completeFile {
			d.ArtifactEvents["a1"] = "$file"
		}
		data, _ := json.Marshal(d)
		if err := store.PutObject(t.Context(), coordinationDone+test.id+".json", data); err != nil {
			t.Fatal(err)
		}
	}
	call := func(role string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/task-deliveries?roomId=!source:test", nil)
		w := httptest.NewRecorder()
		h.ListTaskDeliveries(w, withCaller(r, &authpkg.CallerIdentity{Role: role, Username: "default"}))
		return w
	}
	w := call(authpkg.RoleManager)
	var response struct {
		Deliveries []taskDeliveryRecord `json:"deliveries"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Deliveries) != 1 || response.Deliveries[0].Report != "Actually sent report" || response.Deliveries[0].Artifacts[0].MessageID != "$file" {
		t.Fatalf("wrong confirmed deliveries: %d %s", w.Code, w.Body.String())
	}
	if denied := call(authpkg.RoleWorker); denied.Code != 403 {
		t.Fatalf("Worker read Manager outgoing context: %d %s", denied.Code, denied.Body.String())
	}
}

func TestTaskDeliveryContextPagesStableRecordIdentities(t *testing.T) {
	h, store := coordinationFixture(t)
	for i := 1; i <= 33; i++ {
		id := fmt.Sprintf("%032x", i)
		data, _ := json.Marshal(coordinationDelivery{Event: coordinationEvent{ID: id, Kind: "main_task_completed", MainTaskID: fmt.Sprint("main-", i), RecipientKind: "manager", RecipientName: "default", Recipient: "@manager:test", RequesterRoute: map[string]any{"target_session": "!source:test"}}, Status: "completed", ReportSnapshot: "Accepted report", UserEventID: "$report"})
		if err := store.PutObject(t.Context(), coordinationDone+id+".json", data); err != nil {
			t.Fatal(err)
		}
	}
	cursor := ""
	for page, want := range []int{32, 1} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/task-deliveries?roomId=!source:test&after="+cursor, nil)
		w := httptest.NewRecorder()
		h.ListTaskDeliveries(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}))
		var response struct {
			Deliveries []taskDeliveryRecord `json:"deliveries"`
			NextCursor string               `json:"nextCursor"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Deliveries) != want {
			t.Fatalf("page %d: %d %s", page, w.Code, w.Body.String())
		}
		if page == 0 && response.NextCursor == "" || page == 1 && response.NextCursor != "" {
			t.Fatalf("wrong pagination boundary: %s", w.Body.String())
		}
		cursor = response.NextCursor
	}
}
