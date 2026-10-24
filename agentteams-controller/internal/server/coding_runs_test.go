package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newCodingRunsTestHandler builds a CodingRunsHandler over an in-memory mc-like
// store and a fake K8s client holding the given Teams (reuses the project test
// scheme / mcLikeOSS directory-view wrapper / team helper).
func newCodingRunsTestHandler(t *testing.T, store *ossfake.Memory, teams ...*v1beta1.Team) *CodingRunsHandler {
	t.Helper()
	objs := make([]runtime.Object, 0, len(teams))
	for _, tm := range teams {
		objs = append(objs, tm)
	}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	return NewCodingRunsHandler(k8s, "default", &mcLikeOSS{Memory: store})
}

func putRun(store *ossfake.Memory, key, data string) {
	_ = store.PutObject(context.Background(), key, []byte(data))
}

const (
	teamRunKey  = "teams/alpha-team/shared/tasks/t1/workspace/coding-cli-logs/run-20261012-120000.log"
	standRunKey = "shared/tasks/t9/workspace/coding-cli-logs/run-20261012-120100.log"
	betaRunKey  = "teams/beta-team/shared/tasks/t2/workspace/coding-cli-logs/run-20261012-120200.log"
)

func TestListCodingRuns_AdminSeesTeamAndStandalone(t *testing.T) {
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "alpha t1 run")
	putRun(store, standRunKey, "standalone t9 run")
	putRun(store, betaRunKey, "beta t2 run")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"), team("beta-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListCodingRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var runs []codingRun
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	// Admin sees team (alpha, beta) + standalone runs: 3.
	if len(runs) != 3 {
		t.Fatalf("admin runs = %d, want 3: %+v", len(runs), runs)
	}
}

func TestListCodingRuns_L2SeesOnlyOwnTeam(t *testing.T) {
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "alpha t1 run")
	putRun(store, standRunKey, "standalone t9 run")
	putRun(store, betaRunKey, "beta t2 run")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"), team("beta-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.ListCodingRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var runs []codingRun
	_ = json.Unmarshal(rec.Body.Bytes(), &runs)
	// L2 alpha-leader sees only the alpha-team run; standalone (no team) and
	// beta-team run are excluded.
	if len(runs) != 1 {
		t.Fatalf("L2 runs = %d, want 1 (alpha only): %+v", len(runs), runs)
	}
	if runs[0].TaskID != "t1" || runs[0].Team != "alpha-team" {
		t.Fatalf("L2 run = %+v, want task t1 team alpha-team", runs[0])
	}
}

func TestListCodingRuns_SkipsNonRunFiles(t *testing.T) {
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "a run")
	putRun(store, "teams/alpha-team/shared/tasks/t1/workspace/coding-cli-logs/.gitkeep", "x")
	putRun(store, "teams/alpha-team/shared/tasks/t1/workspace/coding-cli-logs/run-notes.txt", "y")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListCodingRuns(rec, req)

	var runs []codingRun
	_ = json.Unmarshal(rec.Body.Bytes(), &runs)
	if len(runs) != 1 || runs[0].Run != "run-20261012-120000.log" {
		t.Fatalf("runs = %+v, want only the well-formed run", runs)
	}
}

func getRun(h *CodingRunsHandler, taskID, run string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs/"+taskID+"/runs/"+run, nil)
	req.SetPathValue("taskID", taskID)
	req.SetPathValue("run", run)
	if caller != nil {
		req = withCaller(req, caller)
	}
	rec := httptest.NewRecorder()
	h.GetCodingRun(rec, req)
	return rec
}

func TestGetCodingRun_AdminReadsByteTransparent(t *testing.T) {
	store := ossfake.NewMemory()
	body := "[run-coding-cli] cli=qwen ...\n<full transcript>"
	putRun(store, teamRunKey, body)
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	rec := getRun(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != body {
		t.Fatalf("body = %q, want byte-transparent %q", got, body)
	}
	// Default (no ?format) stays byte-transparent text/plain.
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/plain; charset=utf-8 (default unchanged)", ct)
	}
}

const (
	runsJSONLKey = "teams/alpha-team/shared/tasks/t1/workspace/coding-cli-logs/runs.jsonl"
	promptKey    = "teams/alpha-team/shared/tasks/t1/coding-prompts/20261012-120000.txt"
)

// fullRecordLine is a runs.jsonl line (the #1342 wrapper's schema) whose
// prompt_file basename matches promptKey, for the ?format=json full-metadata
// case.
const fullRecordLine = `{"v":1,"run":"run-20261012-120000.log","cli":"qwen","worker":"sysdev-coder","runner":"sysdev-lead","status":"ok","exit_code":0,"timeout_secs":3600,"started_at":"2026-10-12T11:59:58Z","ended_at":"2026-10-12T12:00:00Z","log_bytes":42,"prompt_file":"/root/agentteams-fs/shared/tasks/t1/coding-prompts/20261012-120000.txt","prompt_sha256":"abc123sha","prompt_summary":"do the thing"}`

func TestListCodingRuns_EnrichedFromRunsJSONL(t *testing.T) {
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "alpha t1 run")
	// A second run in the same task with no runs.jsonl record — its degraded
	// fields are asserted alongside the enriched one.
	putRun(store, "teams/alpha-team/shared/tasks/t1/workspace/coding-cli-logs/run-20261012-130000.log", "alpha t1 second run")
	putRun(store, runsJSONLKey, fullRecordLine)
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListCodingRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var runs []codingRun
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2: %+v", len(runs), runs)
	}
	// Files list in name order: the recorded run first.
	enriched := runs[0]
	if enriched.Run != "run-20261012-120000.log" {
		t.Fatalf("runs[0] = %q, want the recorded run first: %+v", enriched.Run, runs)
	}
	if enriched.Cli != "qwen" || enriched.Worker != "sysdev-coder" || enriched.Status != "ok" {
		t.Fatalf("enriched metadata = %+v, want cli=qwen worker=sysdev-coder status=ok", enriched)
	}
	if enriched.ExitCode == nil || *enriched.ExitCode != 0 {
		t.Fatalf("exit_code = %v, want 0 shown (0 is a legal value, not 'unknown')", enriched.ExitCode)
	}
	if enriched.StartedAt != "2026-10-12T11:59:58Z" {
		t.Fatalf("started_at = %q, want the record's value (not the filename fallback)", enriched.StartedAt)
	}
	if enriched.EndedAt != "2026-10-12T12:00:00Z" || enriched.LogBytes == nil || *enriched.LogBytes != 42 {
		t.Fatalf("ended_at/log_bytes = %q/%v, want the record's values", enriched.EndedAt, enriched.LogBytes)
	}
	// The run without a record degrades to the base fields only: no metadata,
	// and no invented timestamp (the filename's timezone is unknowable — see
	// TestListCodingRuns_LocalFilenameIsNotReadAsUTC).
	fallback := runs[1]
	if fallback.Run != "run-20261012-130000.log" {
		t.Fatalf("runs[1] = %+v, want the unrecorded run", fallback)
	}
	if fallback.Cli != "" || fallback.Worker != "" || fallback.Status != "" ||
		fallback.ExitCode != nil || fallback.EndedAt != "" || fallback.LogBytes != nil ||
		fallback.StartedAt != "" {
		t.Fatalf("fallback run must not carry record metadata or a fabricated start: %+v", fallback)
	}
}

func TestListCodingRuns_LocalFilenameIsNotReadAsUTC(t *testing.T) {
	// run-coding-cli.sh names transcripts with the CONTAINER clock
	// (`date +%Y%m%d-%H%M%S`), so the filename is a LOCAL wall-clock stamp,
	// not a UTC instant — a "2026-10-15 23:00" name from an Asia/Shanghai
	// container is 15:00Z. Without a runs.jsonl record the container's zone is
	// unknowable, so started_at must stay absent rather than assert a
	// fabricated UTC instant (review finding: do not reinterpret legacy local
	// filenames using the controller's timezone).
	store := ossfake.NewMemory()
	putRun(store, "teams/alpha-team/shared/tasks/t1/workspace/coding-cli-logs/run-20261015-230000.log", "legacy local-time run")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListCodingRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var runs []codingRun
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1: %+v", len(runs), runs)
	}
	if runs[0].StartedAt != "" {
		t.Fatalf("started_at = %q, want empty: a local filename must not be asserted as UTC", runs[0].StartedAt)
	}
	// Unknown metadata keys are omitted from the wire (omitempty contract).
	var rawList []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rawList); err != nil {
		t.Fatalf("decode raw list: %v", err)
	}
	for _, key := range []string{"cli", "worker", "status", "exit_code", "started_at", "ended_at", "log_bytes"} {
		if _, ok := rawList[0][key]; ok {
			t.Fatalf("key %q must be omitted when unknown, got: %v", key, rawList[0])
		}
	}
}

// ── blocked attempts (review: recorded but must also be readable) ───────────

// A blocked attempt (the pre-flight guards refuse to run the CLI) is a
// first-class record, not an index-only ghost: the writer leaves a diagnostic
// transcript beside its index line, so the read API sees it like any other
// run — on the list and in the detail. Regression for the review finding that
// these attempts were mirrored but invisible (no transcript object).
func TestBlockedAttemptVisibleToListAndDetail(t *testing.T) {
	const diagnostic = "[run-coding-cli] cli=qwen workspace=… timeout=3600s\n" +
		"[run-coding-cli] ERROR: …/.processing is held by 'alice'\n" +
		"[run-coding-cli] removes the marker or it expires.\n"
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, diagnostic) // teamRunKey is t1/run-20261012-120000.log
	putRun(store, runsJSONLKey, `{"v":1,"run":"run-20261012-120000.log","cli":"qwen","worker":"sysdev-coder","status":"blocked","reason":"marker-conflict","exit_code":125,"timeout_secs":3600,"started_at":"2026-10-12T11:59:58Z","ended_at":"2026-10-12T11:59:58Z","log_bytes":214,"prompt_file":"/root/agentteams-fs/shared/tasks/t1/coding-prompts/20261012-120000.txt","prompt_sha256":"abc123sha","prompt_summary":"do the thing"}`)
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListCodingRuns(rec, req)
	var runs []codingRun
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != "blocked" || runs[0].ExitCode == nil || *runs[0].ExitCode != 125 {
		t.Fatalf("blocked attempt must be listed with status=blocked exit_code=125: %+v", runs)
	}
	if runs[0].LogBytes == nil || *runs[0].LogBytes != 214 {
		t.Fatalf("blocked attempt log_bytes = %v, want the transcript size 214", runs[0].LogBytes)
	}

	// Detail: the default text route serves the diagnostic transcript…
	recText := getRun(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if recText.Code != http.StatusOK || !strings.Contains(recText.Body.String(), "is held by") {
		t.Fatalf("blocked detail (text) = %d %q, want the diagnostic transcript", recText.Code, recText.Body.String())
	}
	// …and the JSON envelope names the outcome plus the machine-readable
	// reason, with the transcript still carried verbatim.
	recJSON := getRunJSON(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if recJSON.Code != http.StatusOK {
		t.Fatalf("blocked detail (json) status = %d: %s", recJSON.Code, recJSON.Body.String())
	}
	var d codingRunDetail
	if err := json.Unmarshal(recJSON.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if d.Meta.Status != "blocked" || d.Meta.Reason != "marker-conflict" || d.Meta.ExitCode == nil || *d.Meta.ExitCode != 125 {
		t.Fatalf("blocked meta = %+v, want status/reason/exit_code", d.Meta)
	}
	if !strings.Contains(d.Output.Transcript, "is held by") || d.Output.Transcript != diagnostic {
		t.Fatalf("blocked output.transcript = %q, want the diagnostic verbatim", d.Output.Transcript)
	}
}

func TestListCodingRuns_MalformedRunsJSONLIsTolerated(t *testing.T) {
	// Bad lines (garbage, a record without a run name, a blank line) are
	// skipped; the one good line is still applied.
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "alpha t1 run")
	putRun(store, runsJSONLKey, "not json at all\n{\"v\":1}\n\n"+`{"v":1,"run":"run-20261012-120000.log","cli":"qwen","status":"failed","exit_code":3}`+"\n")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListCodingRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("malformed runs.jsonl must not fail the list: status = %d: %s", rec.Code, rec.Body.String())
	}
	var runs []codingRun
	_ = json.Unmarshal(rec.Body.Bytes(), &runs)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1: %+v", len(runs), runs)
	}
	if runs[0].Cli != "qwen" || runs[0].Status != "failed" || runs[0].ExitCode == nil || *runs[0].ExitCode != 3 {
		t.Fatalf("the good line must be applied, bad lines skipped: %+v", runs[0])
	}
}

func getRunJSON(h *CodingRunsHandler, taskID, run string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs/"+taskID+"/runs/"+run+"?format=json", nil)
	req.SetPathValue("taskID", taskID)
	req.SetPathValue("run", run)
	if caller != nil {
		req = withCaller(req, caller)
	}
	rec := httptest.NewRecorder()
	h.GetCodingRun(rec, req)
	return rec
}

func TestGetCodingRun_JSONFormat_FullMetadata(t *testing.T) {
	store := ossfake.NewMemory()
	body := "[run-coding-cli] cli=qwen workspace=/root/agentteams-fs/shared/tasks/t1/workspace timeout=3600s\n<full transcript>"
	putRun(store, teamRunKey, body)
	putRun(store, runsJSONLKey, fullRecordLine)
	const promptBody = "fix the parser\nplease be careful"
	putRun(store, promptKey, promptBody)
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	rec := getRunJSON(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var d codingRunDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	// Envelope.
	if d.TaskID != "t1" || d.Run != "run-20261012-120000.log" || d.Team != "alpha-team" {
		t.Fatalf("envelope = %+v, want task t1 / run / team alpha-team", d)
	}
	// Meta (from the runs.jsonl record, fields pass-through).
	if d.Meta.Cli != "qwen" || d.Meta.Worker != "sysdev-coder" || d.Meta.Runner != "sysdev-lead" ||
		d.Meta.Status != "ok" || d.Meta.ExitCode == nil || *d.Meta.ExitCode != 0 ||
		d.Meta.TimeoutSecs == nil || *d.Meta.TimeoutSecs != 3600 ||
		d.Meta.StartedAt != "2026-10-12T11:59:58Z" || d.Meta.EndedAt != "2026-10-12T12:00:00Z" ||
		d.Meta.LogBytes == nil || *d.Meta.LogBytes != 42 ||
		d.Meta.PromptFile != "/root/agentteams-fs/shared/tasks/t1/coding-prompts/20261012-120000.txt" ||
		d.Meta.PromptSHA256 != "abc123sha" {
		t.Fatalf("meta = %+v, want the record's fields pass-through", d.Meta)
	}
	// Input (prompt mirrored and available).
	if !d.Input.PromptAvailable || d.Input.Prompt != promptBody ||
		d.Input.PromptSummary != "do the thing" || d.Input.PromptSHA256 != "abc123sha" {
		t.Fatalf("input = %+v, want available prompt + summary + sha", d.Input)
	}
	// Output (transcript byte-transparent inside the envelope).
	if d.Output.Transcript != body || d.Output.Status != "ok" || d.Output.ExitCode == nil || *d.Output.ExitCode != 0 {
		t.Fatalf("output = %+v, want transcript + status + exit_code", d.Output)
	}
}

func TestGetCodingRun_JSONFormat_PromptUnavailable(t *testing.T) {
	// The record names a prompt the mirror never stored (a pre-fix run):
	// prompt_available=false, the prompt key omitted, summary/sha still
	// surface from the record.
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "transcript")
	putRun(store, runsJSONLKey, `{"v":1,"run":"run-20261012-120000.log","cli":"qwen","status":"failed","exit_code":1,"prompt_file":"/root/agentteams-fs/shared/tasks/t1/coding-prompts/20261012-120000.txt","prompt_sha256":"deadbeef","prompt_summary":"do the thing"}`)
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	rec := getRunJSON(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var d codingRunDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if d.Input.PromptAvailable || d.Input.Prompt != "" {
		t.Fatalf("input = %+v, want prompt unavailable + empty", d.Input)
	}
	if d.Input.PromptSummary != "do the thing" || d.Input.PromptSHA256 != "deadbeef" {
		t.Fatalf("input = %+v, want summary/sha from the record even when the prompt is unavailable", d.Input)
	}
	if d.Output.Status != "failed" || d.Output.ExitCode == nil || *d.Output.ExitCode != 1 {
		t.Fatalf("output = %+v, want status=failed exit_code=1", d.Output)
	}
	// The prompt key is omitted from the wire (empty-value contract).
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw detail: %v", err)
	}
	inputRaw, ok := raw["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing from raw detail: %v", raw)
	}
	if _, ok := inputRaw["prompt"]; ok {
		t.Fatalf("prompt key must be omitted when unavailable: %v", inputRaw)
	}
}

func TestGetCodingRun_JSONFormat_NoMetadata(t *testing.T) {
	// No runs.jsonl and no prompt object: meta degrades to an empty shell and
	// the transcript stays available — a metadata read failure never 500s.
	store := ossfake.NewMemory()
	body := "raw transcript only"
	putRun(store, teamRunKey, body)
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	rec := getRunJSON(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrade, not 500): %s", rec.Code, rec.Body.String())
	}
	var d codingRunDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw detail: %v", err)
	}
	metaRaw, ok := raw["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta must be an object (empty shell), got: %v", raw["meta"])
	}
	if len(metaRaw) != 0 {
		t.Fatalf("meta = %v, want an empty shell when there is no record", metaRaw)
	}
	if d.Input.PromptAvailable || d.Input.Prompt != "" || d.Input.PromptSummary != "" || d.Input.PromptSHA256 != "" {
		t.Fatalf("input = %+v, want fully degraded", d.Input)
	}
	if d.Output.Transcript != body || d.Output.Status != "" || d.Output.ExitCode != nil {
		t.Fatalf("output = %+v, want the raw transcript with no metadata", d.Output)
	}
}

func TestGetCodingRun_JSONFormat_PromptBasenameFallback(t *testing.T) {
	// Record without prompt_file: the prompt basename is derived from the run
	// name (run-20261012-120000.log → 20261012-120000.txt) when the mirror
	// made it available.
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "transcript")
	putRun(store, runsJSONLKey, `{"v":1,"run":"run-20261012-120000.log","cli":"qwen","status":"ok","exit_code":0}`)
	const promptBody = "derived basename prompt"
	putRun(store, promptKey, promptBody)
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))

	rec := getRunJSON(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var d codingRunDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if !d.Input.PromptAvailable || d.Input.Prompt != promptBody {
		t.Fatalf("input = %+v, want the prompt read via the run-name-derived basename", d.Input)
	}
}

func TestGetCodingRun_L2OwnTeam(t *testing.T) {
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "alpha t1")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))
	rec := getRun(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	if rec.Code != http.StatusOK {
		t.Fatalf("L2 own-team status = %d, want 200", rec.Code)
	}
}

func TestGetCodingRun_L2OtherTeam(t *testing.T) {
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "alpha t1")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))
	// beta leader reading an alpha-team run → 404 (W4, no probing).
	rec := getRun(h, "t1", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "beta-lead", Team: "beta-team"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("L2 cross-team status = %d, want 404", rec.Code)
	}
}

func TestGetCodingRun_L2CannotSeeStandalone(t *testing.T) {
	store := ossfake.NewMemory()
	putRun(store, standRunKey, "standalone t9")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))
	// L2 leader cannot read a standalone (global shared/) run — L1-only.
	rec := getRun(h, "t9", "run-20261012-120100.log", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("L2 standalone status = %d, want 404", rec.Code)
	}
	// But admin can.
	rec2 := getRun(h, "t9", "run-20261012-120100.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("admin standalone status = %d, want 200", rec2.Code)
	}
}

func TestGetCodingRun_InvalidTaskID(t *testing.T) {
	store := ossfake.NewMemory()
	h := newCodingRunsTestHandler(t, store)
	rec := getRun(h, "../../etc", "run-20261012-120000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal taskID status = %d, want 400", rec.Code)
	}
}

func TestGetCodingRun_InvalidRun(t *testing.T) {
	store := ossfake.NewMemory()
	putRun(store, teamRunKey, "x")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))
	rec := getRun(h, "t1", "run-evil.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad run status = %d, want 400", rec.Code)
	}
}

func TestGetCodingRun_NotFound(t *testing.T) {
	store := ossfake.NewMemory()
	// task t1 exists (has a dir marker) but no matching run.
	putRun(store, teamRunKey, "a run")
	h := newCodingRunsTestHandler(t, store, team("alpha-team"))
	rec := getRun(h, "t1", "run-20260101-000000.log", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing run status = %d, want 404", rec.Code)
	}
}

// TestListCodingRuns_CapIsGlobalAcrossTeams pins the list cap as GLOBAL across
// the caller's visible prefixes, not per-prefix. Regression for the case the
// upstream maintainer reproduced: two teams with codingRunsListCap runs each
// returned 201 because the inner file/task loops broke at the cap but the
// outer visible-prefix loop kept going into the next team's prefix. The break
// must stop the outer traversal as well.
func TestListCodingRuns_CapIsGlobalAcrossTeams(t *testing.T) {
	store := ossfake.NewMemory()
	// Two teams, each with exactly codingRunsListCap runs (one per task).
	for i := 0; i < codingRunsListCap; i++ {
		putRun(store, fmt.Sprintf("teams/alpha-team/shared/tasks/a%03d/workspace/coding-cli-logs/run-20261012-120000.log", i), "alpha")
		putRun(store, fmt.Sprintf("teams/beta-team/shared/tasks/b%03d/workspace/coding-cli-logs/run-20261012-120000.log", i), "beta")
	}
	h := newCodingRunsTestHandler(t, store, team("alpha-team"), team("beta-team"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListCodingRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var runs []codingRun
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(runs) != codingRunsListCap {
		t.Fatalf("len(runs) = %d, want global cap %d (outer prefix loop must stop once the cap is reached)", len(runs), codingRunsListCap)
	}
}

// ── writer → storage → API: the blocked attempts, end to end ────────────────

// codingRunsWrapperRelPath is the coding-cli wrapper, relative to this
// package's directory (agentteams-controller/internal/server).
const codingRunsWrapperRelPath = "../../../manager/agent/skills/coding-cli-management/scripts/run-coding-cli.sh"

// TestBlockedAttempts_EndToEnd_WriterStorageAPI drives the REAL writer through
// both blocked exits (125: a conflicting .processing marker, and a CLI absent
// from PATH), mirrors what the skill mirrors into the in-memory store, and
// then reads the API. Regression for the review finding: a blocked attempt
// used to append only its runs.jsonl line — no transcript object — so mirroring
// the directory left it absent from the list and unreadable in detail. The
// writer's contract is now "one index line ⇔ one transcript".
func TestBlockedAttempts_EndToEnd_WriterStorageAPI(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the coding-cli wrapper is a POSIX shell script")
	}
	for _, bin := range []string{"bash", "python3", "date", "sha256sum", "sed", "tr", "head", "wc", "mkdir"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("end-to-end writer regression needs %s on PATH", bin)
		}
	}
	wrapper, err := filepath.Abs(codingRunsWrapperRelPath)
	if err != nil {
		t.Fatalf("resolve the wrapper path: %v", err)
	}
	if _, err := os.Stat(wrapper); err != nil {
		t.Skipf("wrapper not found at %s (controller tree checked out without the manager tree?)", wrapper)
	}
	// A bogus CLI name keeps the missing-CLI guard deterministic even on a host
	// that has a real qwen installed (the marker guard fires first anyway).
	const cliName = "no-such-coding-cli-for-test"
	const promptBody = "fix the parser\nplease be careful"
	const taskPrefix = "teams/alpha-team/shared/tasks/t1/"

	for _, tc := range []struct {
		name       string
		reason     string
		diagnostic string // a phrase the blocked transcript must carry
	}{
		{"marker-conflict", "marker-conflict", "is held by"},
		{"cli-missing", "cli-missing", "not on PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskDir := t.TempDir()
			workspace := filepath.Join(taskDir, "workspace")
			promptDir := filepath.Join(taskDir, "coding-prompts")
			for _, d := range []string{workspace, promptDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", d, err)
				}
			}
			prompt := filepath.Join(promptDir, "20261015-101500.txt")
			if err := os.WriteFile(prompt, []byte(promptBody), 0o644); err != nil {
				t.Fatalf("write prompt: %v", err)
			}
			args := []string{wrapper, "--cli", cliName, "--workspace", workspace, "--prompt-file", prompt}
			if tc.reason == "marker-conflict" {
				marker := fmt.Sprintf(`{"processor":"alice","expires_at":%q}`,
					time.Now().UTC().Add(15*time.Minute).Format("2006-01-02T15:04:05Z"))
				if err := os.WriteFile(filepath.Join(taskDir, ".processing"), []byte(marker), 0o644); err != nil {
					t.Fatalf("write marker: %v", err)
				}
				args = append(args, "--task-dir", taskDir, "--processor", "bob")
			}
			cmd := exec.Command("bash", args...)
			cmd.Env = append(os.Environ(),
				"AGENTTEAMS_WORKER_NAME=sysdev-lead",
				"CODING_CLI_CONFIG="+filepath.Join(t.TempDir(), "no-such-config.json"),
			)
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 125 {
				t.Fatalf("wrapper exit = %v, want 125 (blocked); output:\n%s", err, out)
			}

			logsDir := filepath.Join(workspace, "coding-cli-logs")
			// Mirror what the skill mirrors (coding-cli-logs/ + coding-prompts/)
			// — the storage leg of the regression.
			store := ossfake.NewMemory()
			mirror := func(dir, prefix string) {
				items, err := os.ReadDir(dir)
				if err != nil {
					t.Fatalf("read %s: %v", dir, err)
				}
				for _, it := range items {
					if it.IsDir() {
						continue
					}
					data, err := os.ReadFile(filepath.Join(dir, it.Name()))
					if err != nil {
						t.Fatalf("read %s: %v", it.Name(), err)
					}
					if err := store.PutObject(context.Background(), prefix+it.Name(), data); err != nil {
						t.Fatalf("put %s: %v", it.Name(), err)
					}
				}
			}
			mirror(logsDir, taskPrefix+"workspace/coding-cli-logs/")
			mirror(promptDir, taskPrefix+"coding-prompts/")

			h := newCodingRunsTestHandler(t, store, team("alpha-team"))
			admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/coding-runs", nil)
			req = withCaller(req, admin)
			rec := httptest.NewRecorder()
			h.ListCodingRuns(rec, req)

			var runs []codingRun
			if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
				t.Fatalf("decode list: %v", err)
			}
			if len(runs) != 1 {
				t.Fatalf("list = %+v, want the blocked attempt visible after the mirror (its transcript must exist)", runs)
			}
			got := runs[0]
			if got.Status != "blocked" || got.ExitCode == nil || *got.ExitCode != 125 {
				t.Fatalf("list item = %+v, want status=blocked exit_code=125", got)
			}
			if got.StartedAt == "" || !strings.HasSuffix(got.StartedAt, "Z") {
				t.Fatalf("started_at = %q, want the record's UTC instant", got.StartedAt)
			}
			if got.LogBytes == nil || *got.LogBytes == 0 {
				t.Fatalf("log_bytes = %v, want the diagnostic transcript's size", got.LogBytes)
			}

			recText := getRun(h, "t1", got.Run, admin)
			if recText.Code != http.StatusOK || !strings.Contains(recText.Body.String(), tc.diagnostic) {
				t.Fatalf("detail (text) = %d %q, want a transcript containing %q", recText.Code, recText.Body.String(), tc.diagnostic)
			}
			recJSON := getRunJSON(h, "t1", got.Run, admin)
			var d codingRunDetail
			if err := json.Unmarshal(recJSON.Body.Bytes(), &d); err != nil {
				t.Fatalf("decode detail: %v", err)
			}
			if d.Meta.Status != "blocked" || d.Meta.Reason != tc.reason {
				t.Fatalf("meta = %+v, want status=blocked reason=%s", d.Meta, tc.reason)
			}
			if !strings.Contains(d.Output.Transcript, tc.diagnostic) {
				t.Fatalf("output.transcript = %q, want the diagnostic containing %q", d.Output.Transcript, tc.diagnostic)
			}
			if !d.Input.PromptAvailable || d.Input.Prompt != promptBody {
				t.Fatalf("input = %+v, want the mirrored prompt available", d.Input)
			}
		})
	}
}
