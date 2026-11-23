package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"unicode/utf8"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

const taskDownloadMaxBytes = 64 << 20

type taskFileResource struct {
	ArtifactID string `json:"artifactId,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind,omitempty"`
	Version    string `json:"version"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256,omitempty"`
}

// GetTaskFiles exposes declared task outputs by task-local name. Legacy storage
// paths are resolved only here, never supplied or reconstructed by the caller.
func (h *ProjectHandler) GetTaskFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isSafeTaskID(id) {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	metaKey, _, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	body, err := h.oss.GetObject(r.Context(), metaKey)
	var record map[string]json.RawMessage
	var taskID string
	if err != nil || json.Unmarshal(body, &record) != nil || json.Unmarshal(record["task_id"], &taskID) != nil || taskID != id {
		writeError(w, http.StatusServiceUnavailable, "task files unavailable")
		return
	}
	catalog, catalogErr := resolveTaskFileCatalog(metaKey, id, record)
	if catalogErr != nil {
		writeError(w, 503, catalogErr.Error())
		return
	}
	name := r.URL.Query().Get("name")
	if identity := r.URL.Query().Get("artifactId"); identity != "" {
		artifact, exists := catalog.index.Artifacts[identity]
		if !exists || (name != "" && name != artifact.Name) {
			writeError(w, 404, "task artifact not found")
			return
		}
		if artifact.ArtifactID != identity || !validTaskFileName(artifact.Name) || !validArtifactDigest(artifact.SHA256) || artifact.Version != artifact.SHA256 {
			writeError(w, 503, "invalid task file reference")
			return
		}
		name = artifact.Name
		catalog.keys[name], catalog.published[name] = path.Dir(metaKey)+"/artifacts/"+artifact.SHA256, artifact
	}
	if name == "" {
		files, err := h.taskFileResources(r.Context(), metaKey, id, record)
		if err != nil {
			writeError(w, 503, err.Error())
			return
		}

		httputil.WriteJSON(w, http.StatusOK, struct {
			TaskID string             `json:"taskId"`
			Files  []taskFileResource `json:"files"`
		}{TaskID: id, Files: files})
		return
	}
	key, exists := catalog.keys[name]
	if !exists {
		writeError(w, http.StatusNotFound, "task file not found")
		return
	}
	meta, err := h.oss.StatMeta(r.Context(), key)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "task file not found")
		return
	}
	if err != nil || meta.ETag == "" {
		writeError(w, http.StatusServiceUnavailable, "task file version unavailable")
		return
	}
	storageVersion := httputil.BareETag(meta.ETag)
	version := storageVersion
	if artifact, exists := catalog.published[name]; exists {
		if artifact.Size != meta.Size {
			writeError(w, 503, "artifact size verification failed")
			return
		}
		version = artifact.Version
	}
	if expected := r.URL.Query().Get("version"); expected != "" && expected != version {
		writeError(w, http.StatusConflict, "task file version changed")
		return
	}
	if meta.Size > taskDownloadMaxBytes && r.URL.Query().Get("view") != "text" {
		writeError(w, http.StatusRequestEntityTooLarge, "task file exceeds download limit")
		return
	}
	reader, supported := h.oss.(oss.RangeReader)
	if !supported {
		writeError(w, http.StatusServiceUnavailable, "versioned task file reads unavailable")
		return
	}
	start, length := int64(0), int64(-1)
	textView := r.URL.Query().Get("view") == "text"
	if view := r.URL.Query().Get("view"); view != "" && !textView {
		writeError(w, http.StatusBadRequest, "invalid task file view")
		return
	}
	if textView {
		length = 32768
		for parameter, target := range map[string]*int64{"offset": &start, "limit": &length} {
			if value := r.URL.Query().Get(parameter); value != "" {
				parsed, err := strconv.ParseInt(value, 10, 64)
				if err != nil || parsed < 0 {
					writeError(w, http.StatusBadRequest, "invalid text range")
					return
				}
				*target = parsed
			}
		}
		if length < 4 || length > 32768 || start > meta.Size {
			writeError(w, http.StatusBadRequest, "text limit must be 4..32768 bytes and offset within the file")
			return
		}
		if start > 0 && r.URL.Query().Get("version") == "" {
			writeError(w, http.StatusBadRequest, "version is required to continue a text read")
			return
		}
	}
	var stream io.ReadCloser
	if start == meta.Size {
		stream = io.NopCloser(bytes.NewReader(nil))
	} else {
		stream, err = reader.OpenRange(r.Context(), key, start, length, storageVersion)
	}
	if errors.Is(err, oss.ErrPreconditionFailed) {
		writeError(w, http.StatusConflict, "task file version changed")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "task file read unavailable")
		return
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, taskDownloadMaxBytes+1))
	expectedSize := meta.Size - start
	if length >= 0 && expectedSize > length {
		expectedSize = length
	}
	if err != nil || int64(len(data)) != expectedSize {
		writeError(w, http.StatusServiceUnavailable, "task file read incomplete")
		return
	}
	if textView {
		if start+int64(len(data)) < meta.Size {
			if !utf8.Valid(data) {
				last := len(data) - 1
				for last >= 0 && !utf8.RuneStart(data[last]) {
					last--
				}
				if last >= 0 && !utf8.FullRune(data[last:]) {
					data = data[:last]
				}
			}
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			writeError(w, http.StatusUnsupportedMediaType, "file is not UTF-8 text or offset splits a character; use get for binary files")
			return
		}
		next := start + int64(len(data))
		httputil.WriteJSON(w, http.StatusOK, struct {
			TaskID     string `json:"taskId"`
			Name       string `json:"name"`
			Version    string `json:"version"`
			Text       string `json:"text"`
			Truncated  bool   `json:"truncated"`
			NextOffset int64  `json:"nextOffset"`
		}{id, name, version, string(data), next < meta.Size, next})
		return
	}
	digest := sha256.Sum256(data)
	if artifact, exists := catalog.published[name]; exists && hex.EncodeToString(digest[:]) != artifact.SHA256 {
		writeError(w, 503, "artifact content verification failed")
		return
	}
	if artifact, exists := catalog.published[name]; exists {
		w.Header().Set("X-AgentTeams-Artifact-ID", artifact.ArtifactID)
	}
	w.Header().Set("ETag", httputil.EtagQuoted(version))
	w.Header().Set("X-Content-SHA256", hex.EncodeToString(digest[:]))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
