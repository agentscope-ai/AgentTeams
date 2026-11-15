package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

func (h *CoordinationHandler) native(ctx context.Context, method, target string, body any) (map[string]any, error) {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, input)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("native HTTP %d", response.StatusCode)
	}
	var result map[string]any
	err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result)
	return result, err
}
func (h *CoordinationHandler) state(ctx context.Context, e coordinationEvent) (map[string]any, map[string]any, error) {
	if e.Kind == "handoff" {
		return map[string]any{}, map[string]any{}, nil
	}
	p, err := h.read(ctx, path.Join("teams", e.StorageTeam, "shared/projects", e.ProjectID, "meta.json"))
	if err != nil {
		return nil, nil, err
	}
	t := map[string]any{}
	if e.TaskID != "" {
		t, err = h.read(ctx, path.Join("teams", e.StorageTeam, "shared/tasks", e.TaskID, "meta.json"))
	}
	return p, t, err
}
func coordinationSession(e coordinationEvent) string {
	switch e.Kind {
	case "handoff":
		return "coordination-handoff:" + e.ID
	case "task_assigned":
		return "coordination-task:" + e.TaskID + ":" + e.DeliveryID
	case "project_ready":
		return "coordination-close:" + e.ProjectID
	case "task_submitted":
		return "coordination-review:" + e.TaskID + ":" + e.SubmissionID
	default:
		return "coordination-result:" + e.ProjectID
	}
}
func coordinationPayload(e coordinationEvent) map[string]any {
	original := e.RequesterRoute
	if e.Kind == "handoff" {
		original = e.SourceRoute
	}
	meta := map[string]any{"room_id": e.RoomID, "sender_id": e.Actor, "agent_matrix_user_id": e.Recipient, "event_id": e.ID, "api_projection": true, "task_id": e.TaskID, "project_id": e.ProjectID, "teamharness_requester_route": original,
		"teamharness_coordination": map[string]any{"id": e.ID, "kind": e.Kind, "task_id": e.TaskID, "project_id": e.ProjectID, "delivery_id": e.DeliveryID, "submission_id": e.SubmissionID}}
	text := e.Text
	switch e.Kind {
	case "handoff":
		meta["teamharness_coordinator_route"] = map[string]any{"channel": "matrix", "target_user": e.Actor, "target_session": e.RoomID}
		text += "\nUse one bounded start_project plan with complete leaf specs, owners and dependencies, and dispatchMode automatic when the finite plan is known. End after dispatch; do not poll or send duplicate assignments."
		roster, _ := json.Marshal(e.EligibleAssignees)
		text += "\nRegistered assignees with native notification support: " + string(roster) + ". Select execution owners from this list; other team members cannot receive this transport yet."
	case "task_assigned":
		text = "TASK_ASSIGNED: " + e.TaskID + "\nFirst call taskflow ack_task after checking the current assignee and execution identity. Write outputs only in this task. Use a fresh isolated test directory instead of pre-deleting an old directory. Upstream inputs are read-only: disable test caches beside imported inputs or copy inputs to your own workspace; do not clean upstream task directories.\n" + text
	case "task_submitted":
		text = "TASK_COMPLETED: " + e.TaskID + "\nCandidate result only. Use check_task to validate the current submission before accepting. After acceptance, the task system dispatches stored downstream specs. Do not redelegate or poll.\n" + text
	case "project_ready":
		text = "PROJECT_READY_TO_CLOSE: " + e.ProjectID + "\nEvery execution leaf has been accepted. Verify the complete request from these accepted results, then call projectflow complete_project with projectId and report (your concise final user report). That single operation closes coordinating parents, saves the report, completes the project and notifies the original coordinator. Do not modify accepted artifacts, repeat QA or end with a pending parent.\n" + text
	case "project_completed":
		meta["teamharness_workflow"] = map[string]any{"type": "PROJECT_COMPLETED", "project_id": e.ProjectID}
		meta["teamharness_final_report"] = e.FinalReport
		text = "PROJECT_COMPLETED: " + e.ProjectID + "\nThe final state is accepted. Reply with the concise final report; the dispatcher returns it to the original Matrix conversation. Do not send another message or poll.\n" + text
	}
	user := coordinationString(original, "target_user")
	if user == "" {
		user = e.Actor
	}
	return map[string]any{"user_id": user, "session_id": coordinationSession(e), "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}, "channel_meta": meta, "request_context": map[string]any{"teamharness_coordination": meta}}
}
func (h *CoordinationHandler) advance(ctx context.Context, d *coordinationDelivery, persist func() error) {
	if d.NativeID != "" && d.Event.Kind == "project_ready" {
		p, _, err := h.state(ctx, d.Event)
		if err != nil {
			d.Error = "closure state unavailable"
			return
		}
		if coordinationString(p, "status") == "completed" {
			d.Status = "completed"
			d.Error = ""
			return
		}
	}
	e, err := h.resolve(ctx, d.Event, &authpkg.CallerIdentity{Role: d.Event.ActorRole, Username: d.Event.ActorName, Team: d.Event.Team})
	if err != nil && !errors.Is(err, errCoordinationRejected) {
		d.Error = "canonical routing unavailable"
		return
	}
	if err != nil || e.Recipient != d.Event.Recipient {
		d.Status = "ignored"
		d.Error = "recipient or execution identity changed"
		return
	}
	p, t, err := h.state(ctx, e)
	if err != nil {
		d.Error = "canonical state unavailable"
		return
	}
	if d.NativeID == "" {
		status := coordinationString(p, "status")
		if status == "paused" || len(coordinationMap(p, "loop")) > 0 && lenAny(coordinationMap(p, "loop")["pause_sources"]) > 0 {
			return
		}
		if status == "cancelled" || status == "blocked" {
			d.Status = "ignored"
			return
		}
		taskStatus := coordinationString(t, "status")
		if e.Kind == "task_assigned" {
			if taskStatus == "prepared" {
				return
			}
			if taskStatus != "assigned" && taskStatus != "in_progress" {
				d.Status = "ignored"
				return
			}
		}
		if e.Kind == "task_submitted" && taskStatus != "submitted" {
			d.Status = "ignored"
			return
		}
		// Persist before admission. An ambiguous response cannot safely be retried
		// against a runtime that lacks an idempotent admission-receipt API.
		d.Status = "dispatching"
		if persist() != nil {
			return
		}
		response, err := h.native(ctx, "POST", e.TargetURL+"/api/console/chat/task", coordinationPayload(e))
		if err != nil {
			d.Status = "failed"
			d.Error = "native admission outcome unknown; operator recovery required"
			return
		}
		d.NativeID = coordinationString(response, "task_id")
		if d.NativeID == "" {
			d.Status = "failed"
			d.Error = "native admission returned no receipt"
			return
		}
		d.Status = "running"
		d.Error = ""
		return
	}
	if d.MatrixEventID == "" {
		label := map[string]string{"handoff": "已交接", "task_assigned": "已派发", "task_submitted": "已提交，待验收", "project_ready": "执行任务已验收，开始汇总", "project_completed": "已完成，正在回报"}[e.Kind]
		id, err := h.projectEvent(ctx, e, e.ActorRole, e.ActorName, e.RoomID, e.ID, label+" · "+coordinationLabel(e))
		if err == nil {
			d.MatrixEventID = id
		}
	}
	if e.Kind == "task_assigned" && d.AckEventID == "" && (coordinationString(t, "status") == "in_progress" || coordinationString(t, "status") == "submitted" || coordinationString(t, "status") == "completed") {
		id, err := h.projectEvent(ctx, e, e.RecipientKind, e.RecipientName, e.RoomID, e.ID+"-ack", "已接收任务，开始处理 · "+e.TaskID)
		if err == nil {
			d.AckEventID = id
		}
	}
	h.projectApprovals(ctx, e, d)
	result, err := h.native(ctx, "GET", e.TargetURL+"/api/console/chat/task/"+url.PathEscape(d.NativeID), nil)
	if err != nil {
		d.Error = "native execution status unavailable"
		return
	}
	status := coordinationString(result, "status")
	if status != "finished" && status != "failed" && status != "cancelled" {
		d.Error = ""
		return
	}
	outcome := coordinationMap(result, "result")
	if coordinationString(outcome, "status") != "completed" {
		d.Status = "failed"
		d.Error = "native execution did not complete"
		return
	}
	// A native turn ending is not business acceptance.
	latestProject, latest, err := h.state(ctx, e)
	if err != nil {
		d.Error = "canonical result unavailable"
		return
	}
	if e.Kind == "task_assigned" {
		state := coordinationString(latest, "status")
		if state != "submitted" && state != "completed" && state != "blocked" && state != "revision" && state != "cancelled" {
			d.Status = "failed"
			d.Error = "execution ended without durable task result"
			return
		}
	}
	if e.Kind == "task_submitted" && coordinationString(latest, "status") == "submitted" {
		d.Status = "failed"
		d.Error = "review ended without acceptance decision"
		return
	}
	if e.Kind == "project_ready" && coordinationString(latestProject, "status") != "completed" {
		d.Status = "failed"
		d.Error = "closure turn ended without completing the project"
		return
	}
	if e.Kind == "project_completed" && d.UserEventID == "" {
		text := coordinationAssistantText(outcome)
		if text == "" || text == "NO_REPLY" {
			d.Status = "failed"
			d.Error = "no final user report"
			return
		}
		id, err := h.projectEvent(ctx, e, e.RecipientKind, e.RecipientName, coordinationString(e.RequesterRoute, "target_session"), e.ID+"-user", text)
		if err != nil {
			d.Error = "final user projection unavailable"
			return
		}
		d.UserEventID = id
	}
	if d.MatrixEventID == "" || (e.Kind == "task_assigned" && d.AckEventID == "") {
		d.Error = "progress projection unavailable"
		return
	}
	if e.Kind == "task_submitted" {
		latestProject, _, readErr := h.state(ctx, e)
		if readErr != nil {
			d.Error = "closure state unavailable"
			return
		}
		if coordinationReady(latestProject) {
			ready, resolveErr := h.resolve(ctx, coordinationEvent{Kind: "project_ready", ProjectID: e.ProjectID}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: e.RecipientName, Team: e.Team})
			if resolveErr != nil {
				d.Error = "closure recipient unavailable"
				return
			}
			if _, archiveErr := h.deps.OSS.GetObject(ctx, coordinationDone+ready.ID+".json"); archiveErr == nil {
				d.Status = "completed"
				d.Error = ""
				return
			} else if !errors.Is(archiveErr, os.ErrNotExist) {
				d.Error = "closure receipt unavailable"
				return
			}
			next := coordinationDelivery{Event: ready, Status: "queued", Created: time.Now().UTC()}
			body, _ := json.Marshal(next)
			if queueErr := h.deps.OSS.PutObjectIfAbsent(ctx, coordinationPending+ready.ID+".json", body); queueErr != nil && !errors.Is(queueErr, oss.ErrPreconditionFailed) {
				d.Error = "closure notification not durable"
				return
			}
		}
	}
	d.Status = "completed"
	d.Error = ""
}
func lenAny(value any) int { list, _ := value.([]any); return len(list) }
func coordinationLabel(e coordinationEvent) string {
	if e.TaskID != "" {
		return e.TaskID
	}
	if e.ProjectID != "" {
		return e.ProjectID
	}
	return e.RecipientName
}
func coordinationAssistantText(result map[string]any) string {
	var texts []string
	messages, _ := result["output"].([]any)
	for _, item := range messages {
		m, _ := item.(map[string]any)
		if coordinationString(m, "type") != "message" || coordinationString(m, "role") != "assistant" {
			continue
		}
		content, _ := m["content"].([]any)
		for _, item := range content {
			b, _ := item.(map[string]any)
			if coordinationString(b, "type") == "text" {
				texts = append(texts, coordinationString(b, "text"))
			}
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}
func (h *CoordinationHandler) project(ctx context.Context, e coordinationEvent, kind, name, room, txn, text string) (string, error) {
	return h.projectContent(ctx, kind, name, room, txn, map[string]any{"msgtype": "m.text", "body": text})
}
func (h *CoordinationHandler) projectContent(ctx context.Context, kind, name, room, txn string, content map[string]any) (string, error) {
	if h.deps.Provisioner == nil || h.deps.MatrixConfig.ServerURL == "" || !strings.HasPrefix(room, "!") {
		return "", fmt.Errorf("Matrix projection unavailable")
	}
	token, err := h.deps.Provisioner.AgentMatrixToken(ctx, kind, name)
	if err != nil {
		return "", err
	}
	target := strings.TrimRight(h.deps.MatrixConfig.ServerURL, "/") + "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/send/m.room.message/" + url.PathEscape(txn)
	content["m.mentions"] = map[string]any{"user_ids": []string{}}
	content["org.agentteams.progress_only"] = true
	data, _ := json.Marshal(content)
	req, err := http.NewRequestWithContext(ctx, "PUT", target, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := h.http.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Matrix projection rejected")
	}
	var result map[string]any
	if err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return "", err
	}
	id := coordinationString(result, "event_id")
	if id == "" {
		return "", fmt.Errorf("Matrix projection returned no receipt")
	}
	return id, nil
}

func (h *CoordinationHandler) projectApprovals(ctx context.Context, e coordinationEvent, d *coordinationDelivery) {
	data, err := h.native(ctx, "GET", e.TargetURL+"/api/teamharness/coordination/approvals", nil)
	if err != nil {
		return
	}
	records, _ := data["approvals"].([]any)
	if d.Approvals == nil {
		d.Approvals = map[string]string{}
	}
	for _, item := range records {
		record, _ := item.(map[string]any)
		if coordinationString(record, "session_id") != coordinationSession(e) {
			continue
		}
		id, status := coordinationString(record, "request_id"), coordinationString(record, "status")
		if id == "" || status == "" || d.Approvals[id] == status {
			continue
		}
		content := map[string]any{}
		for key, value := range record {
			if key != "session_id" {
				content[key] = value
			}
		}
		_, err = h.projectContent(ctx, e.RecipientKind, e.RecipientName, e.RoomID, "approval-"+id+"-"+status, map[string]any{"msgtype": "m.text", "body": "操作审批 · " + coordinationString(record, "tool_name") + " · " + status, "org.agentteams.approval": content})
		if err == nil {
			d.Approvals[id] = status
		}
	}
}
