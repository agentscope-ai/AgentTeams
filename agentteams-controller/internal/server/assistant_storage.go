package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// Both initialization and updates require a storage-level precondition. An
// in-process mutex cannot protect context from another Controller replica.
func (h *AssistantHandler) load(ctx context.Context, key string, value any) (string, error) {
	b, err := h.deps.OSS.GetObject(ctx, key)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err = json.Unmarshal(b, value); err != nil {
		return "", err
	}
	return contentETag(b), nil
}

func (h *AssistantHandler) save(ctx context.Context, key string, value any, revision string) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if revision == "" {
		err = h.deps.OSS.PutObjectIfAbsent(ctx, key, b)
	} else {
		strict, ok := h.deps.OSS.(conditionalWriter)
		if !ok {
			return "", oss.ErrConditionalUnsupported
		}
		err = strict.PutObjectIfMatchStrict(ctx, key, b, revision)
	}
	if err != nil {
		return "", err
	}
	return contentETag(b), nil
}

// Business is deliberately a separate operation/document from private notes:
// changing the Team Admin never copies personal interests into shared history.
func (h *AssistantHandler) Business(w http.ResponseWriter, r *http.Request) {
	route, err := h.route(r)
	if err != nil {
		httputil.WriteError(w, 403, err.Error())
		return
	}
	var state struct {
		Summary string `json:"summary"`
	}
	key := path.Join(route.rolePrefix, "business.json")
	revision, err := h.load(r.Context(), key, &state)
	if err != nil {
		httputil.WriteError(w, 503, "business context unavailable")
		return
	}
	if r.Method == http.MethodPut {
		var input struct {
			Owner    string  `json:"owner"`
			Revision *string `json:"revision"`
			Summary  string  `json:"summary"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input) != nil {
			httputil.WriteError(w, 400, "invalid business context")
			return
		}
		if input.Owner != route.owner || input.Revision == nil || *input.Revision != revision || !h.sameRoute(r, route) {
			httputil.WriteError(w, 409, "business context or execution changed; reload context")
			return
		}
		state.Summary = input.Summary
		revision, err = h.save(r.Context(), key, state, revision)
		if err != nil {
			h.storageError(w, err)
			return
		}
	}
	httputil.WriteJSON(w, 200, map[string]any{"owner": route.owner, "revision": revision, "summary": state.Summary})
}

func (h *AssistantHandler) reconcileContext(r *http.Request, route assistantRoute, receipt assistantReceipt) error {
	for range 5 {
		state := assistantContext{Enabled: true, EverySeconds: 86400}
		key := path.Join(route.prefix, "context.json")
		revision, err := h.load(r.Context(), key, &state)
		if err != nil {
			return err
		}
		if !h.sameRoute(r, route) {
			return fmt.Errorf("role changed before context reconciliation: %w", oss.ErrPreconditionFailed)
		}
		last, _ := time.Parse(time.RFC3339Nano, state.LastRun)
		sent, err := time.Parse(time.RFC3339Nano, receipt.SentAt)
		if err != nil {
			return fmt.Errorf("invalid delivery timestamp")
		}
		if last.After(sent) {
			return nil
		}
		state.LastRun, state.LastMessageID = receipt.SentAt, receipt.MessageID
		// Message delivery must preserve a concurrent Human edit. The Agent
		// updates ongoing notes separately with their exact public revision.
		if revision == receipt.ContextRevision {
			state.Summary = receipt.Summary
		}
		_, err = h.save(r.Context(), key, state, revision)
		if !errors.Is(err, oss.ErrPreconditionFailed) {
			return err
		}
	}
	return fmt.Errorf("context changed repeatedly; retry the same receipt: %w", oss.ErrPreconditionFailed)
}
