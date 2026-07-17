package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	hiclawmetrics "github.com/hiclaw/hiclaw-controller/internal/metrics"
)

func (c *TuwunelClient) doJSON(ctx context.Context, method, path, token string, reqBody interface{}, respOut interface{}) (int, []byte, error) {
	operation := matrixOperation(method, path)
	start := time.Now()
	statusCode := 0
	var observeErr error
	defer func() {
		hiclawmetrics.ObserveUpstream("matrix", operation, start, statusCode, observeErr)
	}()

	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			observeErr = err
			return 0, nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	url := strings.TrimRight(c.config.ServerURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		observeErr = err
		return 0, nil, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		observeErr = err
		return 0, nil, err
	}
	defer resp.Body.Close()
	statusCode = resp.StatusCode

	respBody, _ := io.ReadAll(resp.Body)

	if respOut != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, respOut); err != nil {
			observeErr = fmt.Errorf("%w: %w", hiclawmetrics.ErrDecodeResponse, err)
			return resp.StatusCode, respBody, fmt.Errorf("decode response: %w (body: %s)", err, truncate(respBody, 200))
		}
	}

	return resp.StatusCode, respBody, nil
}

// doJSONAsAdmin performs the same request as doJSON but is used by call sites
// that authenticated with the cached admin token (via ensureAdminToken). It
// scopes token-clearing to genuine admin-token invalidation: on an HTTP 401
// whose body carries errcode M_UNKNOWN_TOKEN, the cached admin token is
// invalidated so the next ensureAdminToken re-logs in. Other 401/403 responses
// (e.g. a transient homeserver race, M_FORBIDDEN on a capability mismatch) do
// NOT clear the token — clearing on every 401/403 caused unbounded re-login
// growth and lost device sessions (Tier 1A finding #17).
func (c *TuwunelClient) doJSONAsAdmin(ctx context.Context, method, path, token string, reqBody interface{}, respOut interface{}) (int, []byte, error) {
	statusCode, respBody, err := c.doJSON(ctx, method, path, token, reqBody, respOut)
	if err != nil {
		// doJSON returns a non-nil err for a decode failure (respOut set but
		// body not JSON). On a 401 with a non-JSON body this skips the token
		// clear below; benign in practice because a genuine M_UNKNOWN_TOKEN
		// is JSON and decodes fine (callers' respOut structs carry ErrCode,
		// and json.Unmarshal ignores extra fields), so the clear fires there.
		return statusCode, respBody, err
	}
	if statusCode == http.StatusUnauthorized {
		var probe struct {
			ErrCode string `json:"errcode"`
		}
		_ = json.Unmarshal(respBody, &probe) // best-effort; body may not be JSON
		if probe.ErrCode == "M_UNKNOWN_TOKEN" {
			c.adminToken.Store("")
		}
	}
	return statusCode, respBody, nil
}
func matrixOperation(method, path string) string {
	pathOnly := path
	if idx := strings.IndexByte(pathOnly, '?'); idx >= 0 {
		pathOnly = pathOnly[:idx]
	}

	switch {
	case method == http.MethodPost && pathOnly == "/_matrix/client/v3/register":
		return "register_user"
	case method == http.MethodPost && pathOnly == "/_matrix/client/v3/login":
		return "login"
	case method == http.MethodPut && strings.Contains(pathOnly, "/profile/") && strings.HasSuffix(pathOnly, "/displayname"):
		return "set_display_name"
	case method == http.MethodPost && pathOnly == "/_matrix/client/v3/createRoom":
		return "create_room"
	case method == http.MethodGet && strings.HasPrefix(pathOnly, "/_matrix/client/v3/directory/room/"):
		return "resolve_room_alias"
	case method == http.MethodDelete && strings.HasPrefix(pathOnly, "/_matrix/client/v3/directory/room/"):
		return "delete_room_alias"
	case method == http.MethodPut && strings.Contains(pathOnly, "/state/m.room.name/"):
		return "set_room_name"
	case method == http.MethodPut && strings.Contains(pathOnly, "/state/"):
		return "set_room_state"
	case method == http.MethodPost && strings.HasSuffix(pathOnly, "/join"):
		return "join_room"
	case method == http.MethodPost && strings.HasSuffix(pathOnly, "/leave"):
		return "leave_room"
	case method == http.MethodPut && strings.Contains(pathOnly, "/send/m.room.message/"):
		return "send_message"
	case method == http.MethodGet && strings.HasSuffix(pathOnly, "/members"):
		return "list_room_members"
	case method == http.MethodPost && strings.HasSuffix(pathOnly, "/invite"):
		return "invite"
	case method == http.MethodPost && strings.HasSuffix(pathOnly, "/kick"):
		return "kick"
	case method == http.MethodGet && pathOnly == "/_matrix/client/v3/joined_rooms":
		return "list_joined_rooms"
	case method == http.MethodGet && pathOnly == "/_matrix/client/v3/sync":
		return "sync_messages"
	default:
		return "unknown"
	}
}

// encodeRoomID percent-encodes the "!" in room IDs for URL paths.
func encodeRoomID(roomID string) string {
	return strings.ReplaceAll(roomID, "!", "%21")
}

// roomAliasFullFor builds the full Matrix alias "#localpart:server" from a
// localpart. Exposed at package level so the service layer can synthesize
// the same alias format used by the client when calling ResolveRoomAlias /
// DeleteRoomAlias.
func roomAliasFullFor(domain, localpart string) string {
	return "#" + localpart + ":" + domain
}

// encodeAlias percent-encodes the "#" and ":" characters used by Matrix room
// aliases for safe inclusion in URL paths.
func encodeAlias(alias string) string {
	s := strings.ReplaceAll(alias, "#", "%23")
	s = strings.ReplaceAll(s, ":", "%3A")
	return s
}
func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// txnCounter provides unique transaction IDs for Matrix event sends.
var txnCounter atomic.Uint64
