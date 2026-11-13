package server

// Status pusher (R1-2e, 58 §5): the controller → service observation
// writeback. When AGENTTEAMS_SERVICE_URL is set, the leader replica lists
// Worker CRs on an interval and POSTs their observed state to the service's
// /internal/v1/workers/status. It is strictly one-way and best-effort: a
// service outage only logs, never blocks or mutates controller state, and a
// deployment without the service is completely unaffected (no goroutine is
// started when the URL is empty).
//
// The generation echoed in each item is the last generation this controller
// applied from the service (the provision annotation), so the service's
// "generation <= incoming" guard keeps working end to end.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// StatusPusher periodically forwards observed worker state to the service.
type StatusPusher struct {
	Reader    client.Reader
	Namespace string
	BaseURL   string
	Token     string
	Interval  time.Duration
	HTTP      *http.Client
	Now       func() time.Time
}

// NewStatusPusher builds a pusher with the R1 defaults (5s interval, 10s
// timeout).
func NewStatusPusher(reader client.Reader, namespace, baseURL, token string) *StatusPusher {
	return &StatusPusher{
		Reader:    reader,
		Namespace: namespace,
		BaseURL:   baseURL,
		Token:     token,
		Interval:  5 * time.Second,
		HTTP:      &http.Client{Timeout: 10 * time.Second},
		Now:       time.Now,
	}
}

// NeedLeaderElection makes the manager run the pusher only on the leader
// replica: duplicate pushes are harmless (generation-guarded) but pointless.
func (p *StatusPusher) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (p *StatusPusher) Start(ctx context.Context) error {
	if p.Reader == nil || p.BaseURL == "" {
		return nil
	}
	interval := p.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	log := logf.FromContext(ctx).WithName("status-pusher")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := p.PushOnce(ctx); err != nil {
			log.Error(err, "status push failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type statusBatch struct {
	Workers []statusItem `json:"workers"`
}

type statusItem struct {
	Name             string `json:"name"`
	Generation       int64  `json:"generation"`
	Phase            string `json:"phase"`
	ContainerState   string `json:"containerState,omitempty"`
	MatrixUserID     string `json:"matrixUserID,omitempty"`
	RoomID           string `json:"roomID,omitempty"`
	Message          string `json:"message,omitempty"`
	LastActiveAt     string `json:"lastActiveAt,omitempty"`
	AgentStatus      string `json:"agentStatus,omitempty"`
	RunningTaskCount *int   `json:"runningTaskCount,omitempty"`
	LastRunAt        string `json:"lastRunAt,omitempty"`
	LastFinishAt     string `json:"lastFinishAt,omitempty"`
}

// PushOnce lists the workers and posts one batch. An empty namespace skips
// the request entirely.
func (p *StatusPusher) PushOnce(ctx context.Context) error {
	var list v1beta1.WorkerList
	if err := p.Reader.List(ctx, &list, client.InNamespace(p.Namespace)); err != nil {
		return fmt.Errorf("list workers: %w", err)
	}
	if len(list.Items) == 0 {
		return nil
	}
	batch := statusBatch{Workers: make([]statusItem, 0, len(list.Items))}
	for i := range list.Items {
		batch.Workers = append(batch.Workers, statusItem{
			Name:             list.Items[i].Name,
			Generation:       appliedGeneration(list.Items[i].Annotations),
			Phase:            list.Items[i].Status.Phase,
			ContainerState:   list.Items[i].Status.ContainerState,
			MatrixUserID:     list.Items[i].Status.MatrixUserID,
			RoomID:           list.Items[i].Status.RoomID,
			Message:          list.Items[i].Status.Message,
			LastActiveAt:     list.Items[i].Status.LastActiveAt,
			AgentStatus:      list.Items[i].Status.AgentStatus,
			RunningTaskCount: list.Items[i].Status.RunningTaskCount,
			LastRunAt:        list.Items[i].Status.LastRunAt,
			LastFinishAt:     list.Items[i].Status.LastFinishAt,
		})
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("encode status batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		trimTrailingSlash(p.BaseURL)+"/internal/v1/workers/status", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build status request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set(internalTimestampHeader, p.now().UTC().Format(time.RFC3339))
	req.Header.Set(internalRequestIDHeader, newPushID())

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("post status batch: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status batch rejected: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (p *StatusPusher) httpClient() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (p *StatusPusher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func newPushID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}
