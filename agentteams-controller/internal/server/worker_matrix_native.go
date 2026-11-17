package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func (h *WorkerMatrixHandler) nativeInputView(w http.ResponseWriter, r *http.Request, name, instance, runtime string, input *service.WorkerMatrixInput, etag string) {
	state, _, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	receipt := input.NativeCommit
	current := receipt != nil && receipt.Start == state.ActiveStart && receipt.Runtime == runtime &&
		receipt.Start.InstanceID == instance && receipt.Start.Runtime == runtime
	w.Header().Set("ETag", etag)
	httputil.WriteJSON(w, 200, map[string]any{"input": input, "etag": etag, "instance_id": instance, "runtime": runtime, "current": current})
}

func (h *WorkerMatrixHandler) GetNativeInput(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	input, etag, err := h.store.InputSnapshot(r.Context(), name, r.PathValue("key"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.nativeInputView(w, r, name, instance, runtime, input, etag)
}

func (h *WorkerMatrixHandler) retireNativeContext(r *http.Request, name string, start service.WorkerStart, input *service.WorkerMatrixInput) {
	if input.Intent == "context" && input.Work == nil {
		// Retain the immutable context, but its native input work is done. The
		// store preserves pending anchors needed by an outstanding backfill.
		_ = h.store.Retire(r.Context(), name, start, r.PathValue("key"))
	}
}

// CommitNativeInput records an adapter's fact after native input serialization.
// This is not a tool-execution receipt, Task checkpoint or completion. Its start
// binding prevents source-Harness evidence from proving target persistence.
func (h *WorkerMatrixHandler) CommitNativeInput(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	var request struct {
		InstanceID   string `json:"instance_id"`
		ProcessID    string `json:"process_id"`
		NativeRef    string `json:"native_ref"`
		NativeDigest string `json:"native_digest"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF || request.ProcessID == "" || len(request.ProcessID) > 256 || request.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") || request.NativeRef == "" || len(request.NativeRef) > 4096 || !workerSessionDigest(request.NativeDigest) {
		writeError(w, 400, "current process and actual native persistence facts required")
		return
	}
	if request.InstanceID != instance {
		writeError(w, 409, "Worker instance changed")
		return
	}
	start := service.WorkerStart{InstanceID: instance, ProcessID: request.ProcessID, Runtime: runtime}
	state, _, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if state.ActiveStart != start {
		writeError(w, 409, "native callback does not belong to the registered Worker start")
		return
	}
	expected := strings.Trim(r.Header.Get("If-Match"), `"`)
	if expected == "" || r.Header.Get("If-None-Match") != "" {
		writeError(w, 428, "read the retained input ETag first")
		return
	}
	input, etag, err := h.store.InputSnapshot(r.Context(), name, r.PathValue("key"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	if etag != expected {
		writeError(w, 409, "retained input execution or native evidence changed")
		return
	}
	if receipt := input.NativeCommit; receipt != nil && receipt.Start == start && receipt.NativeRef == request.NativeRef && receipt.NativeDigest == request.NativeDigest {
		h.retireNativeContext(r, name, start, input)
		h.nativeInputView(w, r, name, instance, runtime, input, etag)
		return
	}
	input.InputState = "native_committed"
	input.NativeCommit = &service.WorkerMatrixNativeCommit{Runtime: runtime, Start: start, NativeRef: request.NativeRef,
		NativeDigest: request.NativeDigest, SavedAt: time.Now().UTC().Format(time.RFC3339)}
	body, err := json.Marshal(input)
	if err != nil {
		writeError(w, 503, "native input evidence encoding failed")
		return
	}
	strict, ok := h.deps.OSS.(conditionalWriter)
	if !ok {
		writeError(w, 503, "strict conditional storage required")
		return
	}
	key := input.MemberPrefix + "/runtime/matrix/events/" + r.PathValue("key") + ".json"
	if err := strict.PutObjectIfMatchStrict(r.Context(), key, body, etag); err != nil {
		if errors.Is(err, oss.ErrPreconditionFailed) {
			writeError(w, 409, "retained input execution or native evidence changed")
		} else {
			writeError(w, 503, "native input evidence commit failed")
		}
		return
	}
	h.retireNativeContext(r, name, start, input)
	// Receipt and index are separate: even an in-flight source receipt retains
	// its source start, never target execution authority. Re-read the index for
	// current-start eligibility; failed cleanup cannot imply executed work.
	h.nativeInputView(w, r, name, instance, runtime, input, contentETag(body))
}
