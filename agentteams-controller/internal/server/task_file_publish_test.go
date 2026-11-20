package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestTaskFilePublishIsIdempotentAndPreservesTaskState(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(map[string]any{
		"assigned_to": "dev",
		"execution":   map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"},
	}))
	h := newProjectTestHandler(t, store, team("alpha-team"))
	h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
	h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return "instance-1", "qwenpaw", nil
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}
	contents := "id,value\na,1\n"
	publish := func(process string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/files?name=analysis.csv&kind=intermediate", strings.NewReader(contents))
		r.SetPathValue("id", "t1")
		r.Header.Set("X-AgentTeams-Process-ID", process)
		w := httptest.NewRecorder()
		h.PublishTaskFile(w, withCaller(r, caller))
		return w
	}
	first, retry := publish("process-1"), publish("process-1")
	if first.Code != http.StatusOK || retry.Code != http.StatusOK {
		t.Fatalf("publish/retry: %d %s; %d %s", first.Code, first.Body.String(), retry.Code, retry.Body.String())
	}
	var a, b struct {
		ArtifactID string `json:"artifactId"`
		SHA256     string `json:"sha256"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.ArtifactID == "" || a.ArtifactID != b.ArtifactID || a.SHA256 == "" {
		t.Fatalf("unstable identity: %s / %s", first.Body.String(), retry.Body.String())
	}
	data, err := store.GetObject(t.Context(), taskMetaKey)
	if err != nil || !strings.Contains(string(data), `"status":"in_progress"`) {
		t.Fatalf("publication changed task state: %s %v", data, err)
	}
	contents = "id,value\na,2\n"
	newer := publish("process-1")
	if newer.Code != http.StatusOK {
		t.Fatalf("new version: %d %s", newer.Code, newer.Body.String())
	}
	var next struct {
		ArtifactID string `json:"artifactId"`
	}
	if err := json.Unmarshal(newer.Body.Bytes(), &next); err != nil || next.ArtifactID == a.ArtifactID {
		t.Fatalf("new identity: %s %v", newer.Body.String(), err)
	}
	for _, tc := range []struct{ query, expected string }{
		{"&artifactId=" + a.ArtifactID, "id,value\na,1\n"},
		{"&name=analysis.csv", contents},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/files?team=alpha-team"+tc.query, nil)
		r.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		h.GetTaskFiles(w, withCaller(r, caller))
		if w.Code != http.StatusOK || w.Body.String() != tc.expected {
			t.Fatalf("artifact read: %d %q", w.Code, w.Body.String())
		}
	}
	if stale := publish("process-old"); stale.Code != http.StatusConflict {
		t.Fatalf("stale process accepted: %d %s", stale.Code, stale.Body.String())
	}
	caller.Username = "other-worker"
	if other := publish("process-1"); other.Code != http.StatusForbidden {
		t.Fatalf("foreign assignee accepted: %d %s", other.Code, other.Body.String())
	}
}

func TestTaskFilePublicationOperationAndContentIdentity(t *testing.T) {
	for _, replay := range []bool{true, false} {
		name, key, want := "new_operation_promotes_previous_content", "op-a-new", "A"
		if replay {
			name, key, want = "late_retry_preserves_latest_content", "op-a", "B"
		}
		t.Run(name, func(t *testing.T) {
			store := ossfake.NewMemory()
			putTask(store, taskMetaKey, taskMeta(map[string]any{
				"assigned_to": "dev",
				"execution":   map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"},
			}))
			h := newProjectTestHandler(t, store, team("alpha-team"))
			h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
			h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
				return "instance-1", "qwenpaw", nil
			}
			caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}
			publish := func(key, content string, wantStatus int) publishedTaskFile {
				t.Helper()
				r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/files?name=result.md&kind=output", strings.NewReader(content))
				r.SetPathValue("id", "t1")
				r.Header.Set("X-AgentTeams-Process-ID", "process-1")
				r.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				h.PublishTaskFile(w, withCaller(r, caller))
				if w.Code != wantStatus {
					t.Fatalf("publish status %d, want %d: %s", w.Code, wantStatus, w.Body.String())
				}
				var result publishedTaskFile
				if wantStatus == http.StatusOK {
					if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
				}
				return result
			}
			read := func(query, want string) {
				t.Helper()
				r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/files?team=alpha-team&"+query, nil)
				r.SetPathValue("id", "t1")
				w := httptest.NewRecorder()
				h.GetTaskFiles(w, withCaller(r, caller))
				if w.Code != http.StatusOK || w.Body.String() != want {
					t.Fatalf("artifact read: status=%d body=%q, want %q", w.Code, w.Body.String(), want)
				}
			}
			a := publish("op-a", "A", http.StatusOK)
			b := publish("op-b", "B", http.StatusOK)
			publish("op-a", "different payload", http.StatusConflict)
			publish("bad key", "A", http.StatusBadRequest)
			if result := publish(key, "A", http.StatusOK); result != a {
				t.Fatalf("identical bytes should retain the immutable reference: got %+v, want %+v", result, a)
			}
			read("name=result.md", want)
			read("artifactId="+a.ArtifactID, "A")
			read("artifactId="+b.ArtifactID, "B")
		})
	}
}

type cancelDuringTaskFileCommit struct {
	*ossfake.Memory
	cancelled bool
}

func (s *cancelDuringTaskFileCommit) PutObjectIfMatchStrict(ctx context.Context, key string, body []byte, etag string) error {
	if key == taskMetaKey && !s.cancelled {
		s.cancelled = true
		current, err := s.Memory.GetObject(ctx, key)
		if err != nil {
			return err
		}
		var task map[string]any
		if err := json.Unmarshal(current, &task); err != nil {
			return err
		}
		task["status"] = "cancelled"
		cancelled, _ := json.Marshal(task)
		if err := s.Memory.PutObject(ctx, key, cancelled); err != nil {
			return err
		}
		return oss.ErrPreconditionFailed
	}
	return s.Memory.PutObjectIfMatchStrict(ctx, key, body, etag)
}

func TestTaskFilePublicationCannotRacePastCancellation(t *testing.T) {
	store := &cancelDuringTaskFileCommit{Memory: ossfake.NewMemory()}
	putTask(store.Memory, taskMetaKey, taskMeta(map[string]any{
		"assigned_to": "dev", "execution": map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"},
	}))
	h := newProjectTestHandler(t, store.Memory, team("alpha-team"))
	h.oss = store
	h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
	h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return "instance-1", "qwenpaw", nil
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/files?name=report.md", strings.NewReader("report"))
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.PublishTaskFile(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("cancel raced publication: %d %s", w.Code, w.Body.String())
	}
	current, err := store.GetObject(t.Context(), taskMetaKey)
	if err != nil || strings.Contains(string(current), "task_files") || !strings.Contains(string(current), `"status":"cancelled"`) {
		t.Fatalf("invalid cancellation state: %s %v", current, err)
	}
}

func TestTaskMetaCannotRewritePublishedArtifactIndex(t *testing.T) {
	store := ossfake.NewMemory()
	meta := taskMeta(map[string]any{"task_files": map[string]any{"artifacts": map[string]any{}, "heads": map[string]string{}}})
	putTask(store, taskMetaKey, meta)
	h := newProjectTestHandler(t, store, team("alpha-team"))
	delete(meta, "task_files")
	response := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(meta), &authpkg.CallerIdentity{Role: authpkg.RoleAdmin})
	if response.Code != http.StatusForbidden {
		t.Fatalf("file index removed: %d %s", response.Code, response.Body.String())
	}
}

func TestTaskFilePublishCannotAcceptCancelledTask(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(map[string]any{"status": "cancelled", "assigned_to": "dev"}))
	h := newProjectTestHandler(t, store, team("alpha-team"))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/files?name=report.md", strings.NewReader("report"))
	r.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	h.PublishTaskFile(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("cancelled task accepted: %d %s", w.Code, w.Body.String())
	}
}
