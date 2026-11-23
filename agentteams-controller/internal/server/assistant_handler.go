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
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// Context and send facts belong to a stable role/user workspace. Harnesses
// consume the public protocol; no native session file is business authority.
type AssistantHandler struct {
	deps ServerDeps
	send func(context.Context, string, string, string, string, map[string]any) (string, error)
}

type assistantContext struct {
	EverySeconds  int    `json:"everySeconds"`
	Enabled       bool   `json:"enabled"`
	Summary       string `json:"summary"`
	LastRun       string `json:"lastRun,omitempty"`
	LastMessageID string `json:"lastMessageId,omitempty"`
}

type assistantReceipt struct {
	RequestID       string    `json:"requestId"`
	Text            string    `json:"text"`
	Title           string    `json:"title"`
	Summary         string    `json:"summary"`
	Owner           string    `json:"owner"`
	ContextRevision string    `json:"contextRevision,omitempty"`
	RoomID          string    `json:"roomId"`
	MessageID       string    `json:"messageId,omitempty"`
	Status          string    `json:"status"`
	SentAt          string    `json:"sentAt,omitempty"`
	Sender          string    `json:"sender,omitempty"`
	LeaseUntil      time.Time `json:"leaseUntil,omitempty"`
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
		if err = deps.MatrixClient.JoinRoom(ctx, room, token); err != nil {
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
	state := assistantContext{Enabled: true, EverySeconds: 86400}
	key := path.Join(route.prefix, "context.json")
	revision, err := h.load(r.Context(), key, &state)
	if err != nil {
		httputil.WriteError(w, 503, "role context unavailable")
		return
	}
	if r.Method == http.MethodPut {
		var input struct {
			Owner        string  `json:"owner"`
			Revision     *string `json:"revision"`
			Summary      *string `json:"summary"`
			EverySeconds int     `json:"everySeconds"`
			Enabled      *bool   `json:"enabled"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input) != nil {
			httputil.WriteError(w, 400, "invalid context")
			return
		}
		if input.Owner != route.owner || input.Revision == nil || *input.Revision != revision {
			httputil.WriteError(w, 409, "context or execution changed; reload context")
			return
		}
		if input.Summary != nil {
			state.Summary = *input.Summary
		}
		if input.EverySeconds != 0 {
			if input.EverySeconds < 30 || input.EverySeconds > 365*86400 {
				httputil.WriteError(w, 400, "digest interval must be between 30 seconds and one year")
				return
			}
			state.EverySeconds = input.EverySeconds
		}
		if input.Enabled != nil {
			state.Enabled = *input.Enabled
		}
		if !h.sameRoute(r, route) {
			httputil.WriteError(w, 409, "assistant role changed")
			return
		}
		revision, err = h.save(r.Context(), key, state, revision)
		if err != nil {
			h.storageError(w, err)
			return
		}
	}
	var business struct {
		Summary string `json:"summary"`
	}
	if _, err = h.load(r.Context(), path.Join(route.rolePrefix, "business.json"), &business); err != nil {
		httputil.WriteError(w, 503, "business context unavailable")
		return
	}
	view := map[string]any{"owner": route.owner, "revision": revision, "roomId": route.room, "recipient": route.user, "assistant": route.label, "context": state, "businessSummary": business.Summary}
	if r.Method == http.MethodGet && h.deps.Provisioner != nil && h.deps.MatrixConfig.ServerURL != "" {
		messages, err := h.history(r.Context(), route, 12)
		if err != nil {
			httputil.WriteError(w, 503, "assistant conversation unavailable")
			return
		}
		recent := []map[string]string{}
		for i := len(messages) - 1; i >= 0; i-- {
			e := messages[i]
			body, _ := e.Content["body"].(string)
			if e.Type != "m.room.message" || body == "" || e.Content["org.agentteams.coordination"] != nil {
				continue
			}
			text := []rune(body)
			if len(text) > 2000 {
				text = text[:2000]
			}
			recent = append(recent, map[string]string{"eventId": e.ID, "sender": e.Sender, "text": string(text)})
		}
		view["recentMessages"] = recent
	}
	if !h.sameRoute(r, route) {
		httputil.WriteError(w, 409, "assistant role changed while reading conversation")
		return
	}
	httputil.WriteJSON(w, 200, view)
}

func (h *AssistantHandler) Send(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || (caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleManager) {
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
	receipt, err := h.deliver(r, route, input)
	if err != nil {
		h.storageError(w, err)
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
	if err = h.read(r.Context(), path.Join(route.prefix, "receipts", id+".json"), &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httputil.WriteError(w, 404, "receipt not found")
		} else {
			httputil.WriteError(w, 503, "receipt unavailable")
		}
		return
	}
	httputil.WriteJSON(w, 200, receipt)
}

func (h *AssistantHandler) sameRoute(r *http.Request, route assistantRoute) bool {
	current, err := h.route(r)
	return err == nil && current.owner == route.owner && current.room == route.room && current.prefix == route.prefix
}

func (h *AssistantHandler) storageError(w http.ResponseWriter, err error) {
	if errors.Is(err, oss.ErrPreconditionFailed) {
		httputil.WriteError(w, 409, err.Error())
	} else {
		httputil.WriteError(w, 503, "assistant operation pending: "+err.Error())
	}
}

type assistantMessage struct {
	ID      string         `json:"event_id"`
	Type    string         `json:"type"`
	Sender  string         `json:"sender"`
	Content map[string]any `json:"content"`
}

func (h *AssistantHandler) history(ctx context.Context, route assistantRoute, limit int) ([]assistantMessage, error) {
	page, err := h.historyPage(ctx, route, limit, "")
	return page.Chunk, err
}

type assistantHistoryPage struct {
	Chunk []assistantMessage `json:"chunk"`
	End   string             `json:"end,omitempty"`
}

func (h *AssistantHandler) Conversation(w http.ResponseWriter, r *http.Request) {
	route, err := h.route(r)
	if err != nil {
		httputil.WriteError(w, 403, err.Error())
		return
	}
	page, err := h.historyPage(r.Context(), route, 50, r.URL.Query().Get("before"))
	if err != nil {
		httputil.WriteError(w, 503, "assistant conversation unavailable")
		return
	}
	if !h.sameRoute(r, route) {
		httputil.WriteError(w, 409, "assistant role changed while reading history")
		return
	}
	httputil.WriteJSON(w, 200, page)
}

func (h *AssistantHandler) historyPage(ctx context.Context, route assistantRoute, limit int, before string) (assistantHistoryPage, error) {
	var history assistantHistoryPage
	if h.deps.Provisioner == nil {
		return history, fmt.Errorf("assistant history unavailable")
	}
	token, err := h.deps.Provisioner.AgentMatrixToken(ctx, route.kind, route.name)
	if err != nil {
		return history, err
	}
	request, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(h.deps.MatrixConfig.ServerURL, "/")+"/_matrix/client/v3/rooms/"+url.PathEscape(route.room)+fmt.Sprintf("/messages?dir=b&limit=%d", limit)+"&from="+url.QueryEscape(before), nil)
	if err != nil {
		return history, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return history, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || json.NewDecoder(http.MaxBytesReader(nil, response.Body, 512<<10)).Decode(&history) != nil {
		return history, fmt.Errorf("assistant history unavailable")
	}
	return history, nil
}
