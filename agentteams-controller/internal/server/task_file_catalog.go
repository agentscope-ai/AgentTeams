package server

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

type taskFileCatalog struct {
	keys      map[string]string
	published map[string]publishedTaskFile
	names     []string
	index     taskFileIndex
}

// resolveTaskFileCatalog resolves only this task's declared outputs and registered
// immutable references. Both explicit file reads and begin use this boundary.
func resolveTaskFileCatalog(metaKey, id string, record map[string]json.RawMessage) (taskFileCatalog, error) {
	keys := map[string]string{}
	published := map[string]publishedTaskFile{}
	names := []string{}
	prefix := "shared/tasks/" + id + "/"
	var deliverables []string
	// New submissions resolve names through their immutable registry. Their
	// legacy deliverables projection contains blob keys, not extra file names.
	if len(record["task_submission"]) == 0 && len(record["task_completion"]) == 0 {
		if raw := record["deliverables"]; len(raw) > 0 && json.Unmarshal(raw, &deliverables) != nil {
			return taskFileCatalog{}, errors.New("invalid task files")
		}
	}
	for _, declared := range deliverables {
		rel := strings.TrimPrefix(declared, prefix)
		if rel == declared || rel == "." || path.Clean(rel) != rel || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") || strings.ContainsAny(rel, "\\\x00") {
			return taskFileCatalog{}, errors.New("invalid task file reference")
		}
		name := strings.TrimPrefix(rel, "workspace/")
		key := path.Dir(metaKey) + "/" + rel
		if previous, exists := keys[name]; exists {
			if previous != key {
				return taskFileCatalog{}, errors.New("ambiguous task file name")
			}
			continue
		}
		keys[name], names = key, append(names, name)
	}
	var index taskFileIndex
	if raw := record["task_files"]; len(raw) > 0 {
		if json.Unmarshal(raw, &index) != nil || index.Artifacts == nil || index.Heads == nil {
			return taskFileCatalog{}, errors.New("invalid task file index")
		}
		for name, identity := range index.Heads {
			artifact, exists := index.Artifacts[identity]
			if !exists || artifact.ArtifactID != identity || artifact.Name != name || !validTaskFileName(name) || !validArtifactDigest(artifact.SHA256) || artifact.Version != artifact.SHA256 {
				return taskFileCatalog{}, errors.New("invalid task file reference")
			}
			if _, exists := keys[name]; !exists {
				names = append(names, name)
			}
			keys[name], published[name] = path.Dir(metaKey)+"/artifacts/"+artifact.SHA256, artifact
		}
	}
	// The candidate report is addressable by an immutable ID without appearing
	// as another output/head or exposing its private object key.
	var submission taskSubmission
	if raw := record["task_submission"]; len(raw) > 0 {
		var submissionID string
		if json.Unmarshal(raw, &submission) != nil || json.Unmarshal(record["submission_id"], &submissionID) != nil || submission.ID == "" || submission.ID != submissionID {
			return taskFileCatalog{}, errors.New("invalid task submission reference")
		}
		file, _ := taskSubmissionReport(submission)
		if index.Artifacts == nil {
			index.Artifacts = map[string]publishedTaskFile{}
		}
		index.Artifacts[file.ArtifactID] = publishedTaskFile{ArtifactID: file.ArtifactID, Name: file.Name, Kind: file.Kind, Version: file.Version, SHA256: file.SHA256, Size: file.Size}
	}
	return taskFileCatalog{keys: keys, published: published, names: names, index: index}, nil
}

func (h *ProjectHandler) taskFileResources(ctx context.Context, metaKey, id string, record map[string]json.RawMessage) ([]taskFileResource, error) {
	catalog, err := resolveTaskFileCatalog(metaKey, id, record)
	if err != nil {
		return nil, err
	}
	sort.Strings(catalog.names)
	files := make([]taskFileResource, 0, len(catalog.names))
	for _, name := range catalog.names {
		meta, err := h.oss.StatMeta(ctx, catalog.keys[name])
		if err != nil || meta.ETag == "" {
			return nil, errors.New("declared task file unavailable")
		}
		file := taskFileResource{Name: name, Kind: "output", Version: httputil.BareETag(meta.ETag), Size: meta.Size}
		if artifact, exists := catalog.published[name]; exists {
			if artifact.Size != meta.Size {
				return nil, errors.New("artifact size verification failed")
			}
			file.ArtifactID, file.Kind, file.Version, file.SHA256 = artifact.ArtifactID, artifact.Kind, artifact.Version, artifact.SHA256
		}
		files = append(files, file)
	}
	return files, nil
}
