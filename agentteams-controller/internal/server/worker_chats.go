// The worker chats proxy exposes a read-only view of a worker's conversations
// (its qwenpaw chats) for the dashboard and other API clients. It is the
// same thin, byte-transparent pattern as the worker checkpoint proxy: the
// controller resolves the worker, enforces the worker-scoped read boundary
// (W8: 404, not 403, so worker existence cannot be probed) plus the
// participation boundary (L2 humans see the worker's conversations in the
// Matrix rooms they are current members of — server-side, resolved with the
// caller's own Matrix token via GET /joined_rooms; no privilege escalation),
// and forwards to the worker's own qwenpaw app over loopback.
//
// The detail endpoint returns the worker's saved agent context converted
// to messages — presented as "agent context", not a guaranteed complete
// room transcript (context may be compacted; the Matrix room's exchanged
// messages remain authoritative in Matrix). See docs/design/
// worker-chats-api.md.
//
// Embedded mode only: the proxy targets the per-worker console app, which
// the Kubernetes deployment model does not expose per worker, so the route
// 503s uniformly there (same rationale as the checkpoint proxy).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// chatProxyTimeout bounds each upstream call to the worker's qwenpaw
	// app (same bound as the checkpoint proxy).
	chatProxyTimeout = 5 * time.Second

	// chatErrorBodyCap limits how much of an upstream 5xx body is surfaced
	// in the wrapped 502 response.
	chatErrorBodyCap = 128
)

// chatIDPattern matches qwenpaw chat ids — every chat is created with
// str(uuid4()) (lowercase hex and hyphens), the same family as
// workerNamePattern. It also blocks path separators, query characters, and
// other input that could be injected into the upstream URL.
var chatIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// chatListQueryWhitelist is the documented read-only filter set of the
// upstream list endpoint. Unknown parameters are rejected with 400 rather
// than forwarded (the checkpoint proxy's query discipline: never 422).
// include_app_owned is a 2.2.1+ filter; older runtimes silently ignore the
// unknown parameter, so forwarding it is harmless across versions.
var chatListQueryWhitelist = map[string]bool{
	"user_id":           true,
	"channel":           true,
	"archived":          true,
	"include_app_owned": true,
}

// ChatsHandler proxies the worker's read-only chat (session) endpoints.
type ChatsHandler struct {
	client          client.Client
	namespace       string
	kubeMode        string
	http            *http.Client
	containerPrefix string
	workerBaseURL   func(name string, env map[string]string) string
	// joinedRooms resolves the rooms the calling Matrix user is in, using
	// the caller's OWN access token (the L2 participation anchor, room
	// level). Wired from matrix.Client.ListJoinedRooms; nil (e.g.
	// deploys without a Matrix client) makes every L2 request fail
	// closed.
	joinedRooms func(ctx context.Context, userToken string) ([]string, error)
}

// NewChatsHandler builds a chats proxy over the given K8s client.
// containerPrefix must be the effective prefix from controller
// configuration (see config.ContainerPrefix).
func NewChatsHandler(c client.Client, namespace, kubeMode, containerPrefix string) *ChatsHandler {
	h := &ChatsHandler{
		client:          c,
		namespace:       namespace,
		kubeMode:        kubeMode,
		http:            &http.Client{Timeout: chatProxyTimeout},
		containerPrefix: containerPrefix,
	}
	h.workerBaseURL = h.defaultWorkerBaseURL
	return h
}

// defaultWorkerBaseURL resolves a worker's qwenpaw app base URL from the
// effective container prefix and the effective console port. The port goes
// through service.EffectiveWorkerConsolePort — the same system-wins env
// chain used at container creation — so the proxy can never target a port
// the container does not listen on.
func (h *ChatsHandler) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", h.containerPrefix, name, port)
}

// chatRoute describes one forwarded route.
type chatRoute struct {
	// upstream is the qwenpaw path to forward (already validated by the
	// caller — worker name and chat id are pattern-checked before the
	// dial, so neither can inject path segments).
	upstream string
	// detail marks the per-chat routes (detail/status). For
	// participation-scoped callers (L2 humans) they require a server-side
	// participation precheck before the dial; the list route is instead
	// filtered to the caller's own conversations.
	detail bool
}

// callerRoomSet resolves the set of Matrix room IDs the calling user is a
// current member of — the L2 participation anchor, at ROOM level (the
// decision the AgentTeams workflow implies: a human's work with a team
// happens in the team's Matrix rooms, where the real conversations —
// manager/leader agents delegating, workers reporting — take place;
// sender-level "my own @-chats only" was too narrow for that, because in
// the AgentTeams work pattern a human's own @-messages are rare and the
// diagnostic evidence is the agent-driven sessions in the same rooms).
//
// Resolution uses the caller's OWN access token (already validated by the
// authenticator's whoami) against the homeserver's GET
// /_matrix/client/v3/joined_rooms: membership is a property of the token's
// user, so no privilege escalation, no stored membership table that could
// drift, and no Human CR dependency (the reconciled status.matrixUserID
// is no longer consulted on this path).
//
// Why room level covers both matrix group-session modes
// (share_session_in_group, AgentTeams decision #7001, 2026-09-05): the
// matrix channel keys every chat by room — session_id is "matrix:{room_id}"
// in both modes; only the per-chat user_id differs (the sender's MXID when
// sessions are isolated per sender, the room ID itself in shared mode).
// So "rooms you are in" admits exactly the conversations a participant may
// see: isolated mode exposes each (room, sender) session of the caller's
// rooms (including the agent-driven ones), shared mode exposes the single
// room session to every current member. DMs (two-member rooms) stay
// private to their participants — room level is the literal "who is in the
// room" check, no broader. Non-matrix channels (qq/console/cron, app-owned
// chats, subagent sessions) have no "!"-prefixed room namespace, so matrixRoomID
// yields "" and an L2 human never sees them (fail closed, no per-channel
// allowlist).
//
// Returns (nil, nil) for full-view callers (admin/manager SAs, team
// leaders), (set, nil) for an L2 human, or an error when the anchor cannot
// be established (no token, homeserver failure, no Matrix source) — the
// caller fails closed (uniform 404), because an unprovable participation
// anchor must not leak a conversation view.
func (h *ChatsHandler) callerRoomSet(r *http.Request) (map[string]bool, error) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || caller.Role != authpkg.RoleHuman {
		return nil, nil
	}
	// Defense in depth: L3 (worker-scoped) humans carry no teams, so they
	// already 404 at the team-scope check. If the scope check ever gains a
	// worker leg, L3 must still get no participation anchor — this surface
	// stays L1/L2 for now (#1277 deliberately keeps the other read
	// surfaces team-scoped; L3 chats access is a follow-up decision).
	if caller.IsWorkerScoped() {
		return nil, fmt.Errorf("worker-scoped callers have no chats access")
	}
	if h.joinedRooms == nil {
		return nil, fmt.Errorf("no matrix source for room membership")
	}
	// Same header the authenticator extracted and validated (the context
	// carries the identity, not the token — the raw token is re-read from
	// the request, mirroring internal/auth's extractBearerToken).
	token := requestBearerToken(r)
	if token == "" {
		return nil, fmt.Errorf("l2 caller request carries no matrix bearer token")
	}
	rooms, err := h.joinedRooms(r.Context(), token)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(rooms))
	for _, roomID := range rooms {
		set[roomID] = true
	}
	return set, nil
}

// requestBearerToken mirrors internal/auth's extractBearerToken (which is
// unexported): the middleware already validated this exact token, so
// re-reading the header is not a second authentication.
func requestBearerToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == authHeader {
		return ""
	}
	return token
}

// matrixRoomID extracts the Matrix room ID from a qwenpaw chat session_id.
// The matrix channel keys every chat as "matrix:{room_id}" — identically in
// both share_session_in_group modes (channel.resolve_session_id derives the
// session id from the room before the per-sender split); Matrix room ids
// are opaque ids starting with "!". Anything else — console/qq/cron
// session ids, app-owned chats, subagent session ids — yields "" and is
// never visible to an L2 human (fail closed).
func matrixRoomID(sessionID string) string {
	const prefix = "matrix:"
	if !strings.HasPrefix(sessionID, prefix) {
		return ""
	}
	roomID := strings.TrimPrefix(sessionID, prefix)
	if !strings.HasPrefix(roomID, "!") {
		return ""
	}
	return roomID
}

// proxy performs the shared request pipeline for all three routes:
// worker-name validation, the kube-mode gate, worker resolution, the
// worker-scoped read boundary (W8), the participation boundary (L2
// humans: server-side, own conversations only), the upstream dial, and
// the status mapping.
func (h *ChatsHandler) proxy(w http.ResponseWriter, r *http.Request, name string, route chatRoute) {
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	// Kube-mode check runs before any worker lookup: the endpoints are
	// entirely unavailable in kube mode, and a uniform 503 (rather than a
	// per-worker 404 vs 503 split) avoids leaking worker existence.
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker session inspection requires embedded mode")
		return
	}

	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "get worker chats", err)
		return
	}
	// Resolve the owning team for the scoped-caller check (same chain as
	// ResourceHandler.GetWorker: standalone workers hide as 404). Note:
	// findTeamMember's second return value is the member (worker) name,
	// not the team name — the check must compare against the Team CR name.
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker chats", err)
		return
	}
	teamName := ""
	if teamObj != nil {
		teamName = teamObj.Name
	}
	// Scoped callers (team leaders / L2 humans) may only inspect workers in
	// the teams they control — mirrors GET /api/v1/workers/{name} (W8: 404,
	// not 403, so worker existence cannot be probed).
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
		!caller.TeamMatches(teamName) {
		httputil.WriteError(w, http.StatusNotFound, "worker not found")
		return
	}

	// Participation boundary (L2 humans only): a team-scoped human sees
	// the worker's conversations in the rooms the human is a current
	// member of — not the worker's conversations in other rooms or DMs,
	// and never non-matrix-channel chats. The filter is resolved
	// server-side (the caller's own Matrix token → joined rooms); the
	// client never supplies it.
	roomSet, perr := h.callerRoomSet(r)
	if perr != nil {
		httputil.WriteError(w, http.StatusNotFound, "worker not found")
		return
	}

	// The container identity is the effective runtime name (spec.workerName
	// when set, the CR name otherwise) — dialing by the CR name would miss
	// the container for imported/renamed workers. The authorization checks
	// above deliberately stay on the original resource/team identity.
	base := h.workerBaseURL(worker.Spec.EffectiveWorkerName(worker.Name), worker.Spec.Env)
	upstreamPath := route.upstream
	if roomSet != nil {
		if route.detail {
			if !h.participatesInRoomChat(w, r, base, roomSet, r.PathValue("chat_id")) {
				return
			}
		} else {
			// list: room-scoped, server-filtered — fetch the worker's
			// matrix chat list and return only the items whose session
			// resolves to one of the caller's rooms (byte-transparent
			// per item). Client-supplied filters are dropped: channel is
			// forced to matrix (L2 visibility is matrix-room-scoped),
			// user_id has no room-level meaning, and include_app_owned
			// would widen into app-owned namespaces.
			items, _, uerr := h.anchoredChats(r, base, roomSet)
			if uerr != nil {
				httputil.WriteError(w, http.StatusBadGateway, uerr.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if err := json.NewEncoder(w).Encode(items); err != nil {
				// headers already sent — nothing left to do.
				return
			}
			return
		}
	} else if r.URL.RawQuery != "" {
		// Full-view callers: forward the (whitelist-validated) query,
		// encoded in the same canonical (sorted) form as before.
		if encoded := r.URL.Query().Encode(); encoded != "" {
			upstreamPath += "?" + encoded
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+upstreamPath, nil)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}

	resp, err := h.http.Do(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker unreachable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Chat histories can be large (full message transcripts); stream
		// verbatim like the checkpoint proxy rather than capping the body.
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// Version-agnostic gate (the skills proxy's pattern): the upstream's
		// own 4xx is passed through verbatim, so callers can distinguish
		// "chat not found" from "this worker build predates the route"
		// (GET /chats/{id}/status exists only on QwenPaw 2.2.1+; on older
		// builds the upstream's 404 is returned as-is and clients hide the
		// status indicator).
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Upstream 5xx — wrap it; the raw body is surfaced truncated.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, chatErrorBodyCap))
	httputil.WriteError(w, http.StatusBadGateway, "worker upstream error: "+strings.TrimSpace(string(body)))
}

// anchoredChats fetches the worker's matrix chat list (GET
// /api/chats?channel=matrix) and keeps only the items whose session_id
// resolves to a room the caller is a current member of. Returns the
// surviving items byte-transparent (raw upstream JSON) plus a set of
// their ids for the detail/status precheck. A non-nil *upstreamListErr
// maps to the same 502 shapes as the transparent dial (worker down vs
// upstream 5xx vs malformed list): participation that cannot be proven is
// never a 404.
func (h *ChatsHandler) anchoredChats(r *http.Request, base string, roomSet map[string]bool) ([]json.RawMessage, map[string]bool, *upstreamListErr) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/api/chats?channel=matrix", nil)
	if err != nil {
		return nil, nil, &upstreamListErr{msg: "failed to build upstream request"}
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, nil, &upstreamListErr{msg: "worker unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, chatErrorBodyCap))
		return nil, nil, &upstreamListErr{msg: "worker upstream error: " + strings.TrimSpace(string(body))}
	}
	var items []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, nil, &upstreamListErr{msg: "worker upstream error: malformed chat list"}
	}
	// Non-nil: an empty room set must encode as [] (the dashboard's
	// array contract), never null.
	kept := []json.RawMessage{}
	ids := make(map[string]bool, len(items))
	for _, item := range items {
		var probe struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			continue
		}
		if roomSet[matrixRoomID(probe.SessionID)] {
			kept = append(kept, item)
			ids[probe.ID] = true
		}
	}
	return kept, ids, nil
}

// upstreamListErr is a fetch failure of the room-scoped list, carrying the
// 502 body verbatim (distinguishable dial failure vs upstream 5xx).
type upstreamListErr struct{ msg string }

func (e *upstreamListErr) Error() string { return e.msg }

// participatesInRoomChat is the L2 participation precheck for the detail
// and status routes: the worker's matrix chat list filtered to the
// caller's rooms must contain the chat id. Three outcomes:
//   - present in the list: true, the dial proceeds.
//   - absent: a uniform 404 in the upstream's own not-found shape
//     ("Chat not found: {id}", the 2.2.1 FastAPI detail) so a denied
//     request is indistinguishable from a genuinely missing chat —
//     neither other rooms' chat existence nor content can be probed.
//   - upstream failure / malformed list: 502. Participation cannot be
//     proven, and a false 404 would silently hide a healthy worker.
//
// One extra loopback hop per L2 detail/status request; full-view callers
// never pay it.
func (h *ChatsHandler) participatesInRoomChat(w http.ResponseWriter, r *http.Request, base string, roomSet map[string]bool, chatID string) bool {
	_, ids, uerr := h.anchoredChats(r, base, roomSet)
	if uerr != nil {
		httputil.WriteError(w, http.StatusBadGateway, uerr.Error())
		return false
	}
	if ids[chatID] {
		return true
	}
	// chatID is pattern-validated ([a-z0-9-]) before this point, so it
	// cannot inject into the JSON body.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, `{"detail":"Chat not found: %s"}`, chatID)
	return false
}

// listChats handles GET /api/v1/workers/{name}/chats.
// Forwards the whitelisted read-only filters to GET /api/chats. L2
// humans: the response is room-scoped server-side — only chats whose
// session resolves to a room the caller is a current member of are
// returned (participation boundary; client-supplied filters are dropped).
func (h *ChatsHandler) listChats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key := range q {
		if !chatListQueryWhitelist[key] {
			httputil.WriteError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
			return
		}
	}
	h.proxy(w, r, r.PathValue("name"), chatRoute{upstream: "/api/chats"})
}

// getChat handles GET /api/v1/workers/{name}/chats/{chat_id}.
// Forwards to GET /api/chats/{chat_id} (the agent context as a message
// view — see the design doc: this is the saved agent context converted
// to messages, not a guaranteed complete room transcript). L2 humans:
// participation precheck first.
func (h *ChatsHandler) getChat(w http.ResponseWriter, r *http.Request) {
	chatID := r.PathValue("chat_id")
	if !chatIDPattern.MatchString(chatID) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid chat id")
		return
	}
	if r.URL.RawQuery != "" {
		httputil.WriteError(w, http.StatusBadRequest, "unsupported query parameter")
		return
	}
	h.proxy(w, r, r.PathValue("name"), chatRoute{upstream: "/api/chats/" + chatID, detail: true})
}

// getChatStatus handles GET /api/v1/workers/{name}/chats/{chat_id}/status.
// Forwards to GET /api/chats/{chat_id}/status (QwenPaw 2.2.1+; older
// builds return their own 404, passed through verbatim). L2 humans:
// participation precheck first.
func (h *ChatsHandler) getChatStatus(w http.ResponseWriter, r *http.Request) {
	chatID := r.PathValue("chat_id")
	if !chatIDPattern.MatchString(chatID) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid chat id")
		return
	}
	if r.URL.RawQuery != "" {
		httputil.WriteError(w, http.StatusBadRequest, "unsupported query parameter")
		return
	}
	h.proxy(w, r, r.PathValue("name"), chatRoute{upstream: "/api/chats/" + chatID + "/status", detail: true})
}
