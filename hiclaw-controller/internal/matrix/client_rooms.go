package matrix

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (c *TuwunelClient) CreateRoom(ctx context.Context, req CreateRoomRequest) (*RoomInfo, error) {
	token := req.CreatorToken
	tokenSource := "explicit"
	usedAdminToken := token == ""
	if token == "" {
		tokenSource = "admin"
		var err error
		token, err = c.ensureAdminToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("create room %q: %w", req.Name, err)
		}
	}

	body := map[string]interface{}{
		"name":      req.Name,
		"topic":     req.Topic,
		"invite":    req.Invite,
		"preset":    "trusted_private_chat",
		"is_direct": req.IsDirect,
	}

	if req.RoomAliasName != "" {
		body["room_alias_name"] = req.RoomAliasName
	}

	if len(req.PowerLevels) > 0 {
		body["power_level_content_override"] = map[string]interface{}{
			"users": req.PowerLevels,
		}
	}

	initialState := append([]StateEvent(nil), req.InitialState...)
	if req.E2EE {
		initialState = append(initialState, StateEvent{
			Type:     "m.room.encryption",
			StateKey: "",
			Content: map[string]interface{}{
				"algorithm": "m.megolm.v1.aes-sha2",
			},
		})
	}
	if len(initialState) > 0 {
		body["initial_state"] = initialState
	}

	var resp struct {
		RoomID  string `json:"room_id"`
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	var statusCode int
	var respBody []byte
	var err error
	if usedAdminToken {
		statusCode, respBody, err = c.doJSONAsAdmin(ctx, http.MethodPost,
			"/_matrix/client/v3/createRoom", token, body, &resp)
	} else {
		statusCode, respBody, err = c.doJSON(ctx, http.MethodPost,
			"/_matrix/client/v3/createRoom", token, body, &resp)
	}
	if err != nil {
		return nil, fmt.Errorf("create room %q: %w", req.Name, err)
	}

	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		if resp.RoomID == "" {
			return nil, fmt.Errorf("create room %q: empty room_id in response", req.Name)
		}
		return &RoomInfo{RoomID: resp.RoomID, Created: true}, nil
	}

	// Alias already claimed by a prior reconcile: resolve it and treat as
	// idempotent success. This is the sole path that turns informer-cache
	// lag / concurrent reconciles into a no-op instead of a duplicate room.
	if req.RoomAliasName != "" && resp.ErrCode == "M_ROOM_IN_USE" {
		alias := roomAliasFullFor(c.config.Domain, req.RoomAliasName)
		existingID, found, resolveErr := c.ResolveRoomAlias(ctx, alias)
		if resolveErr != nil {
			return nil, fmt.Errorf("create room %q: alias %s in use, resolve failed: %w",
				req.Name, alias, resolveErr)
		}
		if !found {
			return nil, fmt.Errorf("create room %q: alias %s reported in use but resolve returned not found",
				req.Name, alias)
		}
		return &RoomInfo{RoomID: existingID, Created: false}, nil
	}

	if statusCode == http.StatusForbidden || resp.ErrCode == "M_FORBIDDEN" {
		c.logCreateRoomFailureDiagnostics(ctx, req, token, tokenSource, statusCode, resp.ErrCode, resp.Error, respBody)
	}

	return nil, fmt.Errorf("create room %q: HTTP %d %s %s: %s",
		req.Name, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}
func (c *TuwunelClient) logCreateRoomFailureDiagnostics(ctx context.Context, req CreateRoomRequest, token, tokenSource string, statusCode int, errCode, errText string, respBody []byte) {
	senderUserID := ""
	senderPowerLevel := 0
	senderPowerLevelFound := false
	whoamiErr := ""
	if token != "" {
		if userID, err := c.accessTokenUserID(ctx, token); err != nil {
			whoamiErr = err.Error()
		} else {
			senderUserID = userID
			senderPowerLevel, senderPowerLevelFound = req.PowerLevels[userID]
		}
	}

	expectedAdminUserID := c.UserID(c.config.AdminUser)
	expectedAdminPowerLevel, expectedAdminPowerLevelFound := req.PowerLevels[expectedAdminUserID]

	log.FromContext(ctx).Info("Matrix createRoom rejected",
		"roomName", req.Name,
		"roomAliasName", req.RoomAliasName,
		"httpStatus", statusCode,
		"errcode", errCode,
		"error", errText,
		"response", truncate(respBody, 500),
		"tokenSource", tokenSource,
		"senderUserID", senderUserID,
		"senderWhoamiError", whoamiErr,
		"senderPowerLevel", senderPowerLevel,
		"senderPowerLevelFound", senderPowerLevelFound,
		"expectedAdminUserID", expectedAdminUserID,
		"expectedAdminPowerLevel", expectedAdminPowerLevel,
		"expectedAdminPowerLevelFound", expectedAdminPowerLevelFound,
		"powerLevels", req.PowerLevels,
		"invite", req.Invite)
}
func (c *TuwunelClient) ResolveRoomAlias(ctx context.Context, alias string) (string, bool, error) {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return "", false, fmt.Errorf("resolve alias %s: %w", alias, err)
	}

	var resp struct {
		RoomID  string `json:"room_id"`
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	statusCode, respBody, err := c.doJSONAsAdmin(ctx, http.MethodGet,
		"/_matrix/client/v3/directory/room/"+encodeAlias(alias),
		token, nil, &resp)
	if err != nil {
		return "", false, fmt.Errorf("resolve alias %s: %w", alias, err)
	}
	if statusCode == http.StatusOK {
		if resp.RoomID == "" {
			return "", false, fmt.Errorf("resolve alias %s: empty room_id in response", alias)
		}
		return resp.RoomID, true, nil
	}
	if statusCode == http.StatusNotFound || resp.ErrCode == "M_NOT_FOUND" {
		return "", false, nil
	}
	return "", false, fmt.Errorf("resolve alias %s: HTTP %d %s %s: %s",
		alias, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}

// DeleteRoomAlias implements Client.DeleteRoomAlias.
func (c *TuwunelClient) DeleteRoomAlias(ctx context.Context, alias string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("delete alias %s: %w", alias, err)
	}

	var resp struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	statusCode, respBody, err := c.doJSONAsAdmin(ctx, http.MethodDelete,
		"/_matrix/client/v3/directory/room/"+encodeAlias(alias),
		token, nil, &resp)
	if err != nil {
		return fmt.Errorf("delete alias %s: %w", alias, err)
	}
	if statusCode == http.StatusOK {
		return nil
	}
	if statusCode == http.StatusNotFound || resp.ErrCode == "M_NOT_FOUND" {
		return nil
	}
	return fmt.Errorf("delete alias %s: HTTP %d %s %s: %s",
		alias, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}
func (c *TuwunelClient) SetRoomName(ctx context.Context, roomID, name, userToken string) error {
	token := userToken
	if token == "" {
		var err error
		token, err = c.ensureAdminToken(ctx)
		if err != nil {
			return fmt.Errorf("set room name %s: %w", roomID, err)
		}
	}
	encodedRoom := encodeRoomID(roomID)
	body := map[string]string{"name": name}
	statusCode, respBody, err := c.doJSON(ctx, http.MethodPut,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.room.name/", encodedRoom),
		token, body, nil)
	if err != nil {
		return fmt.Errorf("set room name %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("set room name %s: HTTP %d: %s", roomID, statusCode, truncate(respBody, 500))
	}
	return nil
}
func (c *TuwunelClient) SetRoomState(ctx context.Context, roomID, eventType, stateKey string, content map[string]interface{}, userToken string) error {
	token := userToken
	if token == "" {
		var err error
		token, err = c.ensureAdminToken(ctx)
		if err != nil {
			return fmt.Errorf("set room state %s %s: %w", roomID, eventType, err)
		}
	}
	if content == nil {
		content = map[string]interface{}{}
	}
	encodedRoom := encodeRoomID(roomID)
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s",
		encodedRoom, url.PathEscape(eventType), url.PathEscape(stateKey))
	statusCode, respBody, err := c.doJSON(ctx, http.MethodPut, path, token, content, nil)
	if err != nil {
		return fmt.Errorf("set room state %s %s: %w", roomID, eventType, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("set room state %s %s: HTTP %d: %s",
			roomID, eventType, statusCode, truncate(respBody, 500))
	}
	return nil
}
func (c *TuwunelClient) JoinRoom(ctx context.Context, roomID, userToken string) error {
	encodedRoom := encodeRoomID(roomID)
	statusCode, respBody, err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", encodedRoom),
		userToken, map[string]interface{}{}, nil)
	if err != nil {
		return fmt.Errorf("join room %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("join room %s: HTTP %d: %s", roomID, statusCode, truncate(respBody, 500))
	}
	return nil
}
func (c *TuwunelClient) LeaveRoom(ctx context.Context, roomID, userToken string) error {
	token := userToken
	usedAdminToken := token == ""
	if usedAdminToken {
		var err error
		token, err = c.ensureAdminToken(ctx)
		if err != nil {
			return fmt.Errorf("leave room %s: %w", roomID, err)
		}
	}
	encodedRoom := encodeRoomID(roomID)
	var statusCode int
	var respBody []byte
	var err error
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/leave", encodedRoom)
	if usedAdminToken {
		statusCode, respBody, err = c.doJSONAsAdmin(ctx, http.MethodPost, path, token, map[string]interface{}{}, nil)
	} else {
		statusCode, respBody, err = c.doJSON(ctx, http.MethodPost, path, token, map[string]interface{}{}, nil)
	}
	if err != nil {
		return fmt.Errorf("leave room %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("leave room %s: HTTP %d: %s", roomID, statusCode, truncate(respBody, 500))
	}
	return nil
}
func (c *TuwunelClient) ListRoomMembers(ctx context.Context, roomID string) ([]RoomMember, error) {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("list members %s: %w", roomID, err)
	}
	return c.listRoomMembers(ctx, roomID, token, true)
}
func (c *TuwunelClient) ListRoomMembersWithToken(ctx context.Context, roomID, userToken string) ([]RoomMember, error) {
	return c.listRoomMembers(ctx, roomID, userToken, false)
}

// listRoomMembers is the shared implementation behind ListRoomMembers (admin
// token) and ListRoomMembersWithToken (caller-supplied token). usedAdminToken
// scopes the doJSONAsAdmin clear-on-failure behavior to the admin-token path.
func (c *TuwunelClient) listRoomMembers(ctx context.Context, roomID, userToken string, usedAdminToken bool) ([]RoomMember, error) {
	if userToken == "" {
		return nil, fmt.Errorf("list members %s: empty user token", roomID)
	}
	encodedRoom := encodeRoomID(roomID)

	var resp struct {
		Chunk []struct {
			StateKey string `json:"state_key"`
			Content  struct {
				Membership string `json:"membership"`
			} `json:"content"`
		} `json:"chunk"`
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/members", encodedRoom)
	var statusCode int
	var respBody []byte
	var err error
	if usedAdminToken {
		statusCode, respBody, err = c.doJSONAsAdmin(ctx, http.MethodGet, path, userToken, nil, &resp)
	} else {
		statusCode, respBody, err = c.doJSON(ctx, http.MethodGet, path, userToken, nil, &resp)
	}
	if err != nil {
		return nil, fmt.Errorf("list members %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("list members %s: HTTP %d %s %s: %s",
			roomID, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
	}

	members := make([]RoomMember, 0, len(resp.Chunk))
	for _, ev := range resp.Chunk {
		if ev.StateKey == "" {
			continue
		}
		if ev.Content.Membership != "join" && ev.Content.Membership != "invite" {
			continue
		}
		members = append(members, RoomMember{
			UserID:     ev.StateKey,
			Membership: ev.Content.Membership,
		})
	}
	return members, nil
}
func (c *TuwunelClient) InviteToRoom(ctx context.Context, roomID, userID string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("invite %s to %s: %w", userID, roomID, err)
	}
	return c.inviteToRoom(ctx, roomID, userID, token, true)
}
func (c *TuwunelClient) InviteToRoomWithToken(ctx context.Context, roomID, userID, inviterToken string) error {
	return c.inviteToRoom(ctx, roomID, userID, inviterToken, false)
}

// inviteToRoom is the shared implementation behind InviteToRoom (admin
// token) and InviteToRoomWithToken (caller-supplied token). usedAdminToken
// scopes the doJSONAsAdmin clear-on-failure behavior to the admin-token path.
func (c *TuwunelClient) inviteToRoom(ctx context.Context, roomID, userID, inviterToken string, usedAdminToken bool) error {
	if inviterToken == "" {
		return fmt.Errorf("invite %s to %s: empty inviter token", userID, roomID)
	}
	encodedRoom := encodeRoomID(roomID)

	var resp struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", encodedRoom)
	var statusCode int
	var respBody []byte
	var err error
	if usedAdminToken {
		statusCode, respBody, err = c.doJSONAsAdmin(ctx, http.MethodPost, path, inviterToken, map[string]string{"user_id": userID}, &resp)
	} else {
		statusCode, respBody, err = c.doJSON(ctx, http.MethodPost, path, inviterToken, map[string]string{"user_id": userID}, &resp)
	}
	if err != nil {
		return fmt.Errorf("invite %s to %s: %w", userID, roomID, err)
	}
	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		return nil
	}
	// Idempotent: user already in the room.
	if statusCode == http.StatusForbidden && resp.ErrCode == "M_FORBIDDEN" {
		lower := strings.ToLower(resp.Error)
		if strings.Contains(lower, "already in") || strings.Contains(lower, "already a member") {
			return nil
		}
	}
	return fmt.Errorf("invite %s to %s: HTTP %d %s %s: %s",
		userID, roomID, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}
func (c *TuwunelClient) KickFromRoom(ctx context.Context, roomID, userID, reason string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("kick %s from %s: %w", userID, roomID, err)
	}
	return c.kickFromRoom(ctx, roomID, userID, reason, token, true)
}
func (c *TuwunelClient) KickFromRoomWithToken(ctx context.Context, roomID, userID, reason, kickerToken string) error {
	return c.kickFromRoom(ctx, roomID, userID, reason, kickerToken, false)
}

// kickFromRoom is the shared implementation behind KickFromRoom (admin
// token) and KickFromRoomWithToken (caller-supplied token). usedAdminToken
// scopes the doJSONAsAdmin clear-on-failure behavior to the admin-token
// path; a benign 403 (M_FORBIDDEN, treated as idempotent success below)
// never carries M_UNKNOWN_TOKEN so it never triggers a clear.
func (c *TuwunelClient) kickFromRoom(ctx context.Context, roomID, userID, reason, kickerToken string, usedAdminToken bool) error {
	if kickerToken == "" {
		return fmt.Errorf("kick %s from %s: empty kicker token", userID, roomID)
	}
	encodedRoom := encodeRoomID(roomID)

	body := map[string]string{"user_id": userID}
	if reason != "" {
		body["reason"] = reason
	}

	var resp struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/kick", encodedRoom)
	var statusCode int
	var respBody []byte
	var err error
	if usedAdminToken {
		statusCode, respBody, err = c.doJSONAsAdmin(ctx, http.MethodPost, path, kickerToken, body, &resp)
	} else {
		statusCode, respBody, err = c.doJSON(ctx, http.MethodPost, path, kickerToken, body, &resp)
	}
	if err != nil {
		return fmt.Errorf("kick %s from %s: %w", userID, roomID, err)
	}
	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		return nil
	}
	// Idempotent: user not in the room (or already left).
	if statusCode == http.StatusNotFound {
		return nil
	}
	if statusCode == http.StatusForbidden && resp.ErrCode == "M_FORBIDDEN" {
		lower := strings.ToLower(resp.Error)
		if strings.Contains(lower, "not in") || strings.Contains(lower, "not a member") ||
			strings.Contains(lower, "cannot kick") {
			return nil
		}
	}
	return fmt.Errorf("kick %s from %s: HTTP %d %s %s: %s",
		userID, roomID, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}

// ListJoinedRooms returns the room IDs joined by the user identified by
// the given access token.
func (c *TuwunelClient) ListJoinedRooms(ctx context.Context, userToken string) ([]string, error) {
	var resp struct {
		JoinedRooms []string `json:"joined_rooms"`
	}
	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet,
		"/_matrix/client/v3/joined_rooms", userToken, nil, &resp)
	if err != nil {
		return nil, fmt.Errorf("list joined rooms: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("list joined rooms: HTTP %d: %s", statusCode, truncate(respBody, 500))
	}
	return resp.JoinedRooms, nil
}
