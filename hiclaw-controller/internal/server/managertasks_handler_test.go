package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestManagerTasksHandlerHappyPath(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	const stateJSON = `{
  "admin_dm_room_id": "!room:example.org",
  "active_tasks": [{"task_id": "T1", "status": "blocked", "blocked_since": "2026-07-01T00:00:00Z"}],
  "cancelled_tasks": [],
  "last_digest_sent_at": null,
  "updated_at": "2026-07-02T00:00:00Z"
}`
	if err := os.WriteFile(statePath, []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	handler := NewManagerTasksHandler(func() string { return statePath })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/manager-tasks", nil)
	rec := httptest.NewRecorder()

	handler.GetManagerTasks(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got["admin_dm_room_id"] != "!room:example.org" {
		t.Fatalf("expected passthrough of admin_dm_room_id, got %v", got["admin_dm_room_id"])
	}
	tasks, ok := got["active_tasks"].([]any)
	if !ok || len(tasks) != 1 {
		t.Fatalf("expected 1 active_task passed through raw, got %v", got["active_tasks"])
	}

	// Confirm true byte-passthrough: no re-modeling changes field order/shape.
	var roundTrip map[string]any
	if err := json.Unmarshal([]byte(stateJSON), &roundTrip); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	gotBytes, _ := json.Marshal(got)
	wantBytes, _ := json.Marshal(roundTrip)
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("expected raw passthrough;\n got: %s\nwant: %s", gotBytes, wantBytes)
	}
}

func TestManagerTasksHandlerMissingFile(t *testing.T) {
	dir := t.TempDir()
	missingPath := filepath.Join(dir, "does-not-exist.json")

	handler := NewManagerTasksHandler(func() string { return missingPath })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/manager-tasks", nil)
	rec := httptest.NewRecorder()

	handler.GetManagerTasks(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "manager state not available" {
		t.Fatalf("unexpected error body: %v", body)
	}
}

func TestManagerTasksHandlerMalformedFile(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	handler := NewManagerTasksHandler(func() string { return statePath })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/manager-tasks", nil)
	rec := httptest.NewRecorder()

	handler.GetManagerTasks(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadGateway, rec.Code, rec.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "manager state unreadable" {
		t.Fatalf("unexpected error body: %v", body)
	}
}

func TestManagerTasksHandlerUnreadableDirectory(t *testing.T) {
	// Pointing the "file" path at a directory reliably triggers a read
	// error that is not os.IsNotExist, exercising the 502 branch distinct
	// from the malformed-JSON case.
	dir := t.TempDir()

	handler := NewManagerTasksHandler(func() string { return dir })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/manager-tasks", nil)
	rec := httptest.NewRecorder()

	handler.GetManagerTasks(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadGateway, rec.Code, rec.Body.String())
	}
}
