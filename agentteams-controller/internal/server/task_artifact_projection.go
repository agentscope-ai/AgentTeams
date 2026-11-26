package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

// projectTaskArtifact uses the original dispatch Agent's credentials and source
// room. Media bytes and identity come only from the immutable completion manifest.
func (h *CoordinationHandler) projectTaskArtifact(ctx context.Context, e coordinationEvent, file taskFinalArtifact, parentEvent, txn string) (string, error) {
	if h.deps.Provisioner == nil || h.deps.OSS == nil || h.deps.MatrixConfig.ServerURL == "" || !coordinationStorageTeamSafe(e.StorageTeam) || !isSafeTaskID(file.TaskID) || !validTaskFileName(file.Name) || file.Kind != "output" || !validArtifactDigest(file.SHA256) || file.Version != file.SHA256 || file.Size < 0 || file.Size > taskDownloadMaxBytes {
		return "", fmt.Errorf("final artifact projection unavailable")
	}
	key := path.Join("teams", e.StorageTeam, "shared/tasks", file.TaskID, "artifacts", file.SHA256)
	data, err := h.deps.OSS.GetObject(ctx, key)
	sum := sha256.Sum256(data)
	if err != nil || int64(len(data)) != file.Size || hex.EncodeToString(sum[:]) != file.SHA256 {
		return "", fmt.Errorf("final artifact content unavailable or changed")
	}
	token, err := h.deps.Provisioner.AgentMatrixToken(ctx, e.RecipientKind, e.RecipientName)
	if err != nil {
		return "", err
	}
	mediaType := artifactMediaType(file.Name)
	if artifactSensitive("shared/tasks/"+file.TaskID+"/"+file.Name, mediaType, data) {
		return "", fmt.Errorf("final artifact did not pass publication checks")
	}
	target := strings.TrimRight(h.deps.MatrixConfig.ServerURL, "/") + "/_matrix/media/v3/upload?filename=" + url.QueryEscape(path.Base(file.Name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mediaType)
	response, err := h.http.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("final artifact media upload rejected")
	}
	var upload struct {
		URI string `json:"content_uri"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&upload) != nil || !strings.HasPrefix(upload.URI, "mxc://") {
		return "", fmt.Errorf("final artifact media upload returned no receipt")
	}
	content := map[string]any{"msgtype": "m.file", "body": file.Name, "filename": path.Base(file.Name), "url": upload.URI, "info": map[string]any{"size": file.Size, "mimetype": mediaType},
		"m.relates_to": map[string]any{"rel_type": "com.agentteams.attachment", "event_id": parentEvent}, "org.agentteams.task_artifact": file}
	return h.sendMatrixContent(ctx, e.RecipientKind, e.RecipientName, coordinationString(e.RequesterRoute, "target_session"), txn, content, true)
}

func (h *CoordinationHandler) projectFinalArtifacts(ctx context.Context, e coordinationEvent, d *coordinationDelivery, persist func() error) error {
	if len(e.Artifacts) == 0 {
		return nil
	}
	if d.UserEventID == "" {
		return fmt.Errorf("final report must be projected before its artifacts")
	}
	if d.ArtifactEvents == nil {
		d.ArtifactEvents = map[string]string{}
	}
	for _, file := range e.Artifacts {
		if d.ArtifactEvents[file.ArtifactID] != "" {
			continue
		}
		event, err := h.projectArtifact(ctx, e, file, d.UserEventID, e.ID+"-artifact-"+file.ArtifactID)
		if err != nil || event == "" {
			return fmt.Errorf("final artifact projection unavailable")
		}
		d.ArtifactEvents[file.ArtifactID] = event
		// Keep per-file progress even when the next upload exceeds this queue lease.
		if err := persist(); err != nil {
			return err
		}
	}
	return nil
}

// Publish declared stage outputs as native Matrix attachments, related to the
// actual business result. No filename parsing or Dashboard-only card synthesis.
func (h *CoordinationHandler) projectStageArtifacts(ctx context.Context, e coordinationEvent, main map[string]any, d *coordinationDelivery, persist func() error) error {
	for _, id := range taskRoomStageIDs(e, main) {
		if !isSafeTaskID(id) {
			return fmt.Errorf("invalid artifact source Task")
		}
		stage, err := h.read(ctx, path.Join("teams", e.StorageTeam, "shared/tasks", id, "meta.json"))
		if err != nil || stage["main_task_id"] != e.MainTaskID || stage["task_id"] != id {
			return fmt.Errorf("artifact source Task changed")
		}
		var submission taskSubmission
		encoded, _ := json.Marshal(stage["task_submission"])
		if json.Unmarshal(encoded, &submission) != nil || submission.ID == "" || submission.ID != stage["submission_id"] {
			return fmt.Errorf("artifact submission unavailable")
		}
		if len(submission.Request.Artifacts) == 0 {
			continue
		}
		checkpoint := coordinationMap(coordinationMap(stage, "execution"), "checkpoint")
		parent := coordinationString(checkpoint, "reply_event")
		if checkpoint["delivery"] != "delivered" || !strings.HasPrefix(parent, "$") {
			return fmt.Errorf("stage result must precede its files")
		}
		worker, err := h.worker(ctx, coordinationString(stage, "assigned_to"))
		if err != nil {
			return err
		}
		target := e
		target.RecipientKind, target.RecipientName = authpkg.RoleWorker, worker.Name
		target.RequesterRoute = map[string]any{"target_session": e.RoomID}
		var index taskFileIndex
		encoded, _ = json.Marshal(stage["task_files"])
		if json.Unmarshal(encoded, &index) != nil {
			return fmt.Errorf("stage artifact manifest unavailable")
		}
		if d.ArtifactEvents == nil {
			d.ArtifactEvents = map[string]string{}
		}
		for _, ref := range submission.Request.Artifacts {
			cache := "stage:" + id + ":" + ref
			if d.ArtifactEvents[cache] != "" {
				continue
			}
			file, ok := index.Artifacts[ref]
			if !ok || file.ArtifactID != ref || file.Kind != "output" {
				return fmt.Errorf("stage artifact identity unavailable")
			}
			identity, _ := json.Marshal([]string{e.StorageTeam, e.RoomID, worker.Name, id, submission.ID, ref, file.Version})
			digest := sha256.Sum256(identity)
			// Stable across fanout notifications as well as queue retries.
			txn := "task-file-" + hex.EncodeToString(digest[:])
			event, err := h.projectArtifact(ctx, target, taskFinalArtifact{TaskID: id, publishedTaskFile: file}, parent, txn)
			if err != nil || event == "" {
				return fmt.Errorf("stage artifact projection unavailable")
			}
			d.ArtifactEvents[cache] = event
			if err := persist(); err != nil {
				return err
			}
		}
	}
	return nil
}
