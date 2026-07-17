package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/slicesx"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	workerHealthProbeTimeout      = 2 * time.Second
	workerHealthOverallBudget     = 5 * time.Second
	workerHealthListBudget        = 5 * time.Second
	workerHealthBatchConcurrency  = 8
	workerHealthCacheTTL          = 30 * time.Second
)

type healthAttachItem struct {
	Resp   *WorkerResponse
	Worker *v1beta1.Worker
}

// WorkerHealthProber computes cached five-point worker health for API responses.
type WorkerHealthProber struct {
	k8s          client.Client
	namespace    string
	gateway      gateway.Client
	dataPlaneURL string
	httpClient   *http.Client

	mu    sync.Mutex
	cache map[string]cachedWorkerHealth

	gitWorkersMu sync.RWMutex
	gitWorkers   map[string]struct{}
	gitWorkersAt time.Time
}

type cachedWorkerHealth struct {
	checks  WorkerHealthChecks
	expires time.Time
}

type workerHealthInput struct {
	Name           string
	Phase          string
	ContainerState string
	LastHeartbeat  string
	LastActiveAt   string
	ModelProvider  string
	McpServers     []v1beta1.MCPServer
	NeedsGitProbe  bool
}

// NewWorkerHealthProber builds a prober. Either component may be nil; probes
// degrade to unknown when prerequisites are missing.
func NewWorkerHealthProber(k8s client.Client, namespace string, gw gateway.Client, dataPlaneURL string) *WorkerHealthProber {
	return &WorkerHealthProber{
		k8s:          k8s,
		namespace:    namespace,
		gateway:      gw,
		dataPlaneURL: strings.TrimRight(dataPlaneURL, "/"),
		httpClient:   &http.Client{Timeout: workerHealthProbeTimeout},
		cache:        make(map[string]cachedWorkerHealth),
	}
}

// AttachHealthChecks fills resp.HealthChecks using a short-TTL cache.
func (p *WorkerHealthProber) AttachHealthChecks(ctx context.Context, resp *WorkerResponse, worker *v1beta1.Worker) {
	if p == nil || resp == nil || resp.Name == "" {
		return
	}

	if cached, ok := p.loadCache(resp.Name); ok {
		checks := cached
		resp.HealthChecks = &checks
		return
	}

	input := p.healthInput(ctx, resp, worker)

	probeCtx, cancel := context.WithTimeout(ctx, workerHealthOverallBudget)
	defer cancel()

	checks := p.probeAll(probeCtx, input)
	p.storeCache(resp.Name, checks)
	resp.HealthChecks = &checks
}

// AttachHealthChecksBatch attaches health to many workers in parallel under one
// shared list budget. Remaining workers after the budget get cache hits or
// cheap local probes with LLM/Git marked unknown.
func (p *WorkerHealthProber) AttachHealthChecksBatch(ctx context.Context, items []healthAttachItem) {
	if p == nil || len(items) == 0 {
		return
	}

	batchCtx, cancel := context.WithTimeout(ctx, workerHealthListBudget)
	defer cancel()

	type attachState struct {
		done bool
	}
	states := make([]attachState, len(items))

	sem := make(chan struct{}, workerHealthBatchConcurrency)
	var wg sync.WaitGroup

	for i := range items {
		item := items[i]
		if item.Resp == nil || item.Resp.Name == "" {
			states[i].done = true
			continue
		}
		if cached, ok := p.loadCache(item.Resp.Name); ok {
			checks := cached
			item.Resp.HealthChecks = &checks
			states[i].done = true
			continue
		}

		wg.Add(1)
		go func(idx int, item healthAttachItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if batchCtx.Err() != nil {
				return
			}

			input := p.healthInput(batchCtx, item.Resp, item.Worker)
			checks := p.probeAll(batchCtx, input)
			p.storeCache(item.Resp.Name, checks)
			item.Resp.HealthChecks = &checks
			states[idx].done = true
		}(i, item)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-batchCtx.Done():
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := range items {
		if states[i].done || items[i].Resp == nil || items[i].Resp.Name == "" {
			continue
		}
		if items[i].Resp.HealthChecks != nil {
			continue
		}
		if cached, ok := p.loadCache(items[i].Resp.Name); ok {
			checks := cached
			items[i].Resp.HealthChecks = &checks
			continue
		}
		input := p.healthInput(ctx, items[i].Resp, items[i].Worker)
		checks := probeCheapChecks(input, now)
		items[i].Resp.HealthChecks = &checks
	}
}

func (p *WorkerHealthProber) healthInput(ctx context.Context, resp *WorkerResponse, worker *v1beta1.Worker) workerHealthInput {
	input := workerHealthInputFromResponse(resp, worker)
	if worker != nil {
		input.NeedsGitProbe = p.workerNeedsGitProbe(ctx, worker)
	}
	return input
}

func probeCheapChecks(input workerHealthInput, checkedAt string) WorkerHealthChecks {
	return WorkerHealthChecks{
		Container: probeContainerHealth(input, checkedAt),
		Heartbeat: probeTimestampHealth(input.LastHeartbeat, checkedAt, 10*time.Minute, 30*time.Minute),
		Sync:      probeSyncHealth(input.LastActiveAt, checkedAt),
		LLM:       healthCheck(HealthUnknown, "probe budget exceeded", checkedAt),
		Git:       healthCheck(HealthUnknown, "probe budget exceeded", checkedAt),
	}
}

func workerHealthInputFromResponse(resp *WorkerResponse, worker *v1beta1.Worker) workerHealthInput {
	input := workerHealthInput{
		Name:           resp.Name,
		Phase:          resp.Phase,
		ContainerState: resp.ContainerState,
		LastHeartbeat:  resp.LastHeartbeat,
		LastActiveAt:   resp.LastActiveAt,
	}
	if worker != nil {
		input.ModelProvider = worker.Spec.ModelProvider
		input.McpServers = worker.Spec.McpServers
	}
	return input
}

func (p *WorkerHealthProber) loadCache(name string) (WorkerHealthChecks, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.cache[name]
	if !ok || time.Now().After(entry.expires) {
		delete(p.cache, name)
		return WorkerHealthChecks{}, false
	}
	return entry.checks, true
}

func (p *WorkerHealthProber) storeCache(name string, checks WorkerHealthChecks) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache[name] = cachedWorkerHealth{
		checks:  checks,
		expires: time.Now().Add(workerHealthCacheTTL),
	}
}

func (p *WorkerHealthProber) probeAll(ctx context.Context, input workerHealthInput) WorkerHealthChecks {
	now := time.Now().UTC().Format(time.RFC3339)
	checks := WorkerHealthChecks{
		Container: probeContainerHealth(input, now),
		Heartbeat: probeTimestampHealth(input.LastHeartbeat, now, 10*time.Minute, 30*time.Minute),
		Sync:      probeSyncHealth(input.LastActiveAt, now),
	}

	var wg sync.WaitGroup
	var llmMu, gitMu sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		llm := p.probeLLM(ctx, input, now)
		llmMu.Lock()
		checks.LLM = llm
		llmMu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		git := p.probeGit(ctx, input, now)
		gitMu.Lock()
		checks.Git = git
		gitMu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		if checks.LLM.Status == "" {
			checks.LLM = healthCheck(HealthUnknown, "probe budget exceeded", now)
		}
		if checks.Git.Status == "" {
			checks.Git = healthCheck(HealthUnknown, "probe budget exceeded", now)
		}
	}

	return checks
}

func probeContainerHealth(input workerHealthInput, checkedAt string) HealthCheck {
	state := strings.ToLower(strings.TrimSpace(input.ContainerState))
	if state == "" {
		state = strings.ToLower(strings.TrimSpace(input.Phase))
	}
	switch state {
	case string(backend.StatusRunning), string(backend.StatusReady):
		return healthCheck(HealthHealthy, "container running", checkedAt)
	case string(backend.StatusStarting), "pending":
		return healthCheck(HealthDegraded, "container starting", checkedAt)
	case string(backend.StatusStopped), "sleeping", "failed":
		return healthCheck(HealthDown, "container not running", checkedAt)
	default:
		return healthCheck(HealthUnknown, "container state unavailable", checkedAt)
	}
}

func probeSyncHealth(ts, checkedAt string) HealthCheck {
	if ts == "" {
		return healthCheck(HealthUnknown, "no activity timestamp", checkedAt)
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return healthCheck(HealthUnknown, "invalid activity timestamp", checkedAt)
	}
	age := time.Since(parsed)
	switch {
	case age < 30*time.Minute:
		return healthCheck(HealthHealthy, fmt.Sprintf("active %s ago", formatHealthAge(age)), checkedAt)
	case age < 2*time.Hour:
		return healthCheck(HealthDegraded, fmt.Sprintf("stale %s ago", formatHealthAge(age)), checkedAt)
	default:
		return healthCheck(HealthDown, fmt.Sprintf("inactive %s ago", formatHealthAge(age)), checkedAt)
	}
}

func probeTimestampHealth(ts, checkedAt string, healthyBefore, degradedBefore time.Duration) HealthCheck {
	if ts == "" {
		return healthCheck(HealthUnknown, "no timestamp", checkedAt)
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return healthCheck(HealthUnknown, "invalid timestamp", checkedAt)
	}
	age := time.Since(parsed)
	switch {
	case age < healthyBefore:
		return healthCheck(HealthHealthy, fmt.Sprintf("seen %s ago", formatHealthAge(age)), checkedAt)
	case age < degradedBefore:
		return healthCheck(HealthDegraded, fmt.Sprintf("stale %s ago", formatHealthAge(age)), checkedAt)
	default:
		return healthCheck(HealthDown, fmt.Sprintf("missing %s ago", formatHealthAge(age)), checkedAt)
	}
}

func (p *WorkerHealthProber) probeLLM(ctx context.Context, input workerHealthInput, checkedAt string) HealthCheck {
	if p.dataPlaneURL == "" {
		return healthCheck(HealthUnknown, "gateway URL not configured", checkedAt)
	}

	gatewayKey := p.loadWorkerGatewayKey(ctx, input.Name)
	if gatewayKey == "" {
		return healthCheck(HealthUnknown, "gateway key unavailable", checkedAt)
	}

	url := p.modelsProbeURL(ctx, input.ModelProvider)
	statusCode, err := p.getModelsStatus(ctx, url, gatewayKey)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return healthCheck(HealthDown, "models probe timed out", checkedAt)
		}
		return healthCheck(HealthDown, "models probe failed", checkedAt)
	}
	switch statusCode {
	case http.StatusOK:
		return healthCheck(HealthHealthy, "models endpoint reachable", checkedAt)
	case http.StatusUnauthorized, http.StatusForbidden:
		return healthCheck(HealthDegraded, fmt.Sprintf("models auth HTTP %d", statusCode), checkedAt)
	default:
		return healthCheck(HealthDown, fmt.Sprintf("models HTTP %d", statusCode), checkedAt)
	}
}

func (p *WorkerHealthProber) modelsProbeURL(ctx context.Context, modelProvider string) string {
	if modelProvider != "" && p.gateway != nil {
		info, err := p.gateway.ResolveModelProvider(ctx, modelProvider)
		if err == nil && info != nil && info.IntranetURL != "" {
			return strings.TrimRight(info.IntranetURL, "/") + "/models"
		}
	}
	return p.dataPlaneURL + "/v1/models"
}

func (p *WorkerHealthProber) getModelsStatus(ctx context.Context, url, gatewayKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func (p *WorkerHealthProber) probeGit(ctx context.Context, input workerHealthInput, checkedAt string) HealthCheck {
	if !input.NeedsGitProbe {
		return healthCheck(HealthUnknown, "git not configured", checkedAt)
	}
	if p.gateway == nil {
		return healthCheck(HealthUnknown, "gateway client unavailable", checkedAt)
	}

	servers, err := p.gateway.ListMCPServers(ctx)
	if err != nil {
		if errors.Is(err, gateway.ErrUnsupportedOp) {
			return healthCheck(HealthUnknown, "MCP inventory unavailable", checkedAt)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return healthCheck(HealthDown, "git probe timed out", checkedAt)
		}
		return healthCheck(HealthDown, "git probe failed", checkedAt)
	}

	target := "mcp-gitea-" + input.Name
	consumer := "worker-" + input.Name
	var found *gateway.MCPServerInfo
	for i := range servers {
		if servers[i].Name == target {
			found = &servers[i]
			break
		}
	}
	if found == nil {
		return healthCheck(HealthDown, target+" not registered", checkedAt)
	}
	if slicesx.Contains(found.AllowedConsumers, consumer) {
		return healthCheck(HealthHealthy, target+" authorized", checkedAt)
	}
	return healthCheck(HealthDegraded, target+" missing consumer auth", checkedAt)
}

func (p *WorkerHealthProber) workerNeedsGitProbe(ctx context.Context, worker *v1beta1.Worker) bool {
	for _, mcp := range worker.Spec.McpServers {
		name := strings.ToLower(mcp.Name)
		url := strings.ToLower(mcp.URL)
		if strings.Contains(name, "gitea") || strings.Contains(url, "gitea") {
			return true
		}
	}
	return p.workerInGitProject(ctx, worker.Name)
}

func (p *WorkerHealthProber) workerInGitProject(ctx context.Context, workerName string) bool {
	workers := p.gitProjectWorkers(ctx)
	_, ok := workers[workerName]
	return ok
}

func (p *WorkerHealthProber) gitProjectWorkers(ctx context.Context) map[string]struct{} {
	p.gitWorkersMu.RLock()
	if time.Since(p.gitWorkersAt) < workerHealthCacheTTL && p.gitWorkers != nil {
		cached := p.gitWorkers
		p.gitWorkersMu.RUnlock()
		return cached
	}
	p.gitWorkersMu.RUnlock()

	workers := make(map[string]struct{})
	if p.k8s != nil {
		var projects v1beta1.ProjectList
		if err := p.k8s.List(ctx, &projects, client.InNamespace(p.namespace)); err == nil {
			for i := range projects.Items {
				if len(projects.Items[i].Spec.Repos) == 0 {
					continue
				}
				for _, w := range projects.Items[i].Spec.Workers {
					workers[w] = struct{}{}
				}
				for _, w := range projects.Items[i].Status.RecordedWorkers {
					workers[w] = struct{}{}
				}
			}
		}
	}

	p.gitWorkersMu.Lock()
	p.gitWorkers = workers
	p.gitWorkersAt = time.Now()
	p.gitWorkersMu.Unlock()
	return workers
}

func (p *WorkerHealthProber) loadWorkerGatewayKey(ctx context.Context, workerName string) string {
	if p.k8s == nil {
		return ""
	}
	var secret corev1.Secret
	if err := p.k8s.Get(ctx, client.ObjectKey{
		Namespace: p.namespace,
		Name:      "hiclaw-creds-" + workerName,
	}, &secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return ""
		}
		return ""
	}
	return strings.TrimSpace(string(secret.Data["WORKER_GATEWAY_KEY"]))
}

func healthCheck(status HealthCheckStatus, detail, checkedAt string) HealthCheck {
	return HealthCheck{
		Status:    status,
		Detail:    detail,
		CheckedAt: checkedAt,
	}
}

func formatHealthAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
