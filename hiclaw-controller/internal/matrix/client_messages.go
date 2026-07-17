package matrix

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

func (c *TuwunelClient) SendMessage(ctx context.Context, roomID, token, body string) error {
	return c.sendMessage(ctx, roomID, token, body, false)
}

// sendMessage is the shared implementation behind SendMessage and the
// admin-token call sites (SendMessageAsAdmin, AdminCommand). usedAdminToken
// scopes the doJSONAsAdmin clear-on-failure behavior to genuine admin-token
// callers so a per-user SendMessage failure never evicts the cached admin
// token.
func (c *TuwunelClient) sendMessage(ctx context.Context, roomID, token, body string, usedAdminToken bool) error {
	encodedRoom := encodeRoomID(roomID)
	txnID := fmt.Sprintf("hc-%d", txnCounter.Add(1))
	msg := map[string]string{
		"msgtype": "m.text",
		"body":    body,
	}

	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s", encodedRoom, txnID)
	var statusCode int
	var respBody []byte
	var err error
	if usedAdminToken {
		statusCode, respBody, err = c.doJSONAsAdmin(ctx, http.MethodPut, path, token, msg, nil)
	} else {
		statusCode, respBody, err = c.doJSON(ctx, http.MethodPut, path, token, msg, nil)
	}
	if err != nil {
		return fmt.Errorf("send message to %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("send message to %s: HTTP %d: %s", roomID, statusCode, truncate(respBody, 500))
	}
	return nil
}

// ensureAdminRoomID resolves the Tuwunel admin room via the well-known
// alias "#admins:<domain>" and caches the result for the lifetime of the
// client. Controller restart re-resolves.
func (c *TuwunelClient) ensureAdminRoomID(ctx context.Context) (string, error) {
	if r, ok := c.adminRoomID.Load().(string); ok && r != "" {
		return r, nil
	}
	alias := fmt.Sprintf("#admins:%s", c.config.Domain)
	path := "/_matrix/client/v3/directory/room/" + url.PathEscape(alias)

	var resp struct {
		RoomID string `json:"room_id"`
	}
	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet, path, "", nil, &resp)
	if err != nil {
		return "", fmt.Errorf("resolve admin room alias %s: %w", alias, err)
	}
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("resolve admin room alias %s: HTTP %d: %s", alias, statusCode, truncate(respBody, 500))
	}
	if resp.RoomID == "" {
		return "", fmt.Errorf("resolve admin room alias %s: empty room_id", alias)
	}
	c.adminRoomID.Store(resp.RoomID)
	return resp.RoomID, nil
}

// SendMessageAsAdmin sends body to roomID using the cached admin token.
// Errors from token acquisition and message send are wrapped to identify
// the failing stage. Used by the controller for system-level prompts that
// must originate from the admin identity (e.g. Manager onboarding welcome).
func (c *TuwunelClient) SendMessageAsAdmin(ctx context.Context, roomID, body string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("send admin message: %w", err)
	}
	if err := c.sendMessage(ctx, roomID, token, body, true); err != nil {
		return fmt.Errorf("send admin message: %w", err)
	}
	return nil
}

// AdminCommand sends a command message to the Tuwunel admin bot room as
// the admin user. The bot parses messages starting with "!admin" in the
// admin room. Processing is asynchronous; this call is fire-and-forget.
func (c *TuwunelClient) AdminCommand(ctx context.Context, command string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("admin command: %w", err)
	}
	roomID, err := c.ensureAdminRoomID(ctx)
	if err != nil {
		return fmt.Errorf("admin command: %w", err)
	}
	if err := c.sendMessage(ctx, roomID, token, command, true); err != nil {
		return fmt.Errorf("admin command: %w", err)
	}
	return nil
}
func (c *TuwunelClient) SyncMessages(ctx context.Context, since string, timeout time.Duration) (*SyncMessagesResult, error) {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("timeout", fmt.Sprintf("%d", timeout.Milliseconds()))
	if since != "" {
		q.Set("since", since)
	}
	path := "/_matrix/client/v3/sync?" + q.Encode()

	var resp struct {
		NextBatch string `json:"next_batch"`
		Rooms     struct {
			Join map[string]struct {
				Timeline struct {
					Events []struct {
						Type    string `json:"type"`
						EventID string `json:"event_id"`
						Sender  string `json:"sender"`
						Content struct {
							Mentions struct {
								UserIDs []string `json:"user_ids"`
							} `json:"m.mentions"`
						} `json:"content"`
					} `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet, path, token, nil, &resp)
	if err != nil {
		return nil, fmt.Errorf("sync messages: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("sync messages: HTTP %d: %s", statusCode, truncate(respBody, 500))
	}
	out := &SyncMessagesResult{NextBatch: resp.NextBatch}
	for roomID, room := range resp.Rooms.Join {
		for _, event := range room.Timeline.Events {
			if event.Type != "m.room.message" || len(event.Content.Mentions.UserIDs) == 0 {
				continue
			}
			out.Events = append(out.Events, MessageEvent{
				RoomID:   roomID,
				EventID:  event.EventID,
				Sender:   event.Sender,
				Mentions: event.Content.Mentions.UserIDs,
			})
		}
	}
	return out, nil
}

// doJSON performs an HTTP request with JSON body/response.
// Returns the HTTP status code, the raw response body, and any transport/decode error.
// If respOut is nil, the response body is not decoded (but still read and returned).
// The raw body is always returned (possibly nil) so callers can include it in
// diagnostic error messages even when respOut is set.
//
// Note: this does not know whether the caller's token was the cached admin
// token, so it never clears c.adminToken itself. Call sites that authenticate
// with the admin token use doJSONAsAdmin, which scopes the clear-on-failure
// behavior to genuine admin-token invalidation.
