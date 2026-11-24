package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Coordination delivers control events; the project/task documents remain the
// only business state machine. Callers never supply an Agent network address.
type CoordinationHandler struct {
	deps              ServerDeps
	wake              chan struct{}
	http              *http.Client
	workerURL         func(string, map[string]string) string
	managerURL        func(string) string
	projectEvent      func(context.Context, coordinationEvent, string, string, string, string, string) (string, error)
	projectArtifact   func(context.Context, coordinationEvent, taskFinalArtifact, string, string) (string, error)
	taskNotifications *ProjectHandler
	assistant         *AssistantHandler
}

type coordinationEvent struct {
	Protocol          string              `json:"protocol,omitempty"`
	Artifacts         []taskFinalArtifact `json:"artifacts,omitempty"`
	Tasks             []mainTaskRequest   `json:"tasks,omitempty"`
	Kind              string              `json:"kind"`
	Actor             string              `json:"actor"`
	Recipient         string              `json:"recipient"`
	MainTaskID        string              `json:"main_task_id,omitempty"`
	ProjectID         string              `json:"project_id,omitempty"`
	TaskID            string              `json:"task_id,omitempty"`
	DeliveryID        string              `json:"delivery_id,omitempty"`
	SubmissionID      string              `json:"submission_id,omitempty"`
	OriginEventID     string              `json:"origin_event_id,omitempty"`
	RoomID            string              `json:"room_id,omitempty"`
	Text              string              `json:"text"`
	SourceRoute       map[string]any      `json:"source_route,omitempty"`
	RequesterRoute    map[string]any      `json:"requester_route,omitempty"`
	ID                string              `json:"id"`
	Team              string              `json:"team"`
	StorageTeam       string              `json:"storage_team"`
	ActorName         string              `json:"actor_name"`
	ActorRole         string              `json:"actor_role"`
	RecipientName     string              `json:"recipient_name"`
	RecipientKind     string              `json:"recipient_kind"`
	TargetURL         string              `json:"-"`
	RecipientRuntime  string              `json:"-"`
	External          bool                `json:"external,omitempty"`
	EligibleAssignees []string            `json:"eligible_assignees,omitempty"`
	FinalReport       string              `json:"final_report,omitempty"`
	PlanType          string              `json:"-"`
	DispatchMode      string              `json:"-"`
}

var coordinationSafeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var errCoordinationRejected = errors.New("coordination event does not match current task ownership or execution identity")

func coordinationStorageTeamSafe(team string) bool {
	return team != "" && team != "." && team != ".." && strings.TrimSpace(team) == team &&
		!strings.ContainsAny(team, "/\\") && strings.IndexFunc(team, unicode.IsControl) < 0
}

func NewCoordinationHandler(deps ServerDeps) *CoordinationHandler {
	h := &CoordinationHandler{deps: deps, wake: make(chan struct{}, 1), http: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	h.workerURL = func(name string, env map[string]string) string {
		return fmt.Sprintf("http://%s%s:%s", deps.ContainerPrefix, name, service.EffectiveWorkerConsolePort(env))
	}
	h.managerURL = func(name string) string {
		return fmt.Sprintf("http://%s:18799", authpkg.ResourcePrefix(deps.ResourcePrefix).ManagerPodName(name))
	}
	h.projectEvent = h.project
	h.projectArtifact = h.projectTaskArtifact
	return h
}

func (h *CoordinationHandler) read(ctx context.Context, key string) (map[string]any, error) {
	b, err := h.deps.OSS.GetObject(ctx, key)
	if err != nil {
		return nil, err
	}
	var x map[string]any
	err = json.Unmarshal(b, &x)
	return x, err
}
func coordinationString(x map[string]any, key string) string { v, _ := x[key].(string); return v }
func coordinationMap(x map[string]any, key string) map[string]any {
	v, _ := x[key].(map[string]any)
	return v
}

func (h *CoordinationHandler) worker(ctx context.Context, name string) (*v1beta1.Worker, error) {
	var list v1beta1.WorkerList
	var reader client.Reader = h.deps.Client
	if h.deps.APIReader != nil {
		reader = h.deps.APIReader
	}
	if err := reader.List(ctx, &list, client.InNamespace(h.deps.Namespace)); err != nil {
		return nil, err
	}
	var found *v1beta1.Worker
	name = strings.TrimPrefix(name, "user:")
	for i := range list.Items {
		w := &list.Items[i]
		if name == w.Name || name == w.Spec.EffectiveWorkerName(w.Name) || (w.Status.MatrixUserID != "" && name == w.Status.MatrixUserID) {
			if found != nil {
				return nil, errCoordinationRejected
			}
			found = w
		}
	}
	if found == nil {
		return nil, errCoordinationRejected
	}
	return found, nil
}
func (h *CoordinationHandler) manager(ctx context.Context, name string) (*v1beta1.Manager, error) {
	var list v1beta1.ManagerList
	if err := h.deps.Client.List(ctx, &list, client.InNamespace(h.deps.Namespace)); err != nil {
		return nil, err
	}
	var found *v1beta1.Manager
	for i := range list.Items {
		m := &list.Items[i]
		if name == m.Name || (m.Status.MatrixUserID != "" && name == m.Status.MatrixUserID) || (name == "manager" && m.Name == "default") {
			if found != nil {
				return nil, errCoordinationRejected
			}
			found = m
		}
	}
	if found == nil {
		return nil, errCoordinationRejected
	}
	return found, nil
}
func (h *CoordinationHandler) memberTeam(ctx context.Context, worker, role, scope string) (*v1beta1.Team, error) {
	var teams v1beta1.TeamList
	if err := h.deps.Client.List(ctx, &teams, client.InNamespace(h.deps.Namespace)); err != nil {
		return nil, err
	}
	var found *v1beta1.Team
	for i := range teams.Items {
		tm := &teams.Items[i]
		if scope != "" && scope != tm.Name && scope != tm.Spec.EffectiveTeamName(tm.Name) {
			continue
		}
		for _, member := range tm.Spec.WorkerMembers {
			if member.Name == worker && (role == "" || member.Role == role) {
				if found != nil {
					return nil, errCoordinationRejected
				}
				found = tm
			}
		}
	}
	if found == nil {
		return nil, errCoordinationRejected
	}
	return found, nil
}
func (h *CoordinationHandler) targetWorker(e *coordinationEvent, w *v1beta1.Worker) error {
	if !coordinationRuntimeSupported(w.Spec.Runtime) || w.Status.MatrixUserID == "" {
		return fmt.Errorf("coordination runtime adapter unavailable")
	}
	e.RecipientRuntime = w.Spec.Runtime
	e.Recipient = w.Status.MatrixUserID
	e.RecipientName = w.Name
	e.RecipientKind = "worker"
	e.TargetURL = h.workerURL(w.Spec.EffectiveWorkerName(w.Name), w.Spec.Env)
	return nil
}

func (h *CoordinationHandler) resolve(ctx context.Context, e coordinationEvent, caller *authpkg.CallerIdentity) (coordinationEvent, error) {
	if e.Kind == "assistant_digest" {
		return h.resolveAssistant(ctx, e, caller)
	}
	e.Protocol = ""   // Rebuild protocol authority from registered tasks, never caller input.
	e.Artifacts = nil // Final file authority is rebuilt from the completed Task snapshot.
	reject := func() (coordinationEvent, error) { return e, errCoordinationRejected }
	if caller == nil || (caller.Role != authpkg.RoleManager && caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleWorker) {
		return reject()
	}
	e.ActorName = caller.Username
	e.ActorRole = caller.Role
	var fence string
	if e.Kind == "handoff" {
		if len(e.Tasks) > 0 {
			e.Protocol = "task/v1"
		}
		if (caller.Role != authpkg.RoleManager && caller.Role != authpkg.RoleTeamLeader) || e.OriginEventID == "" || !strings.HasPrefix(coordinationString(e.SourceRoute, "target_session"), "!") || !strings.HasPrefix(coordinationString(e.SourceRoute, "target_user"), "@") {
			return reject()
		}
		if caller.Role == authpkg.RoleManager {
			actor, err := h.manager(ctx, caller.Username)
			if err != nil {
				return e, err
			}
			e.Actor, e.ActorName = actor.Status.MatrixUserID, actor.Name
		}
		recipient, err := h.worker(ctx, e.Recipient)
		if err != nil {
			return e, err
		}
		tm, err := h.memberTeam(ctx, recipient.Name, "team_leader", "")
		if err != nil {
			return e, err
		}
		e.Team = tm.Name
		e.StorageTeam = tm.Spec.EffectiveTeamName(tm.Name)
		if caller.Role == authpkg.RoleTeamLeader {
			route, err := NewAssistantHandler(h.deps).routeFor(ctx, caller, tm.Name)
			room, user := coordinationString(e.SourceRoute, "target_session"), coordinationString(e.SourceRoute, "target_user")
			if err != nil || caller.Team != tm.Name || route.name != recipient.Name || route.room != room || route.user != user || len(e.Tasks) > 1 {
				return reject()
			}
			e.Actor, e.ActorName = recipient.Status.MatrixUserID, recipient.Name
			original, status := h.taskSourceRequestFor(ctx, "worker", recipient.Name, room, e.OriginEventID, user)
			if status != 0 {
				return reject()
			}
			if e.RoomID != room {
				if len(e.Tasks) != 1 {
					return reject()
				}
				request := e.Tasks[0]
				main, err := h.read(ctx, path.Join("teams", e.StorageTeam, "shared/tasks", request.TaskID, "meta.json"))
				origin := coordinationMap(main, "task_origin")
				if err != nil || main["task_kind"] != "main" || main["room_id"] != e.RoomID || main["title"] != request.Title || main["spec"] != request.Spec || main["assigned_to"] != recipient.Status.MatrixUserID || origin["event_id"] != e.OriginEventID || origin["room_id"] != room || origin["requester"] != user || origin["dispatch_agent"] != recipient.Name {
					return reject()
				}
			}
			if len(e.Tasks) == 1 {
				if e.Tasks[0].TaskID != sourceMainTaskID(e.ActorName, e.StorageTeam, recipient.Name, e.OriginEventID) {
					return reject()
				}
				if !strings.Contains(e.Tasks[0].Spec, original) {
					return reject()
				}
			}
		}
		if !coordinationStorageTeamSafe(e.StorageTeam) {
			return reject()
		}
		e.EligibleAssignees = nil
		for _, member := range tm.Spec.WorkerMembers {
			w, err := h.worker(ctx, member.Name)
			if errors.Is(err, errCoordinationRejected) {
				continue
			}
			if err != nil {
				return e, err
			}
			if coordinationRuntimeSupported(w.Spec.Runtime) && w.Status.MatrixUserID != "" {
				e.EligibleAssignees = append(e.EligibleAssignees, w.Name)
			}
		}
		if e.RoomID == "" {
			e.RoomID = coordinationString(e.SourceRoute, "target_session")
		}
		if !strings.HasPrefix(e.RoomID, "!") {
			return reject()
		}
		if err = h.targetWorker(&e, recipient); err != nil {
			return e, err
		}
		fence = e.OriginEventID + "\x00" + e.Recipient
	} else {
		if caller.Role == authpkg.RoleManager || (e.ProjectID == "" && e.MainTaskID == "") || (e.ProjectID != "" && !coordinationSafeID.MatchString(e.ProjectID)) || (e.MainTaskID != "" && !coordinationSafeID.MatchString(e.MainTaskID)) || (e.TaskID != "" && !coordinationSafeID.MatchString(e.TaskID)) {
			return reject()
		}
		actor, err := h.worker(ctx, caller.Username)
		if err != nil {
			return e, err
		}
		e.ActorName = actor.Name
		e.Actor = actor.Status.MatrixUserID
		requiredRole := ""
		if caller.Role == authpkg.RoleTeamLeader {
			requiredRole = "team_leader"
		}
		tm, err := h.memberTeam(ctx, actor.Name, requiredRole, caller.Team)
		if err != nil {
			return e, err
		}
		if caller.Team == "" {
			return reject()
		}
		e.Team = tm.Name
		e.StorageTeam = tm.Spec.EffectiveTeamName(tm.Name)
		if !coordinationStorageTeamSafe(e.StorageTeam) {
			return reject()
		}
		project, err := h.read(ctx, coordinationPlanPath(e, "meta.json"))
		if err != nil {
			return e, err
		}
		identityMatches := coordinationString(project, "project_id") == e.ProjectID
		if e.MainTaskID != "" {
			identityMatches = coordinationString(project, "task_id") == e.MainTaskID && coordinationString(project, "task_kind") == "main"
			if e.ProjectID != "" && coordinationString(project, "project_id") != e.ProjectID {
				return reject()
			}
			e.ProjectID = coordinationString(project, "project_id")
		}
		if !identityMatches || (coordinationString(project, "team_id") != tm.Name && coordinationString(project, "team_id") != e.StorageTeam) {
			return reject()
		}
		if e.MainTaskID != "" && project["task_plan"] != nil {
			e.Protocol = "task/v1"
		}
		e.RoomID = coordinationString(project, "room_id")
		e.PlanType = coordinationString(project, "plan_type")
		e.DispatchMode = coordinationString(project, "dispatch_mode")
		e.RequesterRoute = coordinationMap(project, "requester_reply_route")
		var task map[string]any
		if e.TaskID != "" {
			task, err = h.read(ctx, path.Join("teams", e.StorageTeam, "shared/tasks", e.TaskID, "meta.json"))
			if err != nil {
				return e, err
			}
			if coordinationString(task, "task_id") != e.TaskID || coordinationString(task, "project_id") != e.ProjectID || coordinationString(task, "main_task_id") != e.MainTaskID {
				return reject()
			}
			if e.MainTaskID != "" {
				nodes, _ := project["tasks"].([]any)
				member := false
				for _, item := range nodes {
					n, _ := item.(map[string]any)
					if coordinationString(n, "task_id") == e.TaskID {
						member = true
						break
					}
				}
				if !member {
					return reject()
				}
			}
		}
		switch e.Kind {
		case "task_assigned":
			if e.DeliveryID == "" || coordinationString(task, "notifyTxn") != e.DeliveryID {
				return reject()
			}
			if caller.Role != authpkg.RoleTeamLeader {
				var notices map[string]taskNotificationIntent
				data, _ := json.Marshal(task["task_notifications"])
				_ = json.Unmarshal(data, &notices)
				intent := notices["assigned"]
				if caller.Role != authpkg.RoleWorker || e.Protocol != "task/v1" || coordinationString(task, "task_plan_id") == "" || intent.ActorRole != caller.Role || intent.ActorName != actor.Name || intent.Event.DeliveryID != e.DeliveryID || intent.Event.TaskID != e.TaskID {
					return reject()
				}
			}
			status := coordinationString(task, "status")
			if status == "" {
				return reject()
			}
			recipient, err := h.worker(ctx, coordinationString(task, "assigned_to"))
			if err != nil {
				return e, err
			}
			if _, err = h.memberTeam(ctx, recipient.Name, "", tm.Name); err != nil {
				return e, err
			}
			if err = h.targetWorker(&e, recipient); err != nil {
				return e, err
			}
			fence = e.TaskID + "\x00" + e.DeliveryID
		case "task_submitted":
			assignee := coordinationString(task, "assigned_to")
			if (assignee != actor.Name && assignee != actor.Spec.EffectiveWorkerName(actor.Name) && assignee != actor.Status.MatrixUserID) || e.SubmissionID == "" || coordinationString(task, "submission_id") != e.SubmissionID {
				return reject()
			}
			var recipient *v1beta1.Worker
			for _, m := range tm.Spec.WorkerMembers {
				if m.Role == "team_leader" {
					if recipient != nil {
						return reject()
					}
					recipient, err = h.worker(ctx, m.Name)
					if err != nil {
						return e, err
					}
				}
			}
			if recipient == nil {
				return reject()
			}
			if err = h.targetWorker(&e, recipient); err != nil {
				return e, err
			}
			fence = e.TaskID + "\x00" + e.SubmissionID

		case "project_ready", "main_task_ready":
			if !coordinationReady(project) || (e.Kind == "main_task_ready") != (e.MainTaskID != "") {
				return reject()
			}
			recipient := actor
			if caller.Role != authpkg.RoleTeamLeader {
				var notices map[string]taskNotificationIntent
				data, _ := json.Marshal(project["task_notifications"])
				_ = json.Unmarshal(data, &notices)
				intent := notices["ready"]
				if caller.Role != authpkg.RoleWorker || e.Protocol != "task/v1" || intent.ActorRole != caller.Role || intent.ActorName != actor.Name || intent.Event.Kind != e.Kind || intent.Event.MainTaskID != e.MainTaskID {
					return reject()
				}
				recipient, err = h.worker(ctx, coordinationString(coordinationMap(project, "task_plan"), "actor_name"))
				if err != nil {
					return e, err
				}
				leaderTeam, err := h.memberTeam(ctx, recipient.Name, "team_leader", tm.Name)
				if err != nil || leaderTeam.Name != tm.Name {
					return reject()
				}
			}
			if err = h.targetWorker(&e, recipient); err != nil {
				return e, err
			}
			context := []map[string]any{}
			nodes, _ := project["tasks"].([]any)
			parents := map[string]bool{}
			for _, item := range nodes {
				n, _ := item.(map[string]any)
				parents[coordinationString(n, "parent_task_id")] = true
			}
			for _, item := range nodes {
				node, _ := item.(map[string]any)
				id := coordinationString(node, "task_id")
				if !coordinationSafeID.MatchString(id) {
					return reject()
				}
				result, readErr := h.read(ctx, path.Join("teams", e.StorageTeam, "shared/tasks", id, "meta.json"))
				if readErr != nil {
					if parents[id] && errors.Is(readErr, os.ErrNotExist) {
						continue // Coordinating parents have no execution record.
					}
					return e, readErr
				}
				context = append(context, map[string]any{"task_id": id, "title": node["title"], "assigned_to": node["assigned_to"], "status": node["status"], "spec": coordinationClip(coordinationString(node, "spec"), 2048), "summary": coordinationClip(coordinationString(result, "summary"), 1024), "deliverables": result["deliverables"]})
			}
			details, _ := json.Marshal(map[string]any{"main_task_id": e.MainTaskID, "project_id": e.ProjectID, "title": project["title"], "accepted_results": context})
			e.Text = string(details)
			fence = coordinationScopeID(e)
		case "project_completed", "main_task_completed":
			e.OriginEventID = coordinationString(coordinationMap(project, "task_origin"), "event_id")
			if caller.Role != authpkg.RoleTeamLeader || coordinationString(project, "status") != "completed" || (e.Kind == "main_task_completed") != (e.MainTaskID != "") {
				return reject()
			}
			reportKey := coordinationPlanPath(e, "result.md")
			var reportDigest string
			if saved, exists := project["task_completion"]; exists {
				data, _ := json.Marshal(saved)
				var completion taskCompletion
				if json.Unmarshal(data, &completion) != nil || !validArtifactDigest(completion.ReportSHA256) {
					return reject()
				}
				e.Artifacts = completion.Artifacts
				reportDigest = completion.ReportSHA256
				reportKey = coordinationPlanPath(e, "artifacts/"+reportDigest)
			}
			report, readErr := h.deps.OSS.GetObject(ctx, reportKey)
			if reportDigest != "" {
				sum := sha256.Sum256(report)
				if readErr != nil || hex.EncodeToString(sum[:]) != reportDigest {
					return e, errors.New("saved final report unavailable or changed")
				}
			}
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return e, readErr
			}
			e.FinalReport = strings.TrimSpace(string(report))

			route := coordinationMap(project, "reply_route")
			target := coordinationString(route, "target_user")
			origin := coordinationMap(project, "task_origin")
			if e.Kind == "main_task_completed" && target == actor.Status.MatrixUserID &&
				coordinationString(origin, "dispatch_agent") == actor.Name && coordinationString(origin, "dispatch_agent_role") == authpkg.RoleTeamLeader {
				if err = h.targetWorker(&e, actor); err != nil {
					return e, err
				}
				fence = coordinationScopeID(e)
				break // Return the frozen report directly using this Leader's dispatch identity.
			}
			var managers v1beta1.ManagerList
			if err = h.deps.Client.List(ctx, &managers, client.InNamespace(h.deps.Namespace)); err != nil {
				return e, err
			}
			var recipient *v1beta1.Manager
			for i := range managers.Items {
				m := &managers.Items[i]
				if m.Status.MatrixUserID != "" && target == m.Status.MatrixUserID {
					if recipient != nil {
						return reject()
					}
					recipient = m
				}
			}
			if recipient == nil {
				e.External = true
				return e, nil
			}
			if !coordinationRuntimeSupported(recipient.Spec.Runtime) || !strings.HasPrefix(coordinationString(e.RequesterRoute, "target_session"), "!") {
				return reject()
			}
			e.RecipientRuntime = recipient.Spec.Runtime
			e.Recipient = recipient.Status.MatrixUserID
			e.RecipientKind = "manager"
			e.RecipientName = recipient.Name
			e.TargetURL = h.managerURL(recipient.Name)
			fence = coordinationScopeID(e)
		default:
			return reject()
		}
	}
	if e.Actor == "" {
		return reject()
	}
	if e.MainTaskID != "" {
		fence = e.StorageTeam + "\x00" + fence
	}
	digest := sha256.Sum256([]byte(e.Kind + "\x00" + e.Actor + "\x00" + fence))
	e.ID = hex.EncodeToString(digest[:16])
	return e, nil
}

func coordinationReady(project map[string]any) bool {
	active := "active"
	if coordinationString(project, "task_kind") == "main" {
		active = "in_progress"
	}
	status := coordinationString(project, "status")
	if (status != active && !(active == "in_progress" && status == "submitted")) || coordinationString(project, "plan_type") == "loop" {
		return false
	}
	nodes, _ := project["tasks"].([]any)
	parents := map[string]bool{}
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		if id := coordinationString(node, "parent_task_id"); id != "" {
			parents[id] = true
		}
	}
	leaves := 0
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		if status := coordinationString(node, "status"); status == "cancelled" || status == "blocked" || status == "revision" {
			return false
		}
		if parents[coordinationString(node, "task_id")] {
			continue
		}
		leaves++
		if coordinationString(node, "status") != "completed" {
			return false
		}
	}
	return leaves > 0
}
func coordinationClip(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return text
}

// Each managed Harness consumes authenticated Task notifications over Matrix.
func coordinationRuntimeSupported(runtime string) bool {
	switch runtime {
	case "qwenpaw", "openclaw", "hermes", "deepseek-harness", "openhuman", "codex", "opencode", "qoder", "claudecode":
		return true
	default:
		return false
	}
}
