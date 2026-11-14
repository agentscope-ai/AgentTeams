package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type monitoringReader interface {
	ReadContainerMetrics(context.Context, string) (*backend.ContainerMetrics, error)
	ReadContainerLogs(context.Context, string, int, time.Time) (*backend.ContainerLogs, error)
}

type monitoringHandler struct {
	deps   ServerDeps
	reader monitoringReader
}

type monitoringTarget struct {
	Kind             string                    `json:"kind"`
	Name             string                    `json:"name"`
	Runtime          string                    `json:"runtime,omitempty"`
	Model            string                    `json:"model,omitempty"`
	Phase            string                    `json:"phase,omitempty"`
	AgentStatus      string                    `json:"agentStatus,omitempty"`
	LastActiveAt     *time.Time                `json:"lastActiveAt,omitempty"`
	RunningTaskCount *int                      `json:"runningTaskCount,omitempty"`
	Availability     string                    `json:"availability"` // available, unsupported, missing, error
	Reason           string                    `json:"reason,omitempty"`
	Metrics          *backend.ContainerMetrics `json:"metrics"`
	container        string
}

func newMonitoringHandler(deps ServerDeps) *monitoringHandler {
	h := &monitoringHandler{deps: deps}
	if deps.KubeMode == "embedded" && deps.SocketPath != "" {
		h.reader = backend.NewDockerBackend(backend.DockerConfig{SocketPath: deps.SocketPath}, "")
	}
	return h
}

// targets is a resource allowlist, not a host container inventory. Platform
// components are fixed; Worker/Manager names must exist in this Controller.
func (h *monitoringHandler) targets(ctx context.Context) ([]monitoringTarget, error) {
	var workers v1beta1.WorkerList
	var managers v1beta1.ManagerList
	opts := []client.ListOption{client.InNamespace(h.deps.Namespace)}
	if h.deps.ControllerName != "" {
		opts = append(opts, client.MatchingLabels{v1beta1.LabelController: h.deps.ControllerName})
	}
	if err := h.deps.Client.List(ctx, &workers, opts...); err != nil {
		return nil, err
	}
	if err := h.deps.Client.List(ctx, &managers, opts...); err != nil {
		return nil, err
	}
	prefix := authpkg.ResourcePrefix(h.deps.ResourcePrefix).Or("")
	workerPrefix := h.deps.ContainerPrefix
	rows := make([]monitoringTarget, 0, len(workers.Items)+len(managers.Items)+2)
	for _, m := range managers.Items {
		rows = append(rows, monitoringTarget{Kind: "manager", Name: m.Name, Runtime: m.Spec.Runtime, Model: m.Spec.Model, Phase: m.Status.Phase, container: prefix.ManagerPodName(m.Name)})
	}
	for _, w := range workers.Items {
		row := monitoringTarget{Kind: "worker", Name: w.Name, Runtime: w.Spec.Runtime, Model: w.Spec.Model, Phase: w.Status.Phase, AgentStatus: w.Status.AgentStatus, RunningTaskCount: w.Status.RunningTaskCount, container: workerPrefix + w.Name}
		if t, err := time.Parse(time.RFC3339, w.Status.LastActiveAt); err == nil {
			row.LastActiveAt = &t
		}
		if !w.Spec.DesiredContainerMan() {
			row.Availability = "unsupported"
			row.Reason = "Externally managed Worker has no container data source"
		}
		rows = append(rows, row)
	}
	for _, component := range []string{"controller", "dashboard"} {
		rows = append(rows, monitoringTarget{Kind: "component", Name: component, container: prefix.String() + component})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Kind == rows[j].Kind {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Kind < rows[j].Kind
	})
	for i := range rows {
		if h.reader == nil {
			rows[i].Availability = "unsupported"
			rows[i].Reason = "Container metrics and logs require an embedded Docker data source"
		}
	}
	return rows, nil
}

func monitoringInt(r *http.Request, key string, fallback, max int) (int, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > max {
		return 0, errors.New("invalid " + key)
	}
	return n, nil
}

func (h *monitoringHandler) snapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	page, err := monitoringInt(r, "page", 1, 1000000)
	if err != nil {
		httputil.WriteError(w, 400, err.Error())
		return
	}
	limit, err := monitoringInt(r, "limit", 12, 50)
	if err != nil {
		httputil.WriteError(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	rows, err := h.targets(ctx)
	if err != nil {
		httputil.WriteError(w, 502, "Unable to list monitoring resources")
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	filtered := make([]monitoringTarget, 0, len(rows))
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row.Name+" "+row.Kind+" "+row.Runtime+" "+row.Phase), query) {
			filtered = append(filtered, row)
		}
	}
	total := len(filtered)
	pages := max(1, (total+limit-1)/limit)
	page = min(page, pages)
	start := (page - 1) * limit
	filtered = filtered[start:min(start+limit, total)]
	// Bound daemon work per request and only sample the requested page.
	var wg sync.WaitGroup
	slots := make(chan struct{}, 6)
	for i := range filtered {
		if filtered[i].Availability == "unsupported" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				filtered[i].Availability = "error"
				filtered[i].Reason = "Data source timed out"
				return
			}
			m, err := h.reader.ReadContainerMetrics(ctx, filtered[i].container)
			if err != nil {
				filtered[i].Availability = "error"
				filtered[i].Reason = "Container data source unavailable"
				if errors.Is(err, backend.ErrNotFound) {
					filtered[i].Availability = "missing"
					filtered[i].Reason = "Container does not exist"
				}
				return
			}
			filtered[i].Availability = "available"
			filtered[i].Metrics = m
		}(i)
	}
	wg.Wait()
	httputil.WriteJSON(w, 200, map[string]any{"mode": h.deps.KubeMode, "sampledAt": time.Now().UTC(), "items": filtered, "total": total, "page": page, "limit": limit, "totalPages": pages})
}

var monitoringBearer = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+`)
var monitoringSecret = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|auth[_-]?token|secret[_-]?key|password|authorization|cookie)["']?\s*[:=]\s*["']?)([^"'\s,;}]+)`)

func redactMonitoringLog(text string) string {
	text = monitoringBearer.ReplaceAllString(text, "$1 [REDACTED]")
	return monitoringSecret.ReplaceAllString(text, "${1}[REDACTED]")
}

func (h *monitoringHandler) logs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tail, err := monitoringInt(r, "tail", 300, 1000)
	if err != nil {
		httputil.WriteError(w, 400, err.Error())
		return
	}
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		since, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			httputil.WriteError(w, 400, "since must be an RFC3339 timestamp")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	rows, err := h.targets(ctx)
	if err != nil {
		httputil.WriteError(w, 502, "Unable to list monitoring resources")
		return
	}
	for _, row := range rows {
		if row.Kind != r.PathValue("kind") || row.Name != r.PathValue("name") {
			continue
		}
		if row.Availability == "unsupported" {
			httputil.WriteError(w, 501, row.Reason)
			return
		}
		result, err := h.reader.ReadContainerLogs(ctx, row.container, tail, since)
		if errors.Is(err, backend.ErrNotFound) {
			httputil.WriteError(w, 404, "Container does not exist")
			return
		}
		if err != nil {
			httputil.WriteError(w, 502, "Container logs unavailable")
			return
		}
		result.Text = redactMonitoringLog(result.Text)
		httputil.WriteJSON(w, 200, map[string]any{"kind": row.Kind, "name": row.Name, "tail": tail, "since": since, "sampledAt": time.Now().UTC(), "text": result.Text, "truncated": result.Truncated})
		return
	}
	httputil.WriteError(w, 404, "Monitoring resource not found")
}
