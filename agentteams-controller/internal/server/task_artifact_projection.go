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
	content := map[string]any{"msgtype": "m.file", "body": file.TaskID + " / " + file.Name, "filename": path.Base(file.Name), "url": upload.URI, "info": map[string]any{"size": file.Size, "mimetype": mediaType},
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
