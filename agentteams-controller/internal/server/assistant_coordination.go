package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (h *AssistantHandler) contextState(ctx context.Context, route assistantRoute) (assistantContext, error) {
	state := assistantContext{Enabled: true, EverySeconds: 86400}
	err := h.read(ctx, path.Join(route.prefix, "context.json"), &state)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return state, err
}

func assistantRequestID(state assistantContext) string {
	return "after-" + assistantHash(state.LastMessageID)
}

// Use the existing Controller outbox and Teams carrier. No Harness timer,
// native chat endpoint or native memory file participates in scheduling.
func (h *CoordinationHandler) scheduleAssistants(ctx context.Context) {
	if h.deps.OSS == nil || h.assistant == nil {
		return
	}
	reader := client.Reader(h.deps.Client)
	if h.deps.APIReader != nil {
		reader = h.deps.APIReader
	}
	queue := func(caller *authpkg.CallerIdentity, team, user string) {
		route, err := h.assistant.routeForRecipient(ctx, caller, team, "", user)
		if err != nil {
			return // Role or conversation is not ready yet.
		}
		state, err := h.assistant.contextState(ctx, route)
		if err == nil && state.Enabled {
			if state.LastRun != "" {
				last, parseErr := time.Parse(time.RFC3339, state.LastRun)
				if parseErr != nil || time.Since(last) < time.Duration(state.EverySeconds)*time.Second {
					return
				}
			}
			_, err = h.enqueue(ctx, coordinationEvent{Kind: "assistant_digest", DeliveryID: route.owner,
				OriginEventID: assistantRequestID(state), SourceRoute: map[string]any{"team": team, "user": user}}, caller)
		}
		if err != nil {
			log.FromContext(ctx).Error(err, "assistant schedule remains pending", "team", team, "role", caller.Role)
		}
	}
	var teams v1.TeamList
	if err := reader.List(ctx, &teams, client.InNamespace(h.deps.Namespace)); err != nil {
		return
	}
	for _, team := range teams.Items {
		if team.Status.LeaderDMRoomID == "" || team.Spec.Admin == nil {
			continue
		}
		for _, member := range team.Spec.WorkerMembers {
			if member.Role == "team_leader" {
				queue(&authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: member.Name, Team: team.Name}, team.Name, "")
			}
		}
	}
	var managers v1.ManagerList
	var humans v1.HumanList
	if reader.List(ctx, &humans, client.InNamespace(h.deps.Namespace)) != nil {
		return
	}
	if reader.List(ctx, &managers, client.InNamespace(h.deps.Namespace)) == nil {
		for _, manager := range managers.Items {
			if manager.Status.RoomID != "" && manager.Status.Phase == "Running" {
				for _, human := range humans.Items {
					if human.DeletionTimestamp.IsZero() && human.Spec.PermissionLevel == 1 && human.Status.MatrixUserID != "" {
						queue(&authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: manager.Name}, "", human.Name)
					}
				}
			}
		}
	}
}

func (h *CoordinationHandler) resolveAssistant(ctx context.Context, e coordinationEvent, caller *authpkg.CallerIdentity) (coordinationEvent, error) {
	if h.assistant == nil || caller == nil || caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleManager {
		return e, errCoordinationRejected
	}
	team := coordinationString(e.SourceRoute, "team")
	route, err := h.assistant.routeForRecipient(ctx, caller, team, coordinationString(e.SourceRoute, "manager"), coordinationString(e.SourceRoute, "user"))
	if err != nil {
		return e, errCoordinationRejected
	}
	state, err := h.assistant.contextState(ctx, route)
	if err != nil {
		return e, err
	}
	if !state.Enabled || e.DeliveryID != route.owner || !coordinationSafeID.MatchString(e.OriginEventID) {
		return e, errCoordinationRejected
	}
	var receipt assistantReceipt
	err = h.assistant.read(ctx, path.Join(route.prefix, "receipts", e.OriginEventID+".json"), &receipt)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return e, err
	}
	if e.OriginEventID != assistantRequestID(state) && receipt.MessageID == "" {
		return e, errCoordinationRejected
	}
	if receipt.MessageID == "" && state.LastRun != "" {
		last, parseErr := time.Parse(time.RFC3339, state.LastRun)
		if parseErr != nil || time.Since(last) < time.Duration(state.EverySeconds)*time.Second {
			return e, errCoordinationRejected
		}
	}
	if route.kind == "manager" {
		manager, err := h.manager(ctx, route.name)
		if err != nil || !coordinationRuntimeSupported(manager.Spec.Runtime) {
			return e, errCoordinationRejected
		}
		e.Recipient, e.RecipientRuntime = manager.Status.MatrixUserID, manager.Spec.Runtime
	} else {
		worker, err := h.worker(ctx, route.name)
		if err != nil || h.targetWorker(&e, worker) != nil {
			return e, errCoordinationRejected
		}
	}
	if e.Recipient == "" {
		return e, errCoordinationRejected
	}
	e.Protocol, e.Actor, e.ActorName, e.ActorRole = "assistant/v1", e.Recipient, route.name, caller.Role
	e.RecipientName, e.RecipientKind, e.RoomID, e.Team = route.name, route.kind, route.room, team
	e.ID = assistantHash(route.prefix + "\x00" + route.owner + "\x00" + e.OriginEventID)
	e.ProjectID, e.MainTaskID, e.TaskID, e.SubmissionID = "", "", "", ""
	e.Tasks, e.Artifacts, e.RequesterRoute = nil, nil, nil
	e.SourceRoute = map[string]any{"team": team}
	scope := ""
	if team != "" {
		scope = " --team " + team
	} else {
		e.SourceRoute["manager"], e.SourceRoute["user"] = route.name, route.human
		scope = " --manager " + route.name + " --user " + route.human
	}
	e.Text = fmt.Sprintf("ASSISTANT_DIGEST_DUE\nYou are handling an AgentTeams scheduled assistant reminder. Read `agt assistant context%s` for the Controller-owned private context, shared businessSummary and recent conversation. Use only this user's authorized team facts and sources you can verify; distinguish verified news dates from uncertain information and never invent facts when retrieval fails. Page older user messages with `agt assistant conversation --before <end>` using the same scope when recent context is incomplete; preserve the last processed event reference in private notes. Read `agt assistant receipt %s%s` first; keep pending text, summary, requestId and contextRevision unchanged, but use the current owner below. Otherwise collect relevant information, then save a JSON file with owner=%q, requestId=%q, contextRevision (the revision read before inference), text (the user-facing reminder with source dates/links), and summary (updated private continuity notes). Submit it with `agt assistant send%s --file <file>`. Maintain shared business facts separately with `agt assistant business` and `agt assistant remember-business`, using the same scope and exact owner/revision; never copy personal interests or private chat into shared business. Treat the returned Matrix messageId as sent, never as read. Do not create a Task or Project, delegate, change task state, use private Harness memory/cron, or send a second chat reply. End after the public send receipt. If ownership changed or the schedule is disabled, stop.", scope, e.OriginEventID, scope, route.owner, e.OriginEventID, scope)
	return e, nil
}

func (h *CoordinationHandler) advanceAssistant(ctx context.Context, d *coordinationDelivery) {
	e, err := h.resolveAssistant(ctx, d.Event, &authpkg.CallerIdentity{Role: d.Event.ActorRole, Username: d.Event.ActorName, Team: d.Event.Team})
	if errors.Is(err, errCoordinationRejected) || err == nil && e.ID != d.Event.ID {
		d.Status, d.Error = "ignored", "assistant ownership or schedule changed"
		return
	}
	if err != nil {
		d.Error = "assistant context unavailable"
		return
	}
	route, err := h.assistant.routeForRecipient(ctx, &authpkg.CallerIdentity{Role: e.ActorRole, Username: e.ActorName}, e.Team, coordinationString(e.SourceRoute, "manager"), coordinationString(e.SourceRoute, "user"))
	if err != nil {
		d.Error = "assistant route unavailable"
		return
	}
	var receipt assistantReceipt
	err = h.assistant.read(ctx, path.Join(route.prefix, "receipts", e.OriginEventID+".json"), &receipt)
	if err == nil && receipt.MessageID != "" {
		d.Status, d.Error, d.UserEventID = "completed", "", receipt.MessageID
		return
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		d.Error = "assistant delivery receipt unavailable"
		return
	}
	if d.NativeID != "" {
		return // Wait for the public send receipt, never infer success from model text.
	}
	id, err := h.assistant.send(ctx, e.RecipientKind, e.RecipientName, e.RoomID, e.ID, coordinationMatrixNotification(e, nil))
	if err != nil {
		d.Error = "Teams assistant trigger remains pending"
		return
	}
	d.NativeID, d.MatrixEventID, d.Status, d.Error = "matrix:"+id, id, "running", ""
}
