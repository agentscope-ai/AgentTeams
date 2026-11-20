package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type publishedTaskFile struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
}

// The current file backend keeps this index in the Task's conditional record.
// Object keys are derived server-side and never appear in the public resource.
type taskFileIndex struct {
	Artifacts map[string]publishedTaskFile `json:"artifacts"`
	Heads     map[string]string            `json:"heads"`
}

func validTaskFileName(name string) bool {
	return name != "" && name != "." && name != ".." && path.Clean(name) == name &&
		!strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\\x00")
}

func validArtifactDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func (h *ProjectHandler) PublishTaskFile(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.URL.Query().Get("name")
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "intermediate"
	}
	if !isSafeTaskID(id) || !validTaskFileName(name) || (kind != "intermediate" && kind != "output") {
		writeError(w, 400, "invalid task file identity or kind")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	if !recoveryCaller(caller) {
		writeError(w, 403, "assigned execution identity required")
		return
	}
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	var data []byte
	for attempt := 0; attempt < 3; attempt++ {
		current, err := h.oss.GetObject(r.Context(), key)
		var task map[string]json.RawMessage
		if err != nil || json.Unmarshal(current, &task) != nil {
			writeError(w, 503, "task snapshot unavailable")
			return
		}
		var status, taskID string
		_ = json.Unmarshal(task["status"], &status)
		_ = json.Unmarshal(task["task_id"], &taskID)
		if taskID != id || status != "in_progress" {
			writeError(w, 409, "task is not executing")
			return
		}
		if h.recovery == nil || !h.recovery.owns(r.Context(), caller, task) {
			writeError(w, 403, "task is not assigned to you")
			return
		}
		var execution taskExecution
		if json.Unmarshal(task["execution"], &execution) != nil || execution.Generation == 0 || execution.State != "active" || execution.ProcessID == "" || execution.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") {
			writeError(w, 409, "task execution changed")
			return
		}
		instance, _, err := h.recovery.resolveInstance(r.Context(), caller, nil)
		if err != nil {
			writeError(w, 503, "execution identity unavailable")
			return
		}
		if instance != execution.InstanceID {
			writeError(w, 409, "task execution instance changed")
			return
		}
		if data == nil {
			data, err = io.ReadAll(http.MaxBytesReader(w, r.Body, taskDownloadMaxBytes))
			if err != nil {
				writeError(w, 413, "task file upload incomplete or too large")
				return
			}
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		identity, _ := json.Marshal([]string{team, id, name, kind, digest})
		identityHash := sha256.Sum256(identity)
		artifact := publishedTaskFile{ArtifactID: hex.EncodeToString(identityHash[:]), Name: name, Kind: kind, Version: digest, SHA256: digest, Size: int64(len(data))}
		index := taskFileIndex{Artifacts: map[string]publishedTaskFile{}, Heads: map[string]string{}}
		if raw := task["task_files"]; len(raw) > 0 && json.Unmarshal(raw, &index) != nil {
			writeError(w, 503, "invalid task file index")
			return
		}
		if index.Artifacts == nil || index.Heads == nil {
			writeError(w, 503, "invalid task file index")
			return
		}
		if previous, exists := index.Artifacts[artifact.ArtifactID]; exists {
			if previous != artifact {
				writeError(w, 409, "artifact identity conflict")
				return
			}
			httputil.WriteJSON(w, 200, artifact)
			return
		}
		create, createOK := h.oss.(createIfAbsentWriter)
		commit, commitOK := h.oss.(conditionalWriter)
		if !createOK || !commitOK {
			writeError(w, 503, "conditional artifact storage required")
			return
		}
		blob := path.Dir(key) + "/artifacts/" + digest
		if err := create.PutObjectIfNoneMatchStrict(r.Context(), blob, data); err != nil {
			if !errors.Is(err, oss.ErrPreconditionFailed) {
				writeError(w, 503, "artifact upload unavailable")
				return
			}
			saved, readErr := h.oss.GetObject(r.Context(), blob)
			if readErr != nil || sha256.Sum256(saved) != sum {
				writeError(w, 503, "artifact content verification failed")
				return
			}
		}
		index.Artifacts[artifact.ArtifactID], index.Heads[name] = artifact, artifact.ArtifactID
		task["task_files"], _ = json.Marshal(index)
		next, err := json.Marshal(task)
		if err != nil || len(next) > taskMetaCommitMaxBytes {
			writeError(w, 413, "task file index exceeds task record limit")
			return
		}
		if err := commit.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(current)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			writeError(w, 503, "artifact registration unavailable; retry the same upload")
			return
		}
		httputil.WriteJSON(w, 200, artifact)
		return
	}
	writeError(w, 409, "task state changed during artifact publication; retry the same upload")
}

func validateTaskFileIndexCommit(w http.ResponseWriter, current, next []byte) bool {
	before, had := rawJSONField(current, "task_files")
	after, has := rawJSONField(next, "task_files")
	if had != has || (had && !sameJSONValue(before, after)) {
		writeError(w, http.StatusForbidden, "task file index is server-owned; use the task file API")
		return false
	}
	return true
}
