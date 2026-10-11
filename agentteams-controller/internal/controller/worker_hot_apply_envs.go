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

// annotationLlmStreamTimeoutsApplied records the worker LLM stream timeouts
// (first-content/idle, in seconds) last successfully hot-applied to the
// running worker's process through the QwenPaw envs API. The value is
// "first:idle" with "-" standing for an unset side (e.g. "300:-"); an
// absent annotation means "never applied (or cleared)".
const annotationLlmStreamTimeoutsApplied = "agentteams.io/llm-stream-timeouts-applied"

// The QwenPaw runtime env keys backing the two timeouts. retry_chat_model.py
// re-reads them (via EnvVarLoader.get_float) on every stream call, so a
// PATCH makes a change effective without a restart. They are hot_runtime
// editable as of QwenPaw 2.2.1 (the envs API / catalog surface).
const (
	envLlmStreamFirstContentTimeout = "QWENPAW_LLM_STREAM_FIRST_CONTENT_TIMEOUT"
	envLlmStreamIdleTimeout         = "QWENPAW_LLM_STREAM_IDLE_TIMEOUT"
)

// llmStreamEnvsDialTimeout bounds one envs dial. Same rationale as
// subagentModelDialTimeout: the worker console is on the same docker network;
// anything slower indicates the process is wedged and the periodic reconcile
// retries.
const llmStreamEnvsDialTimeout = 5 * time.Second

// applyLlmStreamTimeoutsHot dials the running worker's console to make a
// changed LLM stream timeout effective without a restart.
//
// The QwenPaw runtime re-reads QWENPAW_LLM_STREAM_FIRST_CONTENT_TIMEOUT /
// QWENPAW_LLM_STREAM_IDLE_TIMEOUT on every stream, so PATCHing the envs
// (which also injects them into os.environ) is enough — no config file edit
// and no agent reload.
//
// Guard rails (mirrors applySubagentModelHot, plus the runtime gate the envs
// API requires):
//   - embedded (docker) mode only — in incluster mode the worker container is
//     not addressable from the controller.
//   - the worker container must be running — a (re)created container starts
//     with the envs already projected and needs no dial.
//   - qwenpaw runtime only — the envs hot-apply API is QwenPaw-specific;
//     other runtimes are silently skipped.
//   - no-op when the annotation already matches the resolved value, including
//     both-empty (no timeout configured → nothing to apply or clear).
//
// Clear semantics: when the resolved pair becomes empty after a previous
// apply, each applied key is reset (POST /api/envs/{key}/reset), so the env
// falls back to its inherited value — for these two keys that is the
// worker-entrypoint export (qwenpaw-worker-entrypoint.sh defaults both to
// 300s via AGENTTEAMS_LLM_STREAM_TIMEOUT_S; the QwenPaw registry default of
// 30s is only the last resort when nothing is exported). A plain DELETE is
// rejected by the QwenPaw router for known registry keys ("Known variable
// must be reset").
//
// Best-effort by design: a failed dial (worker restarting, transient network,
// invalid value) logs and leaves the annotation untouched, so the next
// periodic reconcile retries. It never fails the worker reconcile.
func (r *WorkerReconciler) applyLlmStreamTimeoutsHot(ctx context.Context, w *v1beta1.Worker, spec v1beta1.WorkerSpec, configCtx MemberContext, state *MemberState) {
	if r.KubeMode != "embedded" {
		return
	}
	if state.ContainerState != string(backend.StatusRunning) {
		return
	}
	if backend.ResolveRuntime(spec.Runtime, r.DefaultRuntime) != backend.RuntimeQwenPaw {
		return
	}

	first := spec.LlmStreamFirstContentTimeout
	if first == "" {
		first = configCtx.TeamLlmStreamFirstContentTimeout
	}
	idle := spec.LlmStreamIdleTimeout
	if idle == "" {
		idle = configCtx.TeamLlmStreamIdleTimeout
	}

	desired := formatLlmStreamTimeouts(first, idle)
	applied := w.Annotations[annotationLlmStreamTimeoutsApplied]
	if desired == applied {
		return // already applied (or nothing to apply and nothing to clear)
	}

	urlFor := r.envsURLFor
	if urlFor == nil {
		urlFor = func(w *v1beta1.Worker, spec v1beta1.WorkerSpec) string {
			port := service.EffectiveWorkerConsolePort(spec.Env)
			return fmt.Sprintf("http://%s%s:%s", r.ContainerPrefix, spec.EffectiveWorkerName(w.Name), port)
		}
	}
	baseURL := urlFor(w, spec)

	if desired == "" {
		// Clear: reset every key the previous annotation applied (known
		// registry keys reject DELETE; reset restores the inherited value,
		// i.e. the worker-entrypoint 300s default).
		for _, key := range appliedLlmStreamEnvKeys(applied) {
			if err := resetWorkerEnv(ctx, baseURL, key); err != nil {
				log.FromContext(ctx).Error(err, "LLM stream timeout clear failed, will retry on next reconcile", "worker", w.Name, "key", key)
				return
			}
		}
	} else {
		payload := map[string]string{}
		if first != "" {
			payload[envLlmStreamFirstContentTimeout] = first
		}
		if idle != "" {
			payload[envLlmStreamIdleTimeout] = idle
		}
		if err := patchWorkerEnvs(ctx, baseURL, payload); err != nil {
			log.FromContext(ctx).Error(err, "LLM stream timeout hot-apply failed, will retry on next reconcile", "worker", w.Name, "first", first, "idle", idle)
			return
		}
	}

	base := w.DeepCopy()
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	if desired == "" {
		delete(w.Annotations, annotationLlmStreamTimeoutsApplied)
	} else {
		w.Annotations[annotationLlmStreamTimeoutsApplied] = desired
	}
	if err := r.Patch(ctx, w, client.MergeFrom(base)); err != nil {
		log.FromContext(ctx).Error(err, "failed to record LLM stream timeout hot-apply annotation", "worker", w.Name)
	}
}

// formatLlmStreamTimeouts renders the "first:idle" annotation value, using
// "-" for an unset side (e.g. "300:-", "-:120", "300:120"). Both-empty
// renders "" so callers treat it as "no annotation".
func formatLlmStreamTimeouts(first, idle string) string {
	if first == "" && idle == "" {
		return ""
	}
	if first == "" {
		first = "-"
	}
	if idle == "" {
		idle = "-"
	}
	return first + ":" + idle
}

// appliedLlmStreamEnvKeys returns the env keys a stored annotation claims
// were applied — the non-"-" sides, in a stable (first, idle) order. A
// malformed annotation yields only the sides it can parse.
func appliedLlmStreamEnvKeys(annotation string) []string {
	var keys []string
	parts := strings.SplitN(annotation, ":", 2)
	if len(parts) > 0 && parts[0] != "" && parts[0] != "-" {
		keys = append(keys, envLlmStreamFirstContentTimeout)
	}
	if len(parts) > 1 && parts[1] != "" && parts[1] != "-" {
		keys = append(keys, envLlmStreamIdleTimeout)
	}
	return keys
}

// patchWorkerEnvs calls the worker console's PATCH /api/envs with the given
// key/value pairs. The envs store persists to envs.json and injects into the
// running process's os.environ, so the runtime picks the change up on its
// next stream without a restart.
//
// Trusted-network call: no auth, same trust model as the channel/skills/
// approval proxies and the model-settings dial that reach the worker console.
func patchWorkerEnvs(ctx context.Context, baseURL string, payload map[string]string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal envs payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, baseURL+"/api/envs", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build envs patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	hclient := &http.Client{Timeout: llmStreamEnvsDialTimeout}
	resp, err := hclient.Do(req)
	if err != nil {
		return fmt.Errorf("dial worker envs patch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("envs patch status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// resetWorkerEnv calls the worker console's POST /api/envs/{key}/reset,
// clearing a previously applied override so the env falls back to its
// inherited value (the worker-entrypoint export — 300s by default for the
// stream timeouts). The QwenPaw router rejects a plain DELETE for known
// registry keys ("Known variable must be reset"), so reset is the only
// clearing path for the stream timeout keys.
func resetWorkerEnv(ctx context.Context, baseURL, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/envs/"+key+"/reset", nil)
	if err != nil {
		return fmt.Errorf("build envs reset request: %w", err)
	}

	hclient := &http.Client{Timeout: llmStreamEnvsDialTimeout}
	resp, err := hclient.Do(req)
	if err != nil {
		return fmt.Errorf("dial worker envs reset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("envs reset status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
