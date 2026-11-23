package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskCreateGetsOriginalRouteFromNativeContext(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks" || json.NewDecoder(r.Body).Decode(&input) != nil || input["leader"] != "lead" || input["title"] != "Request" || input["spec"] != "Original criteria" || len(input) != 3 {
			t.Errorf("invalid create request: %s %s %+v", r.Method, r.URL, input)
		}
		if r.Header.Get("X-AgentTeams-Origin-Event-ID") != "$request" || r.Header.Get("X-AgentTeams-Requester-User-ID") != "@human:test" || r.Header.Get("X-AgentTeams-Requester-Room-ID") != "!source:test" {
			t.Error("missing native source identity")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"sourceEventId":"$request","task":{"id":"main","kind":"main","status":"planned"},"notification":{"status":"queued","eventId":"handoff","recipient":"lead"}}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_ORIGIN_EVENT_ID", "$request")
	t.Setenv("AGENTTEAMS_REQUESTER_USER_ID", "@human:test")
	t.Setenv("AGENTTEAMS_REQUESTER_ROOM_ID", "!source:test")
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "create", "--leader", "lead", "--title", "Request", "--spec", "Original criteria"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !json.Valid(output.Bytes()) {
		t.Fatalf("invalid creation receipt: %d %s", calls, output.String())
	}
}

func TestTaskCreateCanCaptureOriginalRequestWithoutSpecFlag(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		if json.NewDecoder(r.Body).Decode(&input) != nil || input["title"] != "Request" || input["leader"] != "lead" || input["spec"] != "" || r.Header.Get("X-AgentTeams-Origin-Event-ID") != "$request" {
			t.Errorf("source capture request changed: %+v", input)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task":{"id":"main","spec":"Server captured original request"}}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_ORIGIN_EVENT_ID", "$request")
	t.Setenv("AGENTTEAMS_REQUESTER_USER_ID", "@human:test")
	t.Setenv("AGENTTEAMS_REQUESTER_ROOM_ID", "!source:test")
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "create", "--leader", "lead", "--title", "Request"})
	if err := cmd.Execute(); err != nil || calls != 1 || !json.Valid(output.Bytes()) {
		t.Fatalf("no-spec creation failed: calls=%d err=%v output=%s", calls, err, output.String())
	}
}

func TestTaskCreateChecksSelectedInputAgainstNativeBinding(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-AgentTeams-Origin-Event-ID") != "$second" {
			t.Error("selected input lost its native source")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sourceEventId":"$second"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_ORIGIN_EVENT_ID", "$second")
	t.Setenv("AGENTTEAMS_REQUESTER_USER_ID", "@human:test")
	t.Setenv("AGENTTEAMS_REQUESTER_ROOM_ID", "!source:test")
	t.Setenv("AGENTTEAMS_INPUT_ID", "input-2")
	for _, input := range []string{"input-1", "input-2"} {
		cmd := newRootCommand("agt")
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"task", "create", "--leader", "lead", "--title", "Request", "--input-id", input})
		err := cmd.Execute()
		if (err == nil) != (input == "input-2") {
			t.Fatalf("input=%s err=%v", input, err)
		}
	}
	if calls != 1 {
		t.Fatalf("unbound input caused a side effect: %d requests", calls)
	}
}

func TestTaskCreateBindsRelatedInputsBeforeSendingHTTP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Related []string `json:"relatedSources"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Related) != 1 || body.Related[0] != "$change" {
			t.Errorf("lost related native source: %+v", body)
		}
		_, _ = w.Write([]byte(`{"sourceEventId":"$original"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_ORIGIN_EVENT_ID", "$original")
	t.Setenv("AGENTTEAMS_REQUESTER_USER_ID", "@a:test")
	t.Setenv("AGENTTEAMS_REQUESTER_ROOM_ID", "!room:test")
	t.Setenv("AGENTTEAMS_INPUT_ID", "input-1")
	t.Setenv("AGENTTEAMS_RELATED_INPUTS", `{"input-2":"$change"}`)
	for _, ref := range []string{"input-unknown", "input-2"} {
		cmd := newRootCommand("agt")
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"task", "create", "--leader", "lead", "--title", "Report", "--input-id", "input-1", "--related-input-id", ref})
		err := cmd.Execute()
		if (err == nil) != (ref == "input-2") {
			t.Fatalf("ref=%s err=%v", ref, err)
		}
	}
	if calls != 1 {
		t.Fatalf("unbound amendment caused HTTP side effect: %d", calls)
	}
}

func TestTaskCreatePreservesTypedNoEffectInputRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-AgentTeams-Input-Frontier") != "$frontier" {
			t.Error("native input frontier lost")
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true,"outcome":"input_changed","noTaskRegistered":true,"sourceEventId":"$original","inputs":[{"eventId":"$new","userId":"@a:test","roomId":"!room:test","body":"Cancel"}]}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_ORIGIN_EVENT_ID", "$original")
	t.Setenv("AGENTTEAMS_REQUESTER_USER_ID", "@a:test")
	t.Setenv("AGENTTEAMS_REQUESTER_ROOM_ID", "!room:test")
	t.Setenv("AGENTTEAMS_INPUT_FRONTIER", "$frontier")
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "create", "--leader", "lead", "--title", "Report"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(output.Bytes(), &result) != nil || result["ok"] != true || result["outcome"] != "input_changed" {
		t.Fatalf("typed refresh lost: %s", output.String())
	}
}
