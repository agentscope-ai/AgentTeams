package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func (f *matrixExecutionFixture) config(generation int) {
	f.t.Helper()
	doc := map[string]any{"apiVersion": "agentteams.io/v1beta1", "kind": "MemberRuntimeConfig",
		"metadata": map[string]any{"generation": generation}, "member": map[string]any{"runtimeName": "runtime-alice", "runtime": f.start.Runtime},
		"storage": map[string]any{"memberPrefix": "agents/runtime-alice"}}
	data, _ := json.Marshal(doc)
	if err := f.storage.PutObject(f.t.Context(), "agents/runtime-alice/runtime/runtime.yaml", data); err != nil {
		f.t.Fatal(err)
	}
}

func (f *matrixExecutionFixture) session(method, query string, body any, etag string) *httptest.ResponseRecorder {
	f.t.Helper()
	data, _ := json.Marshal(body)
	r := withCaller(httptest.NewRequest(method, "/api/v1/runtime/sessions?"+query, strings.NewReader(string(data))),
		&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice", Team: "alpha-team"})
	r.Header.Set("X-AgentTeams-Process-ID", f.start.ProcessID)
	if etag == "*" {
		r.Header.Set("If-None-Match", "*")
	} else if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	w := httptest.NewRecorder()
	if method == http.MethodGet {
		f.h.GetSession(w, r)
	} else {
		f.h.RememberSession(w, r)
	}
	return w
}

func sessionBody(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != 200 {
		t.Fatalf("session request: %d %s", response.Code, response.Body)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *matrixExecutionFixture) cacheRequest(view map[string]any) map[string]any {
	return map[string]any{"instance_id": f.start.InstanceID, "process_id": f.start.ProcessID,
		"generation": view["generation"], "checkpoint_version": view["checkpoint_version"], "checkpoint_digest": view["checkpoint_digest"],
		"config_generation": view["config_generation"], "native_ref": "native-session-1", "native_digest": strings.Repeat("a", 64), "expires_at": time.Now().Add(time.Hour).Unix()}
}

func TestWorkerSessionCacheTracksAuthorityWithoutOverwritingIt(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.config(5)
	f.expect("claim", nil, 200)
	f.expect("checkpoint", &taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two"}}, 200)
	query := "kind=message&event_key=" + f.key
	view := sessionBody(t, f.session("GET", query, nil, ""))
	if view["compatible"] != false || view["cache"] != nil {
		t.Fatal("missing cache was reusable")
	}
	before, _ := f.storage.GetObject(t.Context(), f.objectKey())
	request := f.cacheRequest(view)
	saved := sessionBody(t, f.session("PUT", query, request, "*"))
	if saved["compatible"] != true {
		t.Fatal("current persisted descriptor was not a candidate")
	}
	after, _ := f.storage.GetObject(t.Context(), f.objectKey())
	if string(before) != string(after) {
		t.Fatal("session descriptor changed authoritative message execution")
	}
	f.expect("checkpoint", &taskCheckpoint{Version: 2, CompletedSteps: []string{"one", "two"}, RemainingSteps: []string{}}, 200)
	view = sessionBody(t, f.session("GET", query, nil, ""))
	if view["compatible"] != false {
		t.Fatal("old native cache overrode a newer checkpoint")
	}
	if r := f.session("PUT", query, request, saved["etag"].(string)); r.Code != 409 {
		t.Fatalf("stale checkpoint callback: %d %s", r.Code, r.Body)
	}
	request = f.cacheRequest(view)
	sessionBody(t, f.session("PUT", query, request, saved["etag"].(string)))
	f.config(6)
	if view := sessionBody(t, f.session("GET", query, nil, "")); view["compatible"] != false {
		t.Fatal("configuration change reused a descriptor for old applied settings")
	}
}

func TestWorkerSessionCacheRejectsStaleWritersAndForeignNativeCache(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.config(1)
	f.expect("claim", nil, 200)
	query := "kind=message&event_key=" + f.key
	view := sessionBody(t, f.session("GET", query, nil, ""))
	request := f.cacheRequest(view)
	saved := sessionBody(t, f.session("PUT", query, request, "*"))
	if r := f.session("PUT", query, request, "*"); r.Code != 409 {
		t.Fatalf("create overwrote native descriptor: %d", r.Code)
	}
	request["native_digest"] = strings.Repeat("A", 64)
	if r := f.session("PUT", query, request, saved["etag"].(string)); r.Code != 400 {
		t.Fatalf("invalid digest accepted: %d %s", r.Code, r.Body)
	}
	request["native_digest"] = strings.Repeat("a", 64)
	f.replace()
	f.config(2)
	f.expect("claim", nil, 200)
	view = sessionBody(t, f.session("GET", query, nil, ""))
	if view["cache"] != nil || view["compatible"] != false {
		t.Fatal("different Harness received source native cache")
	}
	request["process_id"] = f.start.ProcessID
	if r := f.session("PUT", query, request, "*"); r.Code != 409 {
		t.Fatalf("old process wrote a new Harness cache: %d %s", r.Code, r.Body)
	}
	request = f.cacheRequest(view)
	request["process_id"] = "other-process"
	if r := f.session("PUT", query, request, "*"); r.Code != 400 {
		t.Fatalf("process header/body disagreement: %d", r.Code)
	}
	f.expect("cancel", nil, 200)
	if r := f.session("GET", query, nil, ""); r.Code != 409 {
		t.Fatalf("cancelled work offered for native session reuse: %d", r.Code)
	}
}

func TestWorkerTaskSessionUsesExistingTaskAuthorityAndAssignment(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.config(2)
	f.h.recovery.project = newProjectTestHandler(t, f.storage, teamWithRoster("alpha-team"))
	key := "teams/alpha-team/shared/tasks/t1/meta.json"
	execution := map[string]any{"generation": 8, "instance_id": f.start.InstanceID, "process_id": f.start.ProcessID, "runtime": f.start.Runtime, "state": "active",
		"checkpoint": json.RawMessage(`{"version":4,"completed_steps":["one"],"remaining_steps":["two"]}`)}
	meta := taskMeta(map[string]any{"assigned_to": "runtime-alice", "execution": execution})
	putTask(f.storage, key, meta)
	query := "kind=task&team=alpha-team&task=t1"
	view := sessionBody(t, f.session("GET", query, nil, ""))
	if view["checkpoint_digest"] != workerCheckpointDigest(execution["checkpoint"].(json.RawMessage)) {
		t.Fatal("checkpoint hashing changed the authoritative stored shape")
	}
	before, _ := f.storage.GetObject(t.Context(), key)
	sessionBody(t, f.session("PUT", query, f.cacheRequest(view), "*"))
	after, _ := f.storage.GetObject(t.Context(), key)
	if string(before) != string(after) {
		t.Fatal("Task session cache created parallel authority or rewrote Task")
	}
	// A delivered turn is terminal for ordinary messages, but a Task may still
	// be in_progress with remaining steps. Its latest session can be cached.
	execution["checkpoint"] = json.RawMessage(`{"version":5,"completed_steps":["one"],"remaining_steps":["two"],"delivery":"delivered"}`)
	putTask(f.storage, key, meta)
	sessionBody(t, f.session("GET", query, nil, ""))
	meta["assigned_to"] = "bob"
	putTask(f.storage, key, meta)
	if r := f.session("GET", query, nil, ""); r.Code != 403 {
		t.Fatalf("reassigned Task cache remains readable: %d %s", r.Code, r.Body)
	}
	meta["assigned_to"], meta["status"] = "runtime-alice", "cancelled"
	putTask(f.storage, key, meta)
	if r := f.session("GET", query, nil, ""); r.Code != 409 {
		t.Fatalf("cancelled Task cache remains reusable: %d", r.Code)
	}
}

func TestWorkerSessionRestartReuseAndReturnToOldHarness(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.config(1)
	f.expect("claim", nil, 200)
	cp := &taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two"}}
	f.expect("checkpoint", cp, 200)
	query := "kind=message&event_key=" + f.key
	view := sessionBody(t, f.session("GET", query, nil, ""))
	sessionBody(t, f.session("PUT", query, f.cacheRequest(view), "*"))
	start := func(instance, process, runtime string, config int) {
		t.Helper()
		_, etag, err := f.h.store.Load(t.Context(), "runtime-alice")
		if err != nil {
			t.Fatal(err)
		}
		f.start = service.WorkerStart{InstanceID: instance, ProcessID: process, Runtime: runtime}
		if _, _, err := f.h.store.Start(t.Context(), "runtime-alice", f.start, etag); err != nil {
			t.Fatal(err)
		}
		f.config(config)
		f.expect("claim", nil, 200)
	}
	start("actual/restart", "second-process", "hermes", 1)
	if view := sessionBody(t, f.session("GET", query, nil, "")); view["compatible"] != true || view["generation"] != float64(2) {
		t.Fatal("same Harness restart unnecessarily discarded a matching native cache")
	}
	start("actual/qwen", "third-process", "qwenpaw", 2)
	cp.Version, cp.CompletedSteps, cp.RemainingSteps = 2, []string{"one", "two"}, []string{"three"}
	f.expect("checkpoint", cp, 200)
	start("actual/return", "fourth-process", "hermes", 3)
	view = sessionBody(t, f.session("GET", query, nil, ""))
	if view["cache"] == nil || view["compatible"] != false || view["checkpoint_version"] != float64(2) {
		t.Fatal("returning Harness either deleted its old native cache or accepted its stale checkpoint")
	}
}

func TestWorkerSessionRequiresCurrentConfigAndPreservesInvalidRecords(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	query := "kind=message&event_key=" + f.key
	if r := f.session("GET", query, nil, ""); r.Code != 503 {
		t.Fatalf("missing public config fell back to private settings: %d", r.Code)
	}
	f.config(1)
	view := sessionBody(t, f.session("GET", query, nil, ""))
	request := f.cacheRequest(view)
	request["expires_at"] = 1
	if r := f.session("PUT", query, request, "*"); r.Code != 400 {
		t.Fatalf("expired session was saved: %d", r.Code)
	}
	key := "agents/runtime-alice/runtime/sessions/hermes/" + service.WorkerMessageSessionKey("agents/runtime-alice", "!room:test", "$input") + ".json"
	corrupt := []byte(`{"schema_version":999,"kind":"WorkerSessionCache"}`)
	if err := f.storage.PutObject(t.Context(), key, corrupt); err != nil {
		t.Fatal(err)
	}
	if r := f.session("PUT", query, f.cacheRequest(view), "*"); r.Code != 503 {
		t.Fatalf("unsupported cache record was silently reset: %d", r.Code)
	}
	data, _ := f.storage.GetObject(t.Context(), key)
	if string(data) != string(corrupt) {
		t.Fatal("unsupported record was overwritten")
	}
}

func TestWorkerCheckpointDigestMatchesPythonRecovery(t *testing.T) {
	// Python Recovery.digest goldens include Unicode, HTML, surrogate pairs and
	// integers beyond JavaScript's exact range. Do not round-trip through float64.
	for raw, canonical := range map[string]string{
		`null`: `null`,
		`{"version":9007199254740993,"text":"中文<&>😀\u2028"}`: `{"text":"\u4e2d\u6587<&>\ud83d\ude00\u2028","version":9007199254740993}`,
	} {
		sum := sha256.Sum256([]byte(canonical))
		if got := workerCheckpointDigest(json.RawMessage(raw)); got != hex.EncodeToString(sum[:]) {
			t.Fatalf("Python checkpoint digest mismatch: %s %s", raw, got)
		}
	}
}
