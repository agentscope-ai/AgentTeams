package server

import (
	"context"
	"encoding/json"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The real Python Recovery client crosses the real Go execution API. Only the
// object storage backend and mc upload transport use isolated test fixtures.
func TestNativeRecoveryPythonJournalRoundTrip(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python native bridge unavailable")
	}
	store := ossfake.NewMemory()
	project := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	putTask(store, taskMetaKey, taskMeta(map[string]any{"assigned_to": "alpha-dev", "status": "in_progress", "task_kind": "execution", "project_id": "", "spec": "Continue the assigned work", "execution": map[string]any{"generation": 1, "instance_id": "instance", "process_id": "process", "state": "active"}}))
	recovery := NewTaskRecoveryHandler(project, ServerDeps{})
	project.recovery = recovery
	recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return "instance", "openclaw", nil
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks/{id}/begin", func(w http.ResponseWriter, r *http.Request) { project.BeginTask(w, withCaller(r, caller)) })
	mux.HandleFunc("GET /api/v1/tasks/recoverable", func(w http.ResponseWriter, r *http.Request) { recovery.ListRecoverableTasks(w, withCaller(r, caller)) })
	mux.HandleFunc("POST /api/v1/tasks/{id}/execution", func(w http.ResponseWriter, r *http.Request) { recovery.UpdateExecution(w, withCaller(r, caller)) })
	mux.HandleFunc("POST /fixture-artifact", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if err := store.PutObject(r.Context(), r.Header.Get("X-Fixture-Key"), data); err != nil {
			t.Error(err)
			w.WriteHeader(500)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("own-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	library, err := filepath.Abs("../../../shared/lib")
	if err != nil {
		t.Fatal(err)
	}
	script := `import os,sys,urllib.request
from unittest.mock import patch
sys.path.insert(0,sys.argv[1])
from task_recovery import Recovery,RecoveryConflict,RecoveryUnavailable
r=Recovery()
i=dict(task_id='t1',team_id='alpha-team',generation=1,instance_id='instance',process_id='process')
op=r.begin_tools(i,[dict(id='call',name='exec',arguments=dict(command='own synthetic command'))])
assert r.current('t1','alpha-team')['execution']['state']=='needs_verification'
try:
 r.tools_current(i)
 raise AssertionError('normal admission passed unverified side effects')
except RecoveryConflict:
 pass
def save(command,**kwargs):
 key=command[-1].removeprefix('storage/')
 req=urllib.request.Request(os.environ['AGENTTEAMS_CONTROLLER_URL']+'/fixture-artifact',data=kwargs['input'],headers={'X-Fixture-Key':key},method='POST')
 with urllib.request.urlopen(req) as response: assert response.status==200
with patch('task_recovery.subprocess.run',side_effect=save):
 r.finish_tools(i,op,[dict(id='call',content=[dict(type='text',text='confirmed output')])])
assert r.tools_current(i)['execution']['state']=='active'
# A delivered progress response is not submission of the assigned Task.
with patch('task_recovery.subprocess.run',side_effect=save):
 r.save_reply('t1','alpha-team','The browser environment still needs setup.')
r.delivered('t1','alpha-team','$progress')
req=urllib.request.Request(os.environ['AGENTTEAMS_CONTROLLER_URL']+'/api/v1/tasks/t1/begin',data=b'{}',headers={'X-AgentTeams-Process-ID':'process'},method='POST')
with urllib.request.urlopen(req) as response: assert response.status==200
previous=r.current('t1','alpha-team')['execution']['checkpoint']
next_call=dict(id='next-call',name='exec',arguments=dict(command='continue verified work'))
op=r.begin_tools(i,[next_call])
started=r.current('t1','alpha-team')['execution']['checkpoint']
assert 'result' not in started and 'delivery' not in started and 'reply_txn' not in started
assert started['completed_steps']==previous['completed_steps']
assert started['artifacts']==previous['artifacts']
with patch('task_recovery.subprocess.run',side_effect=save):
 r.finish_tools(i,op,[dict(id='next-call',content=[dict(type='text',text='continued work')])])
assert r.current('t1','alpha-team')['status']=='in_progress'
# Go encodes an empty remaining_steps slice as null, including after a real
# native tool return. The managed model projection must still be resumable.
current=r.current('t1','alpha-team')
assert current['execution']['checkpoint']['remaining_steps'] is None
current['task_plan_id']='fixture-plan'
assert 'common task protocol' in Recovery.resume_text(current)
# A native return survives a receipt GET failure; a later real Core claim
# validates and commits that same return without another native execution.
op=r.begin_tools(i,[dict(id='receipt-outage',name='exec',arguments=dict(command='already executed'))])
with patch.object(r,'current',side_effect=RecoveryUnavailable('receipt connection failed')):
 try:
  r.finish_tools(i,op,[dict(id='receipt-outage',content='original native return')])
  raise AssertionError('fixture outage was not observed')
 except RecoveryUnavailable:
  pass
snapshot=r.discover()
entry=next(row for row in snapshot['tasks'] if row['task']['task_id']=='t1')
with patch('task_recovery.subprocess.run',side_effect=save):
 recovered=r.claim(snapshot,entry)
assert recovered['task']['execution']['state']=='active'
checkpoint=recovered['task']['execution']['checkpoint']
assert checkpoint['operation']['state']=='verified'
assert checkpoint['completed_steps'].count(op)==1
`
	command := exec.Command(python, "-c", script, library)
	command.Env = append(os.Environ(), "AGENTTEAMS_PROCESS_ID=process", "AGENTTEAMS_CONTROLLER_URL="+server.URL, "AGENTTEAMS_AUTH_TOKEN_FILE="+token, "AGENTTEAMS_STORAGE_PREFIX=storage", "TEAMHARNESS_RUNTIME_CONFIG="+filepath.Join(t.TempDir(), "runtime.yaml"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python/Go journal boundary: %v\n%s", err, output)
	}
	data, _ := store.GetObject(t.Context(), taskMetaKey)
	var task map[string]json.RawMessage
	_ = json.Unmarshal(data, &task)
	var execution taskExecution
	_ = json.Unmarshal(task["execution"], &execution)
	if execution.State != "active" || execution.Checkpoint.Operation.State != "verified" {
		t.Fatalf("unfinished checkpoint: %+v", execution)
	}
}

func TestNativeMessagePythonReturnRecovery(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python native bridge unavailable")
	}
	f := newMatrixExecutionFixture(t, nil)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/runtime/matrix/events/{key}/execution", func(w http.ResponseWriter, r *http.Request) { f.h.GetExecution(w, withCaller(r, caller)) })
	mux.HandleFunc("POST /api/v1/runtime/matrix/events/{key}/execution", func(w http.ResponseWriter, r *http.Request) { f.h.UpdateExecution(w, withCaller(r, caller)) })
	mux.HandleFunc("POST /fixture-artifact", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if err := f.storage.PutObject(r.Context(), r.Header.Get("X-Fixture-Key"), data); err != nil {
			t.Error(err)
			w.WriteHeader(500)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("private-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	library, err := filepath.Abs("../../../shared/lib")
	if err != nil {
		t.Fatal(err)
	}
	script := `import os,sys,urllib.request
from unittest.mock import patch
sys.path.insert(0,sys.argv[1])
from task_recovery import RecoveryUnavailable
from worker_matrix_state import WorkerMatrix
m=WorkerMatrix('hermes')
key=sys.argv[2]
identity=m.message_claim(key)['identity']
op=m.message_begin_tools(identity,[dict(id='native-call',name='external',arguments=dict(value='one'))])
with patch.object(m,'message_pending_tools',side_effect=RecoveryUnavailable('receipt connection failed')):
 try:
  m.message_finish_tools(identity,op,[dict(id='native-call',content='original return')])
  raise AssertionError('fixture outage was not observed')
 except RecoveryUnavailable:
  pass
def save(command,**kwargs):
 req=urllib.request.Request(os.environ['AGENTTEAMS_CONTROLLER_URL']+'/fixture-artifact',data=kwargs['input'],headers={'X-Fixture-Key':command[-1].removeprefix('storage/')},method='POST')
 with urllib.request.urlopen(req) as response: assert response.status==200
with patch('worker_matrix_state.subprocess.run',side_effect=save):
 recovered=m.message_claim(key)
assert recovered['recovery']=='resume'
assert recovered['execution']['checkpoint']['operation']['state']=='verified'
assert recovered['execution']['checkpoint']['completed_steps'].count(op)==1
`
	command := exec.Command(python, "-c", script, library, f.key)
	command.Env = append(os.Environ(), "AGENTTEAMS_PROCESS_ID="+f.start.ProcessID, "AGENTTEAMS_CONTROLLER_URL="+server.URL, "AGENTTEAMS_AUTH_TOKEN_FILE="+token, "AGENTTEAMS_STORAGE_PREFIX=storage", "TEAMHARNESS_RUNTIME_CONFIG="+filepath.Join(t.TempDir(), "runtime.yaml"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python/Go message return recovery: %v\n%s", err, output)
	}
}
