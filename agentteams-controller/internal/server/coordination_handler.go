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

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Coordination delivers control events; the project/task documents remain the
// only business state machine. Callers never supply an Agent network address.
type CoordinationHandler struct {
	deps         ServerDeps
	http         *http.Client
	workerURL    func(string, map[string]string) string
	managerURL   func(string) string
	projectEvent func(context.Context, coordinationEvent, string, string, string, string, string) (string, error)
}

type coordinationEvent struct {
	Kind              string         `json:"kind"`
	Actor             string         `json:"actor"`
	Recipient         string         `json:"recipient"`
	ProjectID         string         `json:"project_id,omitempty"`
	TaskID            string         `json:"task_id,omitempty"`
	DeliveryID        string         `json:"delivery_id,omitempty"`
	SubmissionID      string         `json:"submission_id,omitempty"`
	OriginEventID     string         `json:"origin_event_id,omitempty"`
	RoomID            string         `json:"room_id,omitempty"`
	Text              string         `json:"text"`
	SourceRoute       map[string]any `json:"source_route,omitempty"`
	RequesterRoute    map[string]any `json:"requester_route,omitempty"`
	ID                string         `json:"id"`
	Team              string         `json:"team"`
	StorageTeam       string         `json:"storage_team"`
	ActorName         string         `json:"actor_name"`
	ActorRole         string         `json:"actor_role"`
	RecipientName     string         `json:"recipient_name"`
	RecipientKind     string         `json:"recipient_kind"`
	TargetURL         string         `json:"-"`
	External          bool           `json:"external,omitempty"`
	EligibleAssignees []string       `json:"eligible_assignees,omitempty"`
	FinalReport       string         `json:"final_report,omitempty"`
	PlanType          string         `json:"-"`
	DispatchMode      string         `json:"-"`
}

var coordinationSafeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var errCoordinationRejected = errors.New("coordination event does not match current task ownership or execution identity")

func NewCoordinationHandler(deps ServerDeps) *CoordinationHandler {
	h := &CoordinationHandler{deps: deps, http: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	h.workerURL = func(name string, env map[string]string) string {
		return fmt.Sprintf("http://%s%s:%s", deps.ContainerPrefix, name, service.EffectiveWorkerConsolePort(env))
	}
	h.managerURL = func(name string) string {
		return fmt.Sprintf("http://%s:18799", authpkg.ResourcePrefix(deps.ResourcePrefix).ManagerPodName(name))
	}
	h.projectEvent = h.project
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
	if err := h.deps.Client.List(ctx, &list, client.InNamespace(h.deps.Namespace)); err != nil {
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
	if w.Spec.Runtime != "qwenpaw" || w.Status.MatrixUserID == "" {
		return fmt.Errorf("coordination runtime adapter unavailable")
	}
	e.Recipient = w.Status.MatrixUserID
	e.RecipientName = w.Name
	e.RecipientKind = "worker"
	e.TargetURL = h.workerURL(w.Spec.EffectiveWorkerName(w.Name), w.Spec.Env)
	return nil
}

func (h *CoordinationHandler) resolve(ctx context.Context, e coordinationEvent, caller *authpkg.CallerIdentity) (coordinationEvent, error) {
	reject := func() (coordinationEvent, error) { return e, errCoordinationRejected }
	if caller == nil || (caller.Role != authpkg.RoleManager && caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleWorker) {
		return reject()
	}
	e.ActorName = caller.Username
	e.ActorRole = caller.Role
	var fence string
	if e.Kind == "handoff" {
		if caller.Role != authpkg.RoleManager || e.OriginEventID == "" || !strings.HasPrefix(coordinationString(e.SourceRoute, "target_session"), "!") || !strings.HasPrefix(coordinationString(e.SourceRoute, "target_user"), "@") {
			return reject()
		}
		actor, err := h.manager(ctx, caller.Username)
		if err != nil {
			return e, err
		}
		e.Actor = actor.Status.MatrixUserID
		e.ActorName = actor.Name
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
		if !coordinationSafeID.MatchString(e.StorageTeam) {
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
			if w.Spec.Runtime == "qwenpaw" && w.Status.MatrixUserID != "" {
				e.EligibleAssignees = append(e.EligibleAssignees, w.Name)
			}
		}
		e.RoomID = coordinationString(e.SourceRoute, "target_session")
		if err = h.targetWorker(&e, recipient); err != nil {
			return e, err
		}
		fence = e.OriginEventID + "\x00" + e.Recipient
	} else {
		if caller.Role == authpkg.RoleManager || !coordinationSafeID.MatchString(e.ProjectID) || (e.TaskID != "" && !coordinationSafeID.MatchString(e.TaskID)) {
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
		if !coordinationSafeID.MatchString(e.StorageTeam) {
			return reject()
		}
		project, err := h.read(ctx, path.Join("teams", e.StorageTeam, "shared/projects", e.ProjectID, "meta.json"))
		if err != nil {
			return e, err
		}
		if coordinationString(project, "project_id") != e.ProjectID || (coordinationString(project, "team_id") != tm.Name && coordinationString(project, "team_id") != e.StorageTeam) {
			return reject()
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
			if coordinationString(task, "task_id") != e.TaskID || coordinationString(task, "project_id") != e.ProjectID {
				return reject()
			}
		}
		switch e.Kind {
		case "task_assigned":
			if caller.Role != authpkg.RoleTeamLeader || e.DeliveryID == "" || coordinationString(task, "notifyTxn") != e.DeliveryID {
				return reject()
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

		case "project_ready":
			if caller.Role != authpkg.RoleTeamLeader || !coordinationReady(project) {
				return reject()
			}
			if err = h.targetWorker(&e, actor); err != nil {
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
			details, _ := json.Marshal(map[string]any{"project_id": e.ProjectID, "title": project["title"], "accepted_results": context})
			e.Text = string(details)
			fence = e.ProjectID
		case "project_completed":
			if caller.Role != authpkg.RoleTeamLeader || coordinationString(project, "status") != "completed" {
				return reject()
			}
			report, readErr := h.deps.OSS.GetObject(ctx, path.Join("teams", e.StorageTeam, "shared/projects", e.ProjectID, "result.md"))
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return e, readErr
			}
			e.FinalReport = strings.TrimSpace(string(report))
			route := coordinationMap(project, "reply_route")
			target := coordinationString(route, "target_user")
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
			if recipient.Spec.Runtime != "qwenpaw" || !strings.HasPrefix(coordinationString(e.RequesterRoute, "target_session"), "!") {
				return reject()
			}
			e.Recipient = recipient.Status.MatrixUserID
			e.RecipientKind = "manager"
			e.RecipientName = recipient.Name
			e.TargetURL = h.managerURL(recipient.Name)
			fence = e.ProjectID
		default:
			return reject()
		}
	}
	if e.Actor == "" {
		return reject()
	}
	digest := sha256.Sum256([]byte(e.Kind + "\x00" + e.Actor + "\x00" + fence))
	e.ID = hex.EncodeToString(digest[:16])
	return e, nil
}

func coordinationReady(project map[string]any) bool {
	if coordinationString(project, "status") != "active" || coordinationString(project, "plan_type") == "loop" {
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
