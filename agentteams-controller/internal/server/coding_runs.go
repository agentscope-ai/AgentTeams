// Package server — read-only coding-run transcript visibility (D12 路 A).
//
// See docs/design/coding-runs-api.md for the routes, the two-layer
// authorization (authorizer admits the action, the handler enforces the
// team scope + W4 404), and the status mapping.
//
// CodingRunsHandler exposes the one-shot coding CLI runs that
// run-coding-cli.sh (#1342) tees into coding-cli-logs/, so a human can see
// what a delegated coding task did — which files it touched, what the CLI
// output, why it timed out or failed — without logging into the worker to
// read a log file. It is the coding-CLI counterpart of the worker chats proxy
// (#1295): same read-only, byte-transparent, W8/W4 404-not-403 discipline.
//
// Where the data lives. The coding skill pins the run workspace under
// /root/agentteams-fs/shared/tasks/{task-id}/workspace and mc-mirrors it to
// object storage, so each run transcript sits at
//
//	shared/tasks/{taskID}/workspace/coding-cli-logs/run-{ts}.log   (standalone)
//	teams/{team}/shared/tasks/{taskID}/workspace/coding-cli-logs/run-{ts}.log (team)
//
// The controller does not mount the worker filesystem (helm mounts only
// /data/worker-creds), so it reads through oss.StorageClient (mc), exactly
// like the project task endpoints — which is also why this works in both
// embedded and K8s modes (it does not proxy a per-worker console port).
//
// Access. The caller's readable storage prefixes are team-scoped, mirroring
// the project endpoints: L1 reads team + global (standalone) tasks; an L2
// team-leader / human reads only their own team's prefix, and standalone
// tasks (the global shared/ prefix, which have no team ownership) stay
// L1-only. A task the caller cannot see 404s like a missing task (W4: no
// probing).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// codingRunsListCap bounds the number of runs returned by the list
	// endpoint (the project list cap discipline: a bounded response, not an
	// unbounded scan of every task).
	codingRunsListCap = 200
)

// codingRunFilePattern matches a run transcript filename: run-{YYYYMMDD}-
// {HHMMSS}.log, the shape run-coding-cli.sh writes.
var codingRunFilePattern = regexp.MustCompile(`^run-\d{8}-\d{6}\.log$`)

// CodingRunsHandler serves read-only coding-run transcripts from object
// storage.
type CodingRunsHandler struct {
	client    client.Client
	namespace string
	oss       oss.StorageClient
}

// NewCodingRunsHandler creates the handler. oss is the object-storage client
// (mc-backed in production, ossfake in tests).
func NewCodingRunsHandler(c client.Client, namespace string, o oss.StorageClient) *CodingRunsHandler {
	return &CodingRunsHandler{client: c, namespace: namespace, oss: o}
}

// taskLogPrefixes builds the dual-prefix task layout (team + global),
// mirroring ProjectHandler.teamProjectPrefixes but for coding runs (tasks,
// not projects). Returns (prefixes, crToEffective, error).
func (h *CodingRunsHandler) taskLogPrefixes(ctx context.Context) ([]string, map[string]string, error) {
	var teams v1beta1.TeamList
	if err := h.client.List(ctx, &teams, client.InNamespace(h.namespace)); err != nil {
		return nil, nil, err
	}
	prefixes := make([]string, 0, len(teams.Items)+1)
	crToEffective := make(map[string]string, len(teams.Items))
	seen := map[string]bool{}
	for i := range teams.Items {
		effective := teams.Items[i].Spec.EffectiveTeamName(teams.Items[i].Name)
		crToEffective[teams.Items[i].Name] = effective
		for _, name := range []string{effective, teams.Items[i].Name} {
			prefix := "teams/" + name + "/shared/tasks/"
			if !seen[prefix] {
				seen[prefix] = true
				prefixes = append(prefixes, prefix)
			}
		}
	}
	prefixes = append(prefixes, "shared/tasks/")
	return prefixes, crToEffective, nil
}

// visibleTaskPrefixes narrows the full prefix set to what a scoped caller may
// read. L1 (admin, and any caller that is neither a team leader nor an L2
// human) reads everything — team prefixes plus the global standalone prefix.
// An L2 caller reads only the prefix of each team they belong to; the global
// standalone prefix is deliberately excluded for them (standalone coding tasks
// have no team ownership and stay L1-only).
func visibleTaskPrefixes(all []string, caller *authpkg.CallerIdentity, crToEffective map[string]string) []string {
	if caller == nil || (caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleHuman) {
		return all
	}
	teams := caller.Teams
	if len(teams) == 0 && caller.Team != "" {
		teams = []string{caller.Team}
	}
	allowed := make(map[string]bool, len(teams))
	for _, t := range teams {
		eff := t
		if mapped, ok := crToEffective[t]; ok && mapped != "" {
			eff = mapped
		}
		allowed["teams/"+eff+"/shared/tasks/"] = true
	}
	out := make([]string, 0, len(allowed))
	for _, p := range all {
		if allowed[p] {
			out = append(out, p)
		}
	}
	return out
}

// codingRun is one run transcript entry in the list response. The base
// fields (task_id / run / team) are v1; the metadata fields are additive
// (v2) and omitted when the task's runs.jsonl has no record for the run
// (older wrapper versions, or a pre-flight failure that left no record).
type codingRun struct {
	TaskID    string `json:"task_id"`
	Run       string `json:"run"`
	Team      string `json:"team,omitempty"`
	Cli       string `json:"cli,omitempty"`
	Worker    string `json:"worker,omitempty"`
	Status    string `json:"status,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"` // 0 is a legal value and must show; unknown omits
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	LogBytes  *int64 `json:"log_bytes,omitempty"`
}

// codingRunRecord is one line of the append-only runs.jsonl that
// run-coding-cli.sh appends per run — the outcome metadata (status, exit
// code, timing, prompt digest) the list and detail endpoints surface. Every
// field is best-effort: absent when the wrapper did not record it.
type codingRunRecord struct {
	V             int    `json:"v"`
	Run           string `json:"run"`
	Cli           string `json:"cli"`
	Worker        string `json:"worker"`
	Runner        string `json:"runner"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	ExitCode      *int   `json:"exit_code"`
	TimeoutSecs   *int   `json:"timeout_secs"`
	StartedAt     string `json:"started_at"`
	EndedAt       string `json:"ended_at"`
	LogBytes      *int64 `json:"log_bytes"`
	PromptFile    string `json:"prompt_file"`
	PromptSHA256  string `json:"prompt_sha256"`
	PromptSummary string `json:"prompt_summary"`
}

// parseRunsIndex parses an append-only runs.jsonl into a run-name → record
// map. The index is best-effort worker-written metadata, so bad lines are
// skipped and a missing run name drops the line — a malformed index must
// never fail the read endpoints. When a run name repeats (it should not —
// single writer, unique names), the later line wins.
func parseRunsIndex(data []byte) map[string]*codingRunRecord {
	out := map[string]*codingRunRecord{}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec codingRunRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Run == "" {
			continue
		}
		out[rec.Run] = &rec
	}
	return out
}

// Note on started_at: the wrapper names transcripts with the CONTAINER's local
// wall clock (`date +%Y%m%d-%H%M%S`), so a run filename is not a UTC instant
// and its timezone is unknowable from the controller. The API therefore only
// reports started_at when the run's record carries it (the wrapper writes UTC
// via `date -u`); it never reinterprets a legacy filename under a UTC
// assumption. Unknown time is omitted, not fabricated.

// GET /api/v1/coding-runs
//
// Lists the coding-run transcripts the caller may read, across their visible
// storage prefixes, bounded by codingRunsListCap.
func (h *CodingRunsHandler) ListCodingRuns(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	prefixes, crToEffective, err := h.taskLogPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "list coding runs: resolve prefixes", err)
		return
	}
	runs := make([]codingRun, 0)
	for _, prefix := range visibleTaskPrefixes(prefixes, caller, crToEffective) {
		dirs, err := h.oss.ListObjects(r.Context(), prefix)
		if err != nil {
			continue // unreadable prefix (no such team's storage) — skip, don't fail the whole list
		}
		for _, d := range dirs {
			if !strings.HasSuffix(d, "/") {
				continue
			}
			taskID := strings.TrimSuffix(d, "/")
			logsPrefix := prefix + taskID + "/workspace/coding-cli-logs/"
			files, err := h.oss.ListObjects(r.Context(), logsPrefix)
			if err != nil {
				continue
			}
			// One extra read per task WITH runs — the append-only runs.jsonl
			// outcome index, never the .log bodies (a per-run read would be
			// N+1). Loaded lazily so a directory holding no run file costs
			// nothing; a missing index or bad lines degrade to the base
			// fields.
			index := map[string]*codingRunRecord{}
			indexLoaded := false
			for _, f := range files {
				if !codingRunFilePattern.MatchString(f) {
					continue
				}
				if !indexLoaded {
					if data, err := h.oss.GetObject(r.Context(), logsPrefix+"runs.jsonl"); err == nil {
						index = parseRunsIndex(data)
					}
					indexLoaded = true
				}
				entry := codingRun{TaskID: taskID, Run: f, Team: teamOfPrefix(prefix)}
				if rec, ok := index[f]; ok {
					entry.Cli = rec.Cli
					entry.Worker = rec.Worker
					entry.Status = rec.Status
					entry.ExitCode = rec.ExitCode
					entry.StartedAt = rec.StartedAt
					entry.EndedAt = rec.EndedAt
					entry.LogBytes = rec.LogBytes
				}
				runs = append(runs, entry)
				if len(runs) >= codingRunsListCap {
					break
				}
			}
			if len(runs) >= codingRunsListCap {
				break
			}
		}
		if len(runs) >= codingRunsListCap {
			break
		}
	}
	httputil.WriteJSON(w, http.StatusOK, runs)
}

// codingRunDetail is the ?format=json detail envelope. The transcript stays
// byte-transparent inside output.transcript; meta carries the runs.jsonl
// record (an empty object when the task has no record — degradation, never
// an error), and input carries the prompt when the mirror made it readable.
type codingRunDetail struct {
	TaskID string          `json:"task_id"`
	Run    string          `json:"run"`
	Team   string          `json:"team,omitempty"`
	Meta   codingRunMeta   `json:"meta"`
	Input  codingRunInput  `json:"input"`
	Output codingRunOutput `json:"output"`
}

type codingRunMeta struct {
	Cli          string `json:"cli,omitempty"`
	Worker       string `json:"worker,omitempty"`
	Runner       string `json:"runner,omitempty"`
	Status       string `json:"status,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	TimeoutSecs  *int   `json:"timeout_secs,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	EndedAt      string `json:"ended_at,omitempty"`
	LogBytes     *int64 `json:"log_bytes,omitempty"`
	PromptFile   string `json:"prompt_file,omitempty"`
	PromptSHA256 string `json:"prompt_sha256,omitempty"`
}

type codingRunInput struct {
	PromptSummary   string `json:"prompt_summary,omitempty"`
	PromptSHA256    string `json:"prompt_sha256,omitempty"`
	Prompt          string `json:"prompt,omitempty"`
	PromptAvailable bool   `json:"prompt_available"`
}

type codingRunOutput struct {
	Transcript string `json:"transcript"`
	Status     string `json:"status,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

// GET /api/v1/coding-runs/{taskID}/runs/{run}
//
// Serves one run transcript. The default (no format parameter) is
// byte-transparent text/plain — the raw .log the CLI teed, unchanged from
// v1; ?format=json adds the structured envelope above. The path segments are
// validated (isSafeTaskID + the run filename pattern) before any storage
// read, so they cannot carry path traversal. The run is looked up across the
// caller's visible prefixes; a miss 404s like a missing task (W4).
func (h *CodingRunsHandler) GetCodingRun(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskID")
	run := r.PathValue("run")
	if !isSafeTaskID(taskID) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	if !codingRunFilePattern.MatchString(run) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	teamFilter := r.URL.Query().Get("team")
	prefixes, crToEffective, err := h.taskLogPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "get coding run: resolve prefixes", err)
		return
	}
	try := make([]string, 0, len(prefixes))
	for _, p := range visibleTaskPrefixes(prefixes, caller, crToEffective) {
		if teamFilter != "" && !strings.HasPrefix(p, "teams/"+teamFilter+"/") {
			continue
		}
		try = append(try, p)
	}
	var transcript []byte
	var foundPrefix string
	found := false
	for _, p := range try {
		key := p + taskID + "/workspace/coding-cli-logs/" + run
		data, err := h.oss.GetObject(r.Context(), key)
		if err != nil {
			continue // not under this prefix; try the next
		}
		transcript, foundPrefix, found = data, p, true
		break
	}
	if !found {
		httputil.WriteError(w, http.StatusNotFound, "coding run not found")
		return
	}
	if r.URL.Query().Get("format") == "json" {
		h.writeRunDetailJSON(w, r, foundPrefix, taskID, run, transcript)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(transcript)
}

// writeRunDetailJSON serves the ?format=json detail. Metadata reads degrade
// independently (meta empty shell / prompt unavailable) so a missing
// runs.jsonl or prompt never turns a readable transcript into an error.
func (h *CodingRunsHandler) writeRunDetailJSON(w http.ResponseWriter, r *http.Request, prefix, taskID, run string, transcript []byte) {
	var rec *codingRunRecord
	if data, err := h.oss.GetObject(r.Context(), prefix+taskID+"/workspace/coding-cli-logs/runs.jsonl"); err == nil {
		rec = parseRunsIndex(data)[run]
	}
	meta := codingRunMeta{}
	input := codingRunInput{}
	output := codingRunOutput{Transcript: string(transcript)}
	if rec != nil {
		meta = codingRunMeta{
			Cli:          rec.Cli,
			Worker:       rec.Worker,
			Runner:       rec.Runner,
			Status:       rec.Status,
			Reason:       rec.Reason,
			ExitCode:     rec.ExitCode,
			TimeoutSecs:  rec.TimeoutSecs,
			StartedAt:    rec.StartedAt,
			EndedAt:      rec.EndedAt,
			LogBytes:     rec.LogBytes,
			PromptFile:   rec.PromptFile,
			PromptSHA256: rec.PromptSHA256,
		}
		input.PromptSummary = rec.PromptSummary
		input.PromptSHA256 = rec.PromptSHA256
		output.Status = rec.Status
		output.ExitCode = rec.ExitCode
	}
	// The prompt sits beside the workspace (…/{taskID}/coding-prompts/), not
	// inside it; the "mirror on every outcome" rule is what makes it readable.
	if base := promptBaseName(rec, run); base != "" {
		if data, err := h.oss.GetObject(r.Context(), prefix+taskID+"/coding-prompts/"+base); err == nil {
			input.Prompt = string(data)
			input.PromptAvailable = true
		}
	}
	httputil.WriteJSON(w, http.StatusOK, codingRunDetail{
		TaskID: taskID,
		Run:    run,
		Team:   teamOfPrefix(prefix),
		Meta:   meta,
		Input:  input,
		Output: output,
	})
}

// promptBaseName resolves the prompt object's basename: the record's
// prompt_file path element when the wrapper recorded one (authoritative — the
// delegator names the prompt file, the wrapper names the run, and the two
// clocks can straddle a second boundary), else the run filename's timestamp
// as .txt.
func promptBaseName(rec *codingRunRecord, run string) string {
	if rec != nil && rec.PromptFile != "" {
		if i := strings.LastIndex(rec.PromptFile, "/"); i >= 0 {
			return rec.PromptFile[i+1:]
		}
		return rec.PromptFile
	}
	return strings.TrimSuffix(strings.TrimPrefix(run, "run-"), ".log") + ".txt"
}

// teamOfPrefix extracts the team name from a storage prefix for the list
// response ("" for the global standalone prefix).
func teamOfPrefix(prefix string) string {
	if strings.HasPrefix(prefix, "teams/") {
		rest := strings.TrimPrefix(prefix, "teams/")
		if i := strings.Index(rest, "/"); i > 0 {
			return rest[:i]
		}
	}
	return ""
}
