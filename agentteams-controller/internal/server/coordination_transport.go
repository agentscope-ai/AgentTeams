package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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

var errNativeReceiptMissing = errors.New("native execution receipt is missing")

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
	if method == http.MethodGet && response.StatusCode == http.StatusNotFound {
		return nil, errNativeReceiptMissing
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("native HTTP %d", response.StatusCode)
	}
	var result map[string]any
	err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result)
	return result, err
}
func coordinationScopeID(e coordinationEvent) string {
	if e.MainTaskID != "" {
		return e.MainTaskID
	}
	return e.ProjectID
}
func coordinationPlanPath(e coordinationEvent, file string) string {
	if e.MainTaskID != "" {
		return path.Join("teams", e.StorageTeam, "shared/tasks", e.MainTaskID, file)
	}
	return path.Join("teams", e.StorageTeam, "shared/projects", e.ProjectID, file)
}
func coordinationClosing(kind string) bool {
	return kind == "project_ready" || kind == "main_task_ready"
}
func coordinationCompleted(kind string) bool {
	return kind == "project_completed" || kind == "main_task_completed"
}
func (h *CoordinationHandler) state(ctx context.Context, e coordinationEvent) (map[string]any, map[string]any, error) {
	if e.Kind == "handoff" {
		return map[string]any{}, map[string]any{}, nil
	}
	p, err := h.read(ctx, coordinationPlanPath(e, "meta.json"))
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
	suffix := ""
	if e.MainTaskID != "" && e.ID != "" {
		suffix = ":" + e.ID
	}
	switch e.Kind {
	case "handoff":
		return "coordination-handoff:" + e.ID
	case "task_assigned":
		return "coordination-task:" + e.TaskID + ":" + e.DeliveryID + suffix
	case "project_ready":
		return "coordination-close:" + e.ProjectID
	case "main_task_ready":
		return "coordination-close-task:" + e.MainTaskID + suffix
	case "task_submitted":
		return "coordination-review:" + e.TaskID + ":" + e.SubmissionID + suffix
	default:
		return "coordination-result:" + coordinationScopeID(e)
	}
}
func coordinationPayload(e coordinationEvent) map[string]any {
	original := e.RequesterRoute
	if e.Kind == "handoff" {
		original = e.SourceRoute
	}
	meta := map[string]any{"room_id": e.RoomID, "sender_id": e.Actor, "agent_matrix_user_id": e.Recipient, "event_id": e.ID, "api_projection": true, "task_id": e.TaskID, "main_task_id": e.MainTaskID, "project_id": e.ProjectID, "teamharness_requester_route": original,
		"teamharness_coordination": map[string]any{"id": e.ID, "kind": e.Kind, "task_id": e.TaskID, "main_task_id": e.MainTaskID, "project_id": e.ProjectID, "delivery_id": e.DeliveryID, "submission_id": e.SubmissionID, "plan_type": e.PlanType, "dispatch_mode": e.DispatchMode}}
	text := e.Text
	switch e.Kind {
	case "handoff":
		meta["teamharness_coordinator_route"] = map[string]any{"channel": "matrix", "target_user": e.Actor, "target_session": e.RoomID}
		if len(e.Tasks) > 0 {
			requests, _ := json.Marshal(e.Tasks)
			text += "\nThese main Tasks are already assigned to you in planned (pending analysis) state. Preserve each taskId, original spec and requester; use start_task on the SAME taskId to attach your execution plan, never create replacement main Tasks: " + string(requests)
			meta["teamharness_main_tasks"] = e.Tasks
		}
		text += "\nUse taskflow start_task to create one main Task with complete execution subtasks, owners and dependencies. Do not create a Project unless the user explicitly requested a Project. A task may optionally join an existing Project; do not invent one. End after dispatch; do not poll or send duplicate assignments."
		roster, _ := json.Marshal(e.EligibleAssignees)
		text += "\nRegistered assignees with native notification support: " + string(roster) + ". Select execution owners from this list; other team members cannot receive this transport yet."
	case "task_assigned":
		text = "TASK_ASSIGNED: " + e.TaskID + "\nUse a synchronized runtime ACK receipt when supplied; otherwise first call taskflow ack_task after checking the current assignee and execution identity. Write outputs only in this task. Use a fresh isolated test directory instead of pre-deleting an old directory. Upstream inputs are read-only: disable test caches beside imported inputs or copy inputs to your own workspace; do not clean upstream task directories.\n" + text
	case "task_submitted":
		guidance := "After acceptance, continue your existing manual DAG or Loop iteration/delegation protocol. Do not wait for a finite project_ready event for a Loop."
		if e.DispatchMode == "automatic" && e.PlanType != "loop" {
			guidance = "After acceptance, the task system dispatches stored downstream specs. Do not redelegate or poll."
		}
		text = "TASK_COMPLETED: " + e.TaskID + "\nCandidate result only. Validate the current submission with check_task or its synchronized runtime-prepared receipt before accepting. " + guidance + "\n" + text
	case "main_task_ready":
		text = "MAIN_TASK_READY_TO_CLOSE: " + e.MainTaskID + "\nEvery execution subtask is accepted. Verify the whole request and call taskflow complete_task with taskId and report. Do not complete an optional Project or wait for its other main tasks.\n" + text
	case "project_ready":
		text = "PROJECT_READY_TO_CLOSE: " + e.ProjectID + "\nEvery execution leaf has been accepted. Verify the complete request from these accepted results, then call projectflow complete_project with projectId and report (your concise final user report). That single operation closes coordinating parents, saves the report, completes the project and notifies the original coordinator. Do not modify accepted artifacts, repeat QA or end with a pending parent.\n" + text
	case "main_task_completed":
		meta["teamharness_workflow"] = map[string]any{"type": "MAIN_TASK_COMPLETED", "task_id": e.MainTaskID}
		meta["teamharness_final_report"] = e.FinalReport
		text = "MAIN_TASK_COMPLETED: " + e.MainTaskID + "\nReturn the saved accepted final report to the original conversation. Do not poll the optional Project.\n" + text
	case "project_completed":
		meta["teamharness_workflow"] = map[string]any{"type": "PROJECT_COMPLETED", "project_id": e.ProjectID}
		meta["teamharness_final_report"] = e.FinalReport
		text = "PROJECT_COMPLETED: " + e.ProjectID + "\nThe final state is accepted. Reply with the concise final report; the dispatcher returns it to the original Matrix conversation. Do not send another message or poll.\n" + text
	}
	if e.Protocol == "task/v1" {
		switch e.Kind {
		case "handoff":
			requests, _ := json.Marshal(e.Tasks)
			roster, _ := json.Marshal(e.EligibleAssignees)
			text = e.Text + "\nThese main tasks are already registered. Preserve their IDs and original requirements. Use the common task tool action plan with the complete execution DAG, selecting registered assignees; the task system creates the room and dispatches ready steps. End after planning; do not send assignments or poll. Tasks: " + string(requests) + "\nRegistered assignees: " + string(roster)
		case "task_assigned":
			text = "TASK_ASSIGNED: " + e.TaskID + "\nUse the framework-prepared task begin receipt. Execute only this task, keep inputs read-only, and submit files relative to its prepared output directory through the common task tool. Do not repeat begin when already prepared.\n" + e.Text
		case "task_submitted":
			text = "TASK_SUBMITTED: " + e.TaskID + "\nReview the exact candidate and its artifacts using the common task tool. Accept or reject this submission ID with independent business evidence; the task system advances dependencies. End after review; do not dispatch or poll.\n" + e.Text
		case "main_task_ready":
			text = "MAIN_TASK_READY_TO_CLOSE: " + e.MainTaskID + "\nVerify the whole original request against accepted results and use the common task tool action complete with your final report. The task system returns the report and accepted artifacts to the original Human conversation using the first dispatch Agent identity. Do not send another notification.\n" + e.Text
		}
	}
	user := coordinationString(original, "target_user")
	if user == "" {
		user = e.Actor
	}
	return map[string]any{"user_id": user, "session_id": coordinationSession(e), "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}, "channel_meta": meta, "request_context": map[string]any{"teamharness_coordination": meta}}
}
func (h *CoordinationHandler) advance(ctx context.Context, d *coordinationDelivery, persist func() error) {
	if d.NativeID != "" && coordinationClosing(d.Event.Kind) {
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
	d.Event.ActorName = e.ActorName
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
			if h.taskNotifications != nil {
				mainKey := coordinationPlanPath(e, "meta.json")
				childKey := path.Join("teams", e.StorageTeam, "shared/tasks", e.TaskID, "meta.json")
				if code, _ := h.taskNotifications.taskExecutionAdmissionFailure(ctx, mainKey, p); code != 0 {
					d.Error = "main task execution is gated"
					return
				}
				if code, _ := h.taskNotifications.taskExecutionAdmissionFailure(ctx, childKey, t); code != 0 {
					d.Error = "task execution is gated"
					return
				}
			}
		}
		if e.Kind == "task_submitted" && taskStatus != "submitted" {
			d.Status = "ignored"
			return
		}
		if d.Transport == "matrix" {
			if coordinationCompleted(e.Kind) {
				// The canonical completed state/report was re-read by resolve above.
				// Relay as the registered Manager without opening another Agent turn.
				if strings.TrimSpace(e.FinalReport) == "" {
					d.Status = "failed"
					d.Error = "no saved final user report"
					return
				}
				if d.UserEventID == "" {
					if d.ReportSnapshot == "" {
						d.ReportSnapshot = e.FinalReport
						if persist() != nil {
							return
						}
					}

					id, sendErr := h.projectEvent(ctx, e, e.RecipientKind, e.RecipientName, coordinationString(e.RequesterRoute, "target_session"), e.ID+"-user", d.ReportSnapshot)
					if sendErr != nil {
						d.Error = "final user projection unavailable"
						return
					}
					d.UserEventID, d.MatrixEventID = id, id
					if persist() != nil {
						return
					}
				}
				if err := h.projectFinalArtifacts(ctx, e, d, persist); err != nil {
					d.Error = err.Error()
					return
				}

				d.Status, d.Error = "completed", ""
				return
			}
			// Matrix transaction IDs make an uncertain send safe to retry.
			id, sendErr := h.sendMatrixContent(ctx, e.ActorRole, e.ActorName, e.RoomID, e.ID, coordinationMatrixNotification(e, p), false)
			if sendErr != nil {
				d.Error = "Matrix coordination notification unavailable"
				return
			}
			d.NativeID, d.MatrixEventID = "matrix:"+id, id
			d.Status, d.Error = "running", ""
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
	if d.Transport == "matrix" {
		switch e.Kind {
		case "task_assigned":
			h.projectApprovals(ctx, e, d)
			status := coordinationString(t, "status")
			if status == "cancelled" || status == "revision" || status == "blocked" {
				d.Status, d.Error = "ignored", "assignment no longer executable"
				return
			}
			if status == "in_progress" || status == "submitted" || status == "completed" {
				if d.AckEventID == "" {
					id, sendErr := h.projectEvent(ctx, e, e.RecipientKind, e.RecipientName, e.RoomID, e.ID+"-ack", "已接收「"+coordinationTaskTitle(e, p)+"」，开始处理。")
					if sendErr != nil {
						return
					}
					d.AckEventID, d.Error = id, ""
				}
				if status != "in_progress" {
					d.Status = "completed"
				}
			}
		case "handoff":
			// The authenticated Matrix notification is the handoff receipt.
			d.Status, d.Error = "completed", ""
		case "task_submitted":
			if coordinationString(t, "status") == "submitted" {
				return
			}
			if coordinationString(t, "status") == "completed" {
				if err := h.queueClosure(ctx, e, p); err != nil {
					d.Error = err.Error()
					return
				}
			}
			d.Status, d.Error = "completed", ""
		}
		return // Business state, not delivery or a model response, proves acceptance/closure.
	}

	if d.MatrixEventID == "" {
		label := map[string]string{"handoff": "已交接", "task_assigned": "已派发", "task_submitted": "已提交，待验收", "main_task_ready": "子任务已验收，开始汇总", "main_task_completed": "主任务已完成，正在回报", "project_ready": "执行任务已验收，开始汇总", "project_completed": "已完成，正在回报"}[e.Kind]
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
		if errors.Is(err, errNativeReceiptMissing) {
			d.Status = "failed"
			d.Error = "native execution receipt no longer available; operator recovery required"
			return
		}
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
	if coordinationClosing(e.Kind) && coordinationString(latestProject, "status") != "completed" {
		d.Status = "failed"
		d.Error = "closure turn ended without completing the project"
		return
	}
	if coordinationCompleted(e.Kind) && d.UserEventID == "" {
		text := coordinationAssistantText(outcome)
		if text == "" || text == "NO_REPLY" {
			d.Status = "failed"
			d.Error = "no final user report"
			return
		}
		if d.ReportSnapshot == "" {
			d.ReportSnapshot = text
			if persist() != nil {
				return
			}
		}
		id, err := h.projectEvent(ctx, e, e.RecipientKind, e.RecipientName, coordinationString(e.RequesterRoute, "target_session"), e.ID+"-user", d.ReportSnapshot)
		if err != nil {
			d.Error = "final user projection unavailable"
			return
		}
		d.UserEventID = id
	}
	if coordinationCompleted(e.Kind) {
		if err := h.projectFinalArtifacts(ctx, e, d, persist); err != nil {
			d.Error = err.Error()
			return
		}
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
		if err := h.queueClosure(ctx, e, latestProject); err != nil {
			d.Error = err.Error()
			return
		}
	}

	d.Status = "completed"
	d.Error = ""
}
func coordinationTransport(kind string, runtime ...string) string {
	if len(runtime) > 0 && coordinationRuntimeSupported(runtime[0]) {
		return "matrix"
	}
	if os.Getenv("AGENTTEAMS_COORDINATION_TRANSPORT") == "matrix" && kind != "handoff" {
		return "matrix"
	}
	return ""
}

func (h *CoordinationHandler) queueClosure(ctx context.Context, e coordinationEvent, p map[string]any) error {
	if !coordinationReady(p) {
		return nil
	}
	kind := "project_ready"
	if e.MainTaskID != "" {
		kind = "main_task_ready"
	}
	ready, err := h.resolve(ctx, coordinationEvent{Kind: kind, ProjectID: e.ProjectID, MainTaskID: e.MainTaskID}, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: e.RecipientName, Team: e.Team})
	if err != nil {
		return fmt.Errorf("closure recipient unavailable")
	}
	if _, err = h.deps.OSS.GetObject(ctx, coordinationDone+ready.ID+".json"); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("closure receipt unavailable")
	}
	next := coordinationDelivery{Event: ready, Transport: coordinationTransport(ready.Kind, ready.RecipientRuntime), Status: "queued", Created: time.Now().UTC()}
	body, _ := json.Marshal(next)
	if err = h.deps.OSS.PutObjectIfAbsent(ctx, coordinationPending+ready.ID+".json", body); err != nil && !errors.Is(err, oss.ErrPreconditionFailed) {
		return fmt.Errorf("closure notification not durable")
	}
	h.wakeRunner()
	return nil
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

func coordinationTaskTitle(e coordinationEvent, p map[string]any) string {
	nodes, _ := p["tasks"].([]any)
	for _, item := range nodes {
		n, _ := item.(map[string]any)
		if coordinationString(n, "task_id") == e.TaskID && coordinationString(n, "title") != "" {
			return coordinationString(n, "title")
		}
	}
	return e.TaskID
}

func coordinationMatrixAssignment(e coordinationEvent, p map[string]any) map[string]any {
	nodes, _ := p["tasks"].([]any)
	titles := map[string]string{}
	var dependencies []any
	for _, item := range nodes {
		n, _ := item.(map[string]any)
		titles[coordinationString(n, "task_id")] = coordinationString(n, "title")
		if coordinationString(n, "task_id") == e.TaskID {
			dependencies, _ = n["depends_on"].([]any)
		}
	}
	var prior []string
	for _, dependency := range dependencies {
		id, _ := dependency.(string)
		if titles[id] != "" {
			prior = append(prior, "「"+titles[id]+"」")
		}
	}
	prefix := "任务已分配。"
	if len(prior) > 0 {
		prefix = "前置的" + strings.Join(prior, "、") + "环节已验收通过。"
	}
	body := e.Recipient + " " + prefix + "请继续开始「" + coordinationTaskTitle(e, p) + "」任务，按任务要求完成并提交结果。"
	formatted := strings.ReplaceAll(html.EscapeString(body), html.EscapeString(e.Recipient), `<a href="https://matrix.to/#/`+url.PathEscape(e.Recipient)+`">`+html.EscapeString(strings.Split(e.Recipient, ":")[0])+`</a>`)
	return map[string]any{"msgtype": "m.text", "body": body, "format": "org.matrix.custom.html", "formatted_body": formatted, "m.mentions": map[string]any{"user_ids": []string{e.Recipient}}, "org.agentteams.coordination": coordinationPayload(e)}
}
func coordinationMatrixNotification(e coordinationEvent, p map[string]any) map[string]any {
	content := coordinationMatrixAssignment(e, p)
	caption := ""
	switch e.Kind {
	case "handoff":
		caption = "请接收这项需求，创建主任务并安排执行。"
	case "task_submitted":
		caption = "「" + coordinationTaskTitle(e, p) + "」已提交结果，请检查本次提交并验收。"
	case "project_ready", "main_task_ready":
		caption = "所有执行环节已验收，请汇总最终结果并完成本次任务。"
	}
	if caption != "" {
		body := e.Recipient + " " + caption
		content["body"] = body
		content["formatted_body"] = strings.ReplaceAll(html.EscapeString(body), html.EscapeString(e.Recipient), `<a href="https://matrix.to/#/`+url.PathEscape(e.Recipient)+`">`+html.EscapeString(strings.Split(e.Recipient, ":")[0])+`</a>`)
	}
	return content
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
	content := map[string]any{"msgtype": "m.text", "body": text}
	if coordinationCompleted(e.Kind) {
		if user := coordinationString(e.RequesterRoute, "target_user"); strings.HasPrefix(user, "@") {
			content["m.mentions"] = map[string]any{"user_ids": []string{user}}
		}
	}
	return h.projectContent(ctx, kind, name, room, txn, content)
}
func (h *CoordinationHandler) projectContent(ctx context.Context, kind, name, room, txn string, content map[string]any) (string, error) {
	return h.sendMatrixContent(ctx, kind, name, room, txn, content, true)
}
func (h *CoordinationHandler) sendMatrixContent(ctx context.Context, kind, name, room, txn string, content map[string]any, progressOnly bool) (string, error) {
	if h.deps.Provisioner == nil || h.deps.MatrixConfig.ServerURL == "" || !strings.HasPrefix(room, "!") {
		return "", fmt.Errorf("Matrix projection unavailable")
	}
	token, err := h.deps.Provisioner.AgentMatrixToken(ctx, kind, name)
	if err != nil {
		return "", err
	}
	target := strings.TrimRight(h.deps.MatrixConfig.ServerURL, "/") + "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/send/m.room.message/" + url.PathEscape(txn)
	if _, exists := content["m.mentions"]; !exists {
		content["m.mentions"] = map[string]any{"user_ids": []string{}}
	}
	content["org.agentteams.progress_only"] = progressOnly
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
	if e.RecipientRuntime != "qwenpaw" {
		return
	}
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
