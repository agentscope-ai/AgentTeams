package server

import (
	"encoding/json"
	"net/http"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

type taskDeliveredArtifact struct {
	TaskID        string `json:"taskId"`
	Name          string `json:"name"`
	NameTruncated bool   `json:"nameTruncated,omitempty"`
	ArtifactID    string `json:"artifactId"`
	Version       string `json:"version"`
	MessageID     string `json:"messageId"`
}

type taskDeliveryRecord struct {
	DeliveredAt           string                  `json:"deliveredAt,omitempty"`
	EventID               string                  `json:"eventId"`
	TaskID                string                  `json:"taskId"`
	SourceEventID         string                  `json:"sourceEventId,omitempty"`
	RoomID                string                  `json:"roomId"`
	Status                string                  `json:"status"`
	Report                string                  `json:"report"`
	ReportTruncated       bool                    `json:"reportTruncated"`
	ReportMessageID       string                  `json:"reportMessageId"`
	ArtifactCount         int                     `json:"artifactCount"`
	ArtifactListTruncated bool                    `json:"artifactListTruncated"`
	Artifacts             []taskDeliveredArtifact `json:"artifacts"`
}

// ListTaskDeliveries is a common read surface for original-dispatch context.
// It reads confirmed outgoing records rather than depending on a creation reply
// reaching the caller, or inspecting a Harness's private history/database.
func (h *CoordinationHandler) ListTaskDeliveries(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || caller.Role != authpkg.RoleManager || (caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.deps.Namespace) {
		writeError(w, 403, "registered dispatch Manager required")
		return
	}
	manager, err := h.manager(r.Context(), caller.Username)
	if err != nil || manager.Status.MatrixUserID == "" {
		writeError(w, 403, "registered dispatch Manager required")
		return
	}
	room, after := r.URL.Query().Get("roomId"), r.URL.Query().Get("after")
	if !strings.HasPrefix(room, "!") {
		writeError(w, 400, "original conversation required")
		return
	}
	if h.deps.OSS == nil {
		writeError(w, 503, "task delivery records unavailable")
		return
	}
	records := []taskDeliveryRecord{}
	cursor := ""
	for {
		entries, next, err := h.deps.OSS.ListObjectsRecursive(r.Context(), coordinationDone, cursor, 200)
		if err != nil {
			writeError(w, 503, "task delivery records unavailable")
			return
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name, "/") || !strings.HasSuffix(entry.Name, ".json") {
				continue
			}
			data, err := h.deps.OSS.GetObject(r.Context(), path.Join(coordinationDone, entry.Name))
			if err != nil {
				writeError(w, 503, "task delivery record unavailable")
				return
			}
			var d coordinationDelivery
			if json.Unmarshal(data, &d) != nil {
				writeError(w, 503, "invalid task delivery record")
				return
			}
			e := d.Event
			if d.Status != "completed" || e.Kind != "main_task_completed" || e.MainTaskID == "" || e.ID <= after || e.RecipientKind != "manager" || e.RecipientName != manager.Name || e.Recipient != manager.Status.MatrixUserID || coordinationString(e.RequesterRoute, "target_session") != room || d.UserEventID == "" || strings.TrimSpace(d.ReportSnapshot) == "" {
				continue
			}
			complete := true
			for _, file := range e.Artifacts {
				if file.ArtifactID == "" || d.ArtifactEvents[file.ArtifactID] == "" {
					complete = false
					break
				}
			}
			if !complete {
				continue
			}
			row := taskDeliveryRecord{EventID: e.ID, TaskID: e.MainTaskID, SourceEventID: e.OriginEventID, RoomID: room, Status: "completed", Report: coordinationClip(d.ReportSnapshot, 4096), ReportTruncated: utf8.RuneCountInString(d.ReportSnapshot) > 4096, ReportMessageID: d.UserEventID, ArtifactCount: len(e.Artifacts), ArtifactListTruncated: len(e.Artifacts) > 16, Artifacts: []taskDeliveredArtifact{}}
			if !d.DeliveredAt.IsZero() {
				row.DeliveredAt = d.DeliveredAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
			}
			for _, file := range e.Artifacts[:min(len(e.Artifacts), 16)] {
				row.Artifacts = append(row.Artifacts, taskDeliveredArtifact{TaskID: file.TaskID, Name: coordinationClip(file.Name, 256), NameTruncated: utf8.RuneCountInString(file.Name) > 256, ArtifactID: file.ArtifactID, Version: file.Version, MessageID: d.ArtifactEvents[file.ArtifactID]})
			}
			records = append(records, row)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	sort.Slice(records, func(i, j int) bool { return records[i].EventID < records[j].EventID })
	nextCursor := ""
	if len(records) > 32 {
		records = records[:32]
		nextCursor = records[len(records)-1].EventID
	}
	httputil.WriteJSON(w, 200, struct {
		Deliveries []taskDeliveryRecord `json:"deliveries"`
		NextCursor string               `json:"nextCursor,omitempty"`
	}{records, nextCursor})
}
