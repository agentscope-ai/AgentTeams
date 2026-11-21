package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Role summaries are durable business context, not native Harness session files.
// The POC runner uses this API; runtimes cannot write the role prefix directly.
type AssistantHandler struct {
	deps ServerDeps
	mu   sync.Mutex
	send func(context.Context, string, string, string, string, map[string]any) (string, error)
}

type assistantRoute struct{ prefix, owner, room, user, name, kind, label string }
type assistantContext struct {
	EverySeconds  int    `json:"everySeconds"`
	Enabled       bool   `json:"enabled"`
	Summary       string `json:"summary"`
	LastRun       string `json:"lastRun,omitempty"`
	LastMessageID string `json:"lastMessageId,omitempty"`
}
type assistantReceipt struct {
	RequestID string `json:"requestId"`
	Text      string `json:"text"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Owner     string `json:"owner"`
	RoomID    string `json:"roomId"`
	MessageID string `json:"messageId,omitempty"`
	Status    string `json:"status"`
	SentAt    string `json:"sentAt,omitempty"`
}

func NewAssistantHandler(deps ServerDeps) *AssistantHandler {
	coord := NewCoordinationHandler(deps)
	return &AssistantHandler{deps: deps, send: func(ctx context.Context, kind, name, room, txn string, content map[string]any) (string, error) {
		if deps.Provisioner == nil || deps.MatrixClient == nil {
			return "", fmt.Errorf("assistant Matrix transport unavailable")
		}
		token, err := deps.Provisioner.AgentMatrixToken(ctx, kind, name)
		if err != nil {
			return "", err
		}
		if err := deps.MatrixClient.JoinRoom(ctx, room, token); err != nil {
			return "", fmt.Errorf("assistant join conversation: %w", err)
		}
		_, control := content["org.agentteams.coordination"]
		return coord.sendMatrixContent(ctx, kind, name, room, txn, content, !control)
	}}
}

func assistantHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func (h *AssistantHandler) route(r *http.Request) (assistantRoute, error) {
	return h.routeFor(r.Context(), authpkg.CallerFromContext(r.Context()), r.URL.Query().Get("team"))
}

func (h *AssistantHandler) routeFor(ctx context.Context, caller *authpkg.CallerIdentity, teamName string) (assistantRoute, error) {
	var out assistantRoute
	if caller == nil || h.deps.OSS == nil {
		return out, fmt.Errorf("assistant unavailable")
	}
	coord := NewCoordinationHandler(h.deps)
	var reader client.Reader = h.deps.Client
	if h.deps.APIReader != nil {
		reader = h.deps.APIReader
	}
	if teamName != "" {
		if caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleHuman {
			return out, fmt.Errorf("current Leader required")
		}
		var team v1.Team
		if err := reader.Get(ctx, types.NamespacedName{Namespace: h.deps.Namespace, Name: teamName}, &team); err != nil {
			return out, err
		}
		leader := ""
		for _, member := range team.Spec.WorkerMembers {
			if member.Role == "team_leader" {
				if leader != "" {
					return out, fmt.Errorf("ambiguous Leader")
				}
				leader = member.Name
			}
		}
		worker, err := coord.worker(ctx, leader)
		if err != nil {
			return out, err
		}
		teamAdmin := caller.Role == authpkg.RoleHuman && team.Spec.Admin != nil && (caller.Username == team.Spec.Admin.Name || caller.MatrixUserID != "" && caller.MatrixUserID == team.Spec.Admin.MatrixUserID)
		if caller.Role != authpkg.RoleAdmin && !teamAdmin && caller.Username != worker.Name {
			return out, fmt.Errorf("former Leader cannot access role context")
		}
		out = assistantRoute{prefix: path.Join("teams", team.Spec.EffectiveTeamName(team.Name), "roles/leader"), owner: string(worker.UID) + ":" + worker.Name, room: team.Status.LeaderDMRoomID, name: worker.Name, kind: "worker", label: team.Spec.EffectiveTeamName(team.Name) + "助手"}
		if h.deps.Backend != nil {
			instance, _, err := NewTaskRecoveryHandler(nil, h.deps).runningWorkerInstance(ctx, worker)
			if err != nil {
				return out, err
			}
			out.owner += ":" + instance
		}
		if team.Spec.Admin != nil {
			out.user = team.Spec.Admin.MatrixUserID
			if out.user == "" && h.deps.MatrixClient != nil {
				out.user = h.deps.MatrixClient.UserID(team.Spec.Admin.Name)
			}
		}
	} else {
		if caller.Role != authpkg.RoleManager {
			return out, fmt.Errorf("Manager required")
		}
		manager, err := coord.manager(ctx, caller.Username)
		if err != nil {
			return out, err
		}
		out = assistantRoute{prefix: path.Join("manager/roles", manager.Name), owner: fmt.Sprintf("%s:%s:%d", manager.UID, manager.Name, manager.Generation), room: manager.Status.RoomID, name: manager.Name, kind: "manager", label: "AT助手"}
		if h.deps.MatrixClient != nil {
			out.user = h.deps.MatrixClient.UserID(h.deps.MatrixConfig.AdminUser)
		}
		// Additional L1 conversations require a verified recipient-room registry.
		// Never accept an arbitrary client-supplied room or user's identity.
	}
	if out.room == "" || out.user == "" {
		return out, fmt.Errorf("assistant recipient room unavailable")
	}
	out.prefix = path.Join(out.prefix, "users", assistantHash(out.user))
	return out, nil
}

func (h *AssistantHandler) read(ctx context.Context, key string, value any) error {
	b, err := h.deps.OSS.GetObject(ctx, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

func (h *AssistantHandler) Context(w http.ResponseWriter, r *http.Request) {
	route, err := h.route(r)
	if err != nil {
		httputil.WriteError(w, 403, err.Error())
		return
	}
	if r.Method == http.MethodPut {
		h.mu.Lock()
		defer h.mu.Unlock()
	}
	state := assistantContext{Enabled: true, EverySeconds: 86400}
	err = h.read(r.Context(), path.Join(route.prefix, "context.json"), &state)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		httputil.WriteError(w, 503, "role context unavailable")
		return
	}
	if r.Method == http.MethodPut {
		var input struct {
			EverySeconds int    `json:"everySeconds"`
			Owner        string `json:"owner"`
			Summary      string `json:"summary"`
			Enabled      *bool  `json:"enabled"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input) != nil {
			httputil.WriteError(w, 400, "invalid context")
			return
		}
		if input.Owner != route.owner {
			httputil.WriteError(w, 409, "role execution changed; reload context")
			return
		}
		state.Summary = input.Summary
		current, err := h.route(r)
		if err != nil || current.owner != route.owner || current.user != route.user {
			httputil.WriteError(w, 409, "role changed before context update")
			return
		}
		if input.EverySeconds != 0 {
			if input.EverySeconds < 30 {
				httputil.WriteError(w, 400, "minimum digest interval is 30 seconds")
				return
			}
			state.EverySeconds = input.EverySeconds
		}
		if input.Enabled != nil {
			state.Enabled = *input.Enabled
		}
		b, _ := json.Marshal(state)
		if h.deps.OSS.PutObject(r.Context(), path.Join(route.prefix, "context.json"), b) != nil {
			httputil.WriteError(w, 503, "context save failed")
			return
		}
	}
	view := map[string]any{"owner": route.owner, "roomId": route.room, "recipient": route.user, "assistant": route.label, "context": state}
	if r.Method == http.MethodGet && h.deps.Provisioner != nil && h.deps.MatrixConfig.ServerURL != "" {
		token, err := h.deps.Provisioner.AgentMatrixToken(r.Context(), route.kind, route.name)
		if err != nil {
			httputil.WriteError(w, 503, "assistant conversation unavailable")
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), "GET", strings.TrimRight(h.deps.MatrixConfig.ServerURL, "/")+"/_matrix/client/v3/rooms/"+url.PathEscape(route.room)+"/messages?dir=b&limit=12", nil)
		if err != nil {
			httputil.WriteError(w, 503, "assistant conversation unavailable")
			return
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := (&http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
		if err != nil {
			httputil.WriteError(w, 503, "assistant conversation unavailable")
			return
		}
		defer response.Body.Close()
		var history struct {
			Chunk []struct {
				Type    string         `json:"type"`
				Sender  string         `json:"sender"`
				Content map[string]any `json:"content"`
			} `json:"chunk"`
		}
		if response.StatusCode != 200 || json.NewDecoder(http.MaxBytesReader(w, response.Body, 256<<10)).Decode(&history) != nil {
			httputil.WriteError(w, 503, "assistant conversation unavailable")
			return
		}
		messages := []map[string]string{}
		for i := len(history.Chunk) - 1; i >= 0; i-- {
			event := history.Chunk[i]
			body, _ := event.Content["body"].(string)
			if event.Type == "m.room.message" && body != "" && event.Content["org.agentteams.coordination"] == nil {
				text := []rune(body)
				if len(text) > 2000 {
					text = text[:2000]
				}
				messages = append(messages, map[string]string{"sender": event.Sender, "text": string(text)})
			}
		}
		current, err := h.route(r)
		if err != nil || current.owner != route.owner || current.user != route.user || current.room != route.room {
			httputil.WriteError(w, 409, "assistant role changed while reading conversation")
			return
		}
		view["recentMessages"] = messages
	}
	httputil.WriteJSON(w, 200, view)
}

func (h *AssistantHandler) Send(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleManager {
		httputil.WriteError(w, 403, "current assistant Agent required")
		return
	}
	route, err := h.route(r)
	if err != nil {
		httputil.WriteError(w, 403, err.Error())
		return
	}
	var input assistantReceipt
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input) != nil || !coordinationSafeID.MatchString(input.RequestID) || strings.TrimSpace(input.Text) == "" {
		httputil.WriteError(w, 400, "requestId and text required")
		return
	}
	if input.Owner != route.owner {
		httputil.WriteError(w, 409, "role execution changed; reload context")
		return
	}
	// Serialize a role's context update and receipt reconciliation in this POC.
	// Storage reservation plus deterministic Matrix txn also fence concurrent retries.
	h.mu.Lock()
	defer h.mu.Unlock()
	ctx := r.Context()
	stateKey := path.Join(route.prefix, "context.json")
	state := assistantContext{Enabled: true, EverySeconds: 86400}
	if err = h.read(ctx, stateKey, &state); err != nil && !errors.Is(err, os.ErrNotExist) {
		httputil.WriteError(w, 503, "context unavailable")
		return
	}
	if !state.Enabled {
		httputil.WriteError(w, 409, "daily digest disabled")
		return
	}
	key := path.Join(route.prefix, "receipts", input.RequestID+".json")
	receipt := input
	receipt.Status, receipt.RoomID, receipt.MessageID, receipt.SentAt = "pending", route.room, "", ""
	receipt.Title = route.label
	b, _ := json.Marshal(receipt)
	err = h.deps.OSS.PutObjectIfAbsent(ctx, key, b)
	if errors.Is(err, oss.ErrPreconditionFailed) {
		if err = h.read(ctx, key, &receipt); err == nil && (receipt.Text != input.Text || receipt.Summary != input.Summary) {
			httputil.WriteError(w, 409, "requestId already has different content")
			return
		}
	}
	if err != nil {
		httputil.WriteError(w, 503, "receipt reservation unavailable")
		return
	}
	if receipt.MessageID == "" {
		receipt.Owner = route.owner
		// Recheck ownership immediately before the external side effect.
		current, err := h.route(r)
		if err != nil || current.owner != route.owner || current.room != route.room || current.user != route.user {
			httputil.WriteError(w, 409, "assistant role changed before delivery")
			return
		}
		id, err := h.send(ctx, route.kind, route.name, route.room, "assistant-"+assistantHash(key), map[string]any{
			"msgtype": "m.text", "body": input.Text,
			"org.agentteams.assistant_reminder": map[string]any{"requestId": input.RequestID, "title": route.label, "recipient": route.user, "kind": "daily_digest"},
		})
		if err != nil {
			httputil.WriteError(w, 503, "delivery pending; retry unchanged requestId")
			return
		}
		receipt.MessageID, receipt.Status, receipt.SentAt = id, "sent", time.Now().UTC().Format(time.RFC3339)
		b, _ = json.Marshal(receipt)
		if h.deps.OSS.PutObject(ctx, key, b) != nil {
			httputil.WriteError(w, 503, "receipt save pending; retry unchanged requestId")
			return
		}
	}
	// A retry after a process crash repairs summary/checkpoint without resending.
	if state.LastRun <= receipt.SentAt {
		state.Summary, state.LastRun, state.LastMessageID = receipt.Summary, receipt.SentAt, receipt.MessageID
	}
	b, _ = json.Marshal(state)
	if h.deps.OSS.PutObject(ctx, stateKey, b) != nil {
		httputil.WriteError(w, 503, "context reconciliation pending; retry unchanged requestId")
		return
	}
	httputil.WriteJSON(w, 200, receipt)
}

func (h *AssistantHandler) Receipt(w http.ResponseWriter, r *http.Request) {
	route, err := h.route(r)
	if err != nil {
		httputil.WriteError(w, 403, err.Error())
		return
	}
	id := r.PathValue("id")
	if !coordinationSafeID.MatchString(id) {
		httputil.WriteError(w, 400, "invalid receipt id")
		return
	}
	var receipt assistantReceipt
	if err := h.read(r.Context(), path.Join(route.prefix, "receipts", id+".json"), &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httputil.WriteError(w, 404, "receipt not found")
		} else {
			httputil.WriteError(w, 503, "receipt unavailable")
		}
		return
	}
	httputil.WriteJSON(w, 200, receipt)
}
