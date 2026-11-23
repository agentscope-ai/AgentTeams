package server

import (
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

func assistantConflict(message string) error {
	return fmt.Errorf("%s: %w", message, oss.ErrPreconditionFailed)
}

func (h *AssistantHandler) deliver(r *http.Request, route assistantRoute, input assistantReceipt) (assistantReceipt, error) {
	ctx := r.Context()
	key := path.Join(route.prefix, "receipts", input.RequestID+".json")
	var receipt assistantReceipt
	revision, err := h.load(ctx, key, &receipt)
	if err != nil {
		return receipt, err
	}
	if revision == "" {
		receipt = assistantReceipt{RequestID: input.RequestID, Text: input.Text, Summary: input.Summary, ContextRevision: input.ContextRevision,
			Owner: route.owner, RoomID: route.room, Title: route.label, Sender: route.agent, Status: "prepared"}
		revision, err = h.save(ctx, key, receipt, "")
		if err != nil {
			return receipt, err
		}
	}
	if receipt.Text != input.Text || receipt.Summary != input.Summary || receipt.ContextRevision != input.ContextRevision || receipt.RoomID != route.room {
		return receipt, assistantConflict("requestId already has different content or room")
	}
	if receipt.MessageID != "" {
		return receipt, h.reconcileContext(r, route, receipt)
	}
	state, err := h.contextState(ctx, route)
	if err != nil {
		return receipt, err
	}
	if !state.Enabled || !h.sameRoute(r, route) {
		return receipt, assistantConflict("assistant ownership or schedule changed")
	}
	if receipt.LeaseUntil.After(time.Now()) {
		return receipt, assistantConflict("delivery is already in progress; retry the same requestId later")
	}
	if receipt.Owner != route.owner && receipt.Status != "prepared" {
		// Matrix transaction deduplication is sender-scoped. Switching sender
		// after an unknown outcome would silently send a second reminder.
		messages, err := h.history(ctx, route, 100)
		if err != nil {
			return receipt, assistantConflict("previous executor delivery is unconfirmed; inspect the stored receipt")
		}
		for _, event := range messages {
			marker, _ := event.Content["org.agentteams.assistant_reminder"].(map[string]any)
			if event.Type == "m.room.message" && event.Sender == receipt.Sender && event.ID != "" &&
				coordinationString(marker, "requestId") == receipt.RequestID && coordinationString(marker, "recipient") == route.user && coordinationString(event.Content, "body") == receipt.Text {
				receipt.MessageID, receipt.Status = event.ID, "sent"
				receipt.SentAt = time.Now().UTC().Format(time.RFC3339Nano)
				if _, err = h.save(ctx, key, receipt, revision); err != nil {
					return receipt, err
				}
				return receipt, h.reconcileContext(r, route, receipt)
			}
		}
		return receipt, assistantConflict("previous executor delivery outcome unknown; automatic resend is blocked")
	}
	receipt.Owner, receipt.Sender, receipt.Status = route.owner, route.agent, "sending"
	receipt.LeaseUntil = time.Now().UTC().Add(30 * time.Second)
	revision, err = h.save(ctx, key, receipt, revision)
	if err != nil {
		return receipt, err
	}
	if !h.sameRoute(r, route) {
		return receipt, assistantConflict("assistant changed before delivery")
	}
	state, err = h.contextState(ctx, route)
	if err != nil {
		return receipt, err
	}
	if !state.Enabled {
		return receipt, assistantConflict("assistant disabled before delivery")
	}
	id, err := h.send(ctx, route.kind, route.name, route.room, "assistant-"+assistantHash(key), map[string]any{
		"msgtype": "m.text", "body": receipt.Text,
		"org.agentteams.assistant_reminder": map[string]any{"requestId": receipt.RequestID, "title": route.label, "recipient": route.user, "kind": "daily_digest"},
	})
	if err != nil {
		return receipt, fmt.Errorf("delivery unconfirmed; retry the unchanged requestId after its lease expires: %w", err)
	}
	if id == "" {
		return receipt, fmt.Errorf("Matrix returned no message receipt")
	}
	receipt.MessageID, receipt.Status, receipt.SentAt = id, "sent", time.Now().UTC().Format(time.RFC3339Nano)
	receipt.LeaseUntil = time.Time{}
	if _, err = h.save(ctx, key, receipt, revision); err != nil {
		return receipt, err
	}
	return receipt, h.reconcileContext(r, route, receipt)
}
