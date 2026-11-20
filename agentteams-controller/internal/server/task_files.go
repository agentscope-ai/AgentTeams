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
	"sort"
	"strconv"
	"strings"
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
	keys := map[string]string{}
	published := map[string]publishedTaskFile{}
	names := []string{}
	prefix := "shared/tasks/" + id + "/"
	var deliverables []string
	if raw := record["deliverables"]; len(raw) > 0 && json.Unmarshal(raw, &deliverables) != nil {
		writeError(w, 503, "invalid task files")
		return
	}
	for _, declared := range deliverables {
		rel := strings.TrimPrefix(declared, prefix)
		if rel == declared || rel == "." || path.Clean(rel) != rel || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") || strings.ContainsAny(rel, "\\\x00") {
			writeError(w, http.StatusServiceUnavailable, "invalid task file reference")
			return
		}
		name := strings.TrimPrefix(rel, "workspace/")
		key := path.Dir(metaKey) + "/" + rel
		if previous, exists := keys[name]; exists {
			if previous != key {
				writeError(w, http.StatusServiceUnavailable, "ambiguous task file name")
				return
			}
			continue
		}
		keys[name], names = key, append(names, name)
	}
	var index taskFileIndex
	if raw := record["task_files"]; len(raw) > 0 {
		if json.Unmarshal(raw, &index) != nil || index.Artifacts == nil || index.Heads == nil {
			writeError(w, 503, "invalid task file index")
			return
		}
		for name, identity := range index.Heads {
			artifact, exists := index.Artifacts[identity]
			if !exists || artifact.ArtifactID != identity || artifact.Name != name || !validTaskFileName(name) || !validArtifactDigest(artifact.SHA256) || artifact.Version != artifact.SHA256 {
				writeError(w, 503, "invalid task file reference")
				return
			}
			if _, exists := keys[name]; !exists {
				names = append(names, name)
			}
			keys[name], published[name] = path.Dir(metaKey)+"/artifacts/"+artifact.SHA256, artifact
		}
	}
	name := r.URL.Query().Get("name")
	if identity := r.URL.Query().Get("artifactId"); identity != "" {
		artifact, exists := index.Artifacts[identity]
		if !exists || (name != "" && name != artifact.Name) {
			writeError(w, 404, "task artifact not found")
			return
		}
		if artifact.ArtifactID != identity || !validTaskFileName(artifact.Name) || !validArtifactDigest(artifact.SHA256) || artifact.Version != artifact.SHA256 {
			writeError(w, 503, "invalid task file reference")
			return
		}
		name = artifact.Name
		keys[name], published[name] = path.Dir(metaKey)+"/artifacts/"+artifact.SHA256, artifact
	}
	if name == "" {
		sort.Strings(names)
		files := make([]taskFileResource, 0, len(names))
		for _, name := range names {
			meta, err := h.oss.StatMeta(r.Context(), keys[name])
			if err != nil || meta.ETag == "" {
				writeError(w, http.StatusServiceUnavailable, "declared task file unavailable")
				return
			}
			file := taskFileResource{Name: name, Version: httputil.BareETag(meta.ETag), Size: meta.Size}
			if artifact, exists := published[name]; exists {
				if artifact.Size != meta.Size {
					writeError(w, 503, "artifact size verification failed")
					return
				}
				file.ArtifactID, file.Kind, file.Version, file.SHA256 = artifact.ArtifactID, artifact.Kind, artifact.Version, artifact.SHA256
			}
			files = append(files, file)
		}
		httputil.WriteJSON(w, http.StatusOK, struct {
			TaskID string             `json:"taskId"`
			Files  []taskFileResource `json:"files"`
		}{TaskID: id, Files: files})
		return
	}
	key, exists := keys[name]
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
	if artifact, exists := published[name]; exists {
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
	if artifact, exists := published[name]; exists && hex.EncodeToString(digest[:]) != artifact.SHA256 {
		writeError(w, 503, "artifact content verification failed")
		return
	}
	if artifact, exists := published[name]; exists {
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
