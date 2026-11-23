package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerWaitReadyIsReadOnly(t *testing.T) {
	var calls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/api/v1/workers/alice/status" {
			t.Errorf("unexpected mutation: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"alice","phase":"Ready","containerState":"running"}`)
	}))
	defer s.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", s.URL)
	cmd := workerWaitReadyCmd()
	cmd.SetArgs([]string{"--name", "alice", "--timeout", "1s", "-o", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestWorkerWaitReadyStopsOnFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"alice","phase":"Failed","message":"image not found"}`)
	}))
	defer s.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", s.URL)
	cmd := workerWaitReadyCmd()
	cmd.SetArgs([]string{"--name", "alice", "--timeout", "1s"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("startup failure reported ready")
	}
}
