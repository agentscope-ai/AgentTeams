package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// annotationSubagentModelApplied records the subagent model value that was
// last successfully hot-applied to the worker's running process. Presence
// means "applied"; an absent annotation means "never applied (or cleared)".
const annotationSubagentModelApplied = "agentteams.io/subagent-model-applied"

// subagentModelDialTimeout bounds one model-settings dial. The worker
// console is on the same docker network; anything slower indicates the
// process is wedged — the periodic reconcile will retry.
const subagentModelDialTimeout = 5 * time.Second

// applySubagentModelHot dials the running worker's console to make a
// changed subagent model effective without a restart (#1292 Part 3).
//
// Why: the declarative chain (openclaw.json → bridge → agent.json) updates
// the file, but a running qwenpaw process keeps its in-memory config until
// it is told otherwise — the model-settings endpoint mutates agent.json and
// schedules an in-process agent reload.
//
// Guard rails:
//   - embedded (docker) mode only — in incluster mode the worker container
//     is not addressable from the controller (same guard as the
//     checkpoint/skills/approval proxies).
//   - worker container must be running — a (re)created container starts
//     from the already-deployed openclaw.json and needs no dial.
//   - no-op when the annotation already matches the resolved value,
//     including both-empty (no subagent model → nothing to apply).
//
// Best-effort by design: a failed dial (worker restarting, transient
// network) logs and leaves the annotation untouched, so the next periodic
// reconcile retries. It never fails the worker reconcile.
func (r *WorkerReconciler) applySubagentModelHot(ctx context.Context, w *v1beta1.Worker, spec v1beta1.WorkerSpec, configCtx MemberContext, state *MemberState) {
	if r.KubeMode != "embedded" {
		return
	}
	if state.ContainerState != string(backend.StatusRunning) {
		return
	}
	resolved := spec.SubagentModel
	if resolved == "" {
		resolved = configCtx.TeamSubagentModel
	}
	if w.Annotations[annotationSubagentModelApplied] == resolved {
		return // already applied (or nothing to apply)
	}

	urlFor := r.subagentModelURLFor
	if urlFor == nil {
		urlFor = func(w *v1beta1.Worker, spec v1beta1.WorkerSpec) string {
			port := service.EffectiveWorkerConsolePort(spec.Env)
			return fmt.Sprintf("http://%s%s:%s", r.ContainerPrefix, spec.EffectiveWorkerName(w.Name), port)
		}
	}
	baseURL := urlFor(w, spec)
	if err := patchSubagentModel(ctx, baseURL, subagentConsoleAgentID, resolved); err != nil {
		log.FromContext(ctx).Error(err, "subagent model hot-apply failed, will retry on next reconcile", "worker", w.Name, "model", resolved)
		return
	}

	base := w.DeepCopy()
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	if resolved == "" {
		delete(w.Annotations, annotationSubagentModelApplied)
	} else {
		w.Annotations[annotationSubagentModelApplied] = resolved
	}
	if err := r.Patch(ctx, w, client.MergeFrom(base)); err != nil {
		log.FromContext(ctx).Error(err, "failed to record subagent model hot-apply annotation", "worker", w.Name)
	}
}

// subagentConsoleAgentID is the single agent ID an AgentTeams worker runs
// (the bridge provisions one default agent per worker container).
const subagentConsoleAgentID = "default"

// subagentModelProviderID is the fixed internal provider the model-settings
// dial must reference — the worker only has the agentteams-gateway provider
// (bridge.py writes it into the provider list at startup).
const subagentModelProviderID = "agentteams-gateway"

// patchSubagentModel calls the worker console's
// PATCH /api/agents/{agentID}/model-settings with an explicit
// subagent_model value (null clears it). The endpoint applies only the
// fields present in the body and triggers an in-process agent reload, so a
// running process picks up the change without a restart.
//
// Trusted-network call: no auth, same trust model as the channel/skills/
// approval proxies that dial the worker console.
func patchSubagentModel(ctx context.Context, baseURL, agentID, model string) error {
	var slot any
	if model != "" {
		slot = map[string]string{
			"provider_id": subagentModelProviderID,
			"model":       model,
		}
	}
	payload := map[string]any{"subagent_model": slot}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal model-settings payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		baseURL+"/api/agents/"+agentID+"/model-settings", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build model-settings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: subagentModelDialTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dial worker model-settings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("model-settings status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
