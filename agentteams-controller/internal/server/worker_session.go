package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

type workerSessionWork struct {
	Kind    string `json:"kind"`
	TeamID  string `json:"team_id,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
	RoomID  string `json:"room_id,omitempty"`
	EventID string `json:"event_id,omitempty"`
}

// A cache descriptor is never an execution checkpoint. It records the native
// adapter's final persistence facts; that adapter must verify the actual native
// content again before reuse. No native session crosses Harness boundaries.
type workerSessionCache struct {
	SchemaVersion     int                 `json:"schema_version"`
	Kind              string              `json:"kind"`
	MemberPrefix      string              `json:"member_prefix"`
	Work              workerSessionWork   `json:"work"`
	Runtime           string              `json:"runtime"`
	Start             service.WorkerStart `json:"start"`
	Generation        uint64              `json:"generation"`
	NativeRef         string              `json:"native_ref"`
	NativeDigest      string              `json:"native_digest"`
	CheckpointVersion uint64              `json:"checkpoint_version"`
	CheckpointDigest  string              `json:"checkpoint_digest"`
	ConfigGeneration  int64               `json:"config_generation"`
	ExpiresAt         int64               `json:"expires_at"`
	SavedAt           string              `json:"saved_at"`
}

// Matches Recovery.digest, including Python's ensure_ascii encoding. Keeping
// this in the common API avoids six language/SDK-specific digest implementations.
func workerCheckpointDigest(raw json.RawMessage) string {
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&value) != nil {
		return ""
	}
	var compact bytes.Buffer
	encoder := json.NewEncoder(&compact)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return ""
	}
	var ascii strings.Builder
	for _, r := range strings.TrimSuffix(compact.String(), "\n") {
		if r < 128 {
			ascii.WriteRune(r)
		} else if r <= 0xffff {
			fmt.Fprintf(&ascii, `\u%04x`, r)
		} else {
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&ascii, `\u%04x\u%04x`, hi, lo)
		}
	}
	sum := sha256.Sum256([]byte(ascii.String()))
	return hex.EncodeToString(sum[:])
}

type workerSessionView struct {
	Work              workerSessionWork   `json:"work"`
	Cache             *workerSessionCache `json:"cache"`
	ETag              string              `json:"etag"`
	Compatible        bool                `json:"compatible"`
	InstanceID        string              `json:"instance_id"`
	Runtime           string              `json:"runtime"`
	Generation        uint64              `json:"generation"`
	CheckpointVersion uint64              `json:"checkpoint_version"`
	CheckpointDigest  string              `json:"checkpoint_digest"`
	ConfigGeneration  int64               `json:"config_generation"`
	execution         taskExecution
	key               string
}

func (h *WorkerMatrixHandler) sessionView(w http.ResponseWriter, r *http.Request, name, instance, runtime string) (*workerSessionView, bool) {
	var execution taskExecution
	var executionRaw json.RawMessage
	var work workerSessionWork
	var workKey string
	caller := authpkg.CallerFromContext(r.Context())
	switch r.URL.Query().Get("kind") {
	case "task":
		id := r.URL.Query().Get("task")
		if id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." || r.URL.Query().Get("team") == "" || r.URL.Query().Get("event_key") != "" {
			writeError(w, 400, "task id and explicit team required")
			return nil, false
		}
		if !recoveryCaller(caller) || h.recovery.project == nil {
			writeError(w, 403, "team Worker identity required")
			return nil, false
		}
		key, team, ok := h.recovery.project.resolveTaskMetaCommitKey(w, r, id)
		if !ok {
			return nil, false
		}
		data, err := h.deps.OSS.GetObject(r.Context(), key)
		var task map[string]json.RawMessage
		var actualID string
		if err != nil || json.Unmarshal(data, &task) != nil || json.Unmarshal(task["task_id"], &actualID) != nil || actualID != id {
			writeError(w, 503, "Task authority unavailable")
			return nil, false
		}
		if !h.recovery.owns(r.Context(), caller, task) {
			writeError(w, 403, "Task is not assigned to you")
			return nil, false
		}
		var status string
		_ = json.Unmarshal(task["status"], &status)
		if !recoveryStatus(task) || status == "submitted" {
			writeError(w, 409, "Task is not recoverable")
			return nil, false
		}
		executionRaw = task["execution"]
		if len(executionRaw) > 0 && json.Unmarshal(executionRaw, &execution) != nil {
			writeError(w, 503, "Task execution unavailable")
			return nil, false
		}
		work = workerSessionWork{Kind: "task", TeamID: team, TaskID: id}
		workKey = service.WorkerTaskSessionKey("agents/"+name, team, id)
	case "message":
		if r.URL.Query().Get("task") != "" || r.URL.Query().Get("team") != "" {
			writeError(w, 400, "message work requires only an event key")
			return nil, false
		}
		input, _, err := h.store.InputSnapshot(r.Context(), name, r.URL.Query().Get("event_key"))
		if err != nil {
			h.writeError(w, err)
			return nil, false
		}
		if input.Intent != "message" || (input.Work != nil && input.Work.Kind != "message") {
			writeError(w, 409, "use the Task's authoritative session scope")
			return nil, false
		}
		executionRaw = input.Execution
		if len(executionRaw) > 0 && json.Unmarshal(executionRaw, &execution) != nil {
			writeError(w, 503, "message execution unavailable")
			return nil, false
		}
		work = workerSessionWork{Kind: "message", RoomID: input.RoomID, EventID: input.EventID}
		workKey = service.WorkerMessageSessionKey("agents/"+name, input.RoomID, input.EventID)
	default:
		writeError(w, 400, "Task or message session scope required")
		return nil, false
	}
	cpState := execution.Checkpoint
	if unresolvedOperation(cpState) || (cpState != nil && (cpState.Delivery == "pending" || (work.Kind == "message" && cpState.Delivery == "delivered"))) || (execution.State != "" && execution.State != "active" && execution.State != "released") {
		writeError(w, 409, "work requires delivery or verification, or is terminal")
		return nil, false
	}
	config, err := service.WorkerRuntimeConfigGeneration(r.Context(), h.deps.OSS, name, runtime)
	if err != nil {
		writeError(w, 503, "current Worker configuration unavailable")
		return nil, false
	}
	// Hash the actual stored checkpoint, without filling omitted legacy fields
	// or discarding unknown fields that the Python client still sees.
	cp := struct {
		Checkpoint json.RawMessage `json:"checkpoint"`
	}{Checkpoint: json.RawMessage("null")}
	if len(executionRaw) > 0 {
		_ = json.Unmarshal(executionRaw, &cp)
	}
	v := &workerSessionView{InstanceID: instance, Runtime: runtime, Generation: execution.Generation,
		CheckpointDigest: workerCheckpointDigest(cp.Checkpoint), ConfigGeneration: config, Work: work, execution: execution,
		key: "agents/" + name + "/runtime/sessions/" + runtime + "/" + workKey + ".json"}
	if execution.Checkpoint != nil {
		v.CheckpointVersion = execution.Checkpoint.Version
	}
	data, err := h.deps.OSS.GetObject(r.Context(), v.key)
	if errors.Is(err, os.ErrNotExist) {
		return v, true
	}
	if err != nil {
		writeError(w, 503, "native cache descriptor unavailable")
		return nil, false
	}
	var cache workerSessionCache
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if !json.Valid(data) || d.Decode(&cache) != nil || cache.SchemaVersion != 1 || cache.Kind != "WorkerSessionCache" || cache.MemberPrefix != "agents/"+name || cache.Work != work || cache.Runtime != runtime {
		writeError(w, 503, "unsupported or foreign native cache descriptor")
		return nil, false
	}
	v.Cache, v.ETag = &cache, contentETag(data)
	v.Compatible = cache.Generation <= v.Generation && cache.Start.Runtime == runtime && cache.ExpiresAt > time.Now().Unix() && cache.NativeRef != "" && workerSessionDigest(cache.NativeDigest) &&
		cache.CheckpointVersion == v.CheckpointVersion && cache.CheckpointDigest == v.CheckpointDigest && cache.ConfigGeneration == config
	return v, true
}

func workerSessionDigest(digest string) bool {
	data, err := hex.DecodeString(digest)
	return err == nil && len(data) == sha256.Size && strings.ToLower(digest) == digest
}

func (h *WorkerMatrixHandler) GetSession(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	if view, ok := h.sessionView(w, r, name, instance, runtime); ok {
		w.Header().Set("ETag", view.ETag)
		httputil.WriteJSON(w, 200, view)
	}
}

func (h *WorkerMatrixHandler) RememberSession(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	var input struct {
		InstanceID        string `json:"instance_id"`
		ProcessID         string `json:"process_id"`
		Generation        uint64 `json:"generation"`
		CheckpointVersion uint64 `json:"checkpoint_version"`
		CheckpointDigest  string `json:"checkpoint_digest"`
		ConfigGeneration  int64  `json:"config_generation"`
		NativeRef         string `json:"native_ref"`
		NativeDigest      string `json:"native_digest"`
		ExpiresAt         int64  `json:"expires_at"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || input.ProcessID == "" || len(input.ProcessID) > 256 || input.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") || input.NativeRef == "" || len(input.NativeRef) > 4096 || !workerSessionDigest(input.NativeDigest) || input.ExpiresAt <= time.Now().Unix() {
		writeError(w, 400, "current process and final native persistence facts required")
		return
	}
	if input.InstanceID != instance {
		writeError(w, 409, "Worker instance changed")
		return
	}
	view, ok := h.sessionView(w, r, name, instance, runtime)
	if !ok {
		return
	}
	start := service.WorkerStart{InstanceID: instance, ProcessID: input.ProcessID, Runtime: runtime}
	state, _, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	execution := view.execution
	if state.ActiveStart != start || execution.Generation == 0 || execution.Generation != input.Generation || execution.InstanceID != instance || execution.ProcessID != input.ProcessID || execution.Runtime != runtime || input.CheckpointVersion != view.CheckpointVersion || input.CheckpointDigest != view.CheckpointDigest || input.ConfigGeneration != view.ConfigGeneration {
		writeError(w, 409, "native persistence facts no longer match this execution and configuration")
		return
	}
	expected, create := strings.Trim(r.Header.Get("If-Match"), `"`), r.Header.Get("If-None-Match")
	if expected == "" && create == "" {
		writeError(w, 428, "read the native cache ETag first")
		return
	}
	if (create != "" && (create != "*" || expected != "")) || (create == "" && expected == "") {
		writeError(w, 400, "one exact ETag or create-if-absent condition required")
		return
	}
	if (create == "*" && view.Cache != nil) || expected != view.ETag || (view.Cache != nil && (view.Cache.Generation > input.Generation || view.Cache.CheckpointVersion > input.CheckpointVersion)) {
		writeError(w, 409, "native cache descriptor changed")
		return
	}
	cache := &workerSessionCache{SchemaVersion: 1, Kind: "WorkerSessionCache", MemberPrefix: "agents/" + name, Work: view.Work,
		Runtime: runtime, Start: start, Generation: input.Generation, NativeRef: input.NativeRef, NativeDigest: input.NativeDigest,
		CheckpointVersion: input.CheckpointVersion, CheckpointDigest: input.CheckpointDigest, ConfigGeneration: input.ConfigGeneration,
		ExpiresAt: input.ExpiresAt, SavedAt: time.Now().UTC().Format(time.RFC3339)}
	data, _ := json.Marshal(cache)
	if create == "*" {
		err = h.deps.OSS.PutObjectIfAbsent(r.Context(), view.key, data)
	} else if strict, ok := h.deps.OSS.(conditionalWriter); ok {
		err = strict.PutObjectIfMatchStrict(r.Context(), view.key, data, expected)
	} else {
		err = fmt.Errorf("strict conditional storage required")
	}
	if err != nil {
		if errors.Is(err, oss.ErrPreconditionFailed) {
			writeError(w, 409, "native cache descriptor changed")
		} else {
			writeError(w, 503, "native cache descriptor commit failed")
		}
		return
	}
	view.Cache, view.ETag, view.Compatible = cache, contentETag(data), true
	w.Header().Set("ETag", view.ETag)
	httputil.WriteJSON(w, 200, view)
}
