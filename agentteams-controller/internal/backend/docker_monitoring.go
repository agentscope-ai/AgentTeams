package backend

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ContainerMetrics describes a container, not an individual OS process.
// Nil usage means unavailable; a nil limit means no explicit container limit.
type ContainerMetrics struct {
	State            string     `json:"state"`
	StartedAt        *time.Time `json:"startedAt"`
	RestartCount     int        `json:"restartCount"`
	OOMKilled        bool       `json:"oomKilled"`
	ExitCode         int        `json:"exitCode"`
	CPUPercent       *float64   `json:"cpuPercent"`
	MemoryUsageBytes *uint64    `json:"memoryUsageBytes"`
	MemoryLimitBytes *uint64    `json:"memoryLimitBytes"`
	SampledAt        *time.Time `json:"sampledAt"`
	UsageError       string     `json:"usageError,omitempty"`
}

type ContainerLogs struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

const monitoringMaxBytes = 1024 * 1024

// monitoringRead is deliberately GET-only and never returns an upstream body
// on error: inspect/log errors can contain deployment credentials.
func (d *DockerBackend) monitoringRead(ctx context.Context, path string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return nil, false, err
	}
	res, err := d.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("container data source unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, false, ErrNotFound
	}
	if res.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("container data source returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, monitoringMaxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("reading container data failed")
	}
	truncated := len(body) > monitoringMaxBytes
	if truncated {
		body = body[:monitoringMaxBytes]
	}
	return body, truncated, nil
}

// ReadContainerMetrics accepts a full container name resolved by the server
// from a resource or a fixed platform component, never a caller-supplied path.
func (d *DockerBackend) ReadContainerMetrics(ctx context.Context, container string) (*ContainerMetrics, error) {
	path := "/containers/" + url.PathEscape(container)
	body, _, err := d.monitoringRead(ctx, path+"/json")
	if err != nil {
		return nil, err
	}
	var inspect struct {
		State struct {
			Status    string
			StartedAt time.Time
			OOMKilled bool
			ExitCode  int
		}
		RestartCount int
		HostConfig   struct{ Memory uint64 }
	}
	if json.Unmarshal(body, &inspect) != nil {
		return nil, fmt.Errorf("invalid container state response")
	}
	m := &ContainerMetrics{State: inspect.State.Status, RestartCount: inspect.RestartCount, OOMKilled: inspect.State.OOMKilled, ExitCode: inspect.State.ExitCode}
	if !inspect.State.StartedAt.IsZero() {
		m.StartedAt = &inspect.State.StartedAt
	}
	if inspect.HostConfig.Memory > 0 {
		m.MemoryLimitBytes = &inspect.HostConfig.Memory
	}
	if m.State != "running" {
		return m, nil
	}
	body, _, err = d.monitoringRead(ctx, path+"/stats?stream=false")
	if err != nil {
		m.UsageError = "Resource usage unavailable"
		return m, nil
	}
	var stats dockerMonitoringStats
	if json.Unmarshal(body, &stats) != nil {
		m.UsageError = "Invalid resource usage response"
		return m, nil
	}
	m.CPUPercent = stats.cpuPercent()
	if stats.Memory.Usage != nil {
		usage := *stats.Memory.Usage
		cache := stats.Memory.Stats["inactive_file"]
		if _, ok := stats.Memory.Stats["inactive_file"]; !ok {
			cache = stats.Memory.Stats["total_inactive_file"]
		}
		if cache <= usage {
			usage -= cache
		}
		m.MemoryUsageBytes = &usage
	}
	if !stats.Read.IsZero() {
		m.SampledAt = &stats.Read
	}
	return m, nil
}

type dockerMonitoringCPU struct {
	CPUUsage struct {
		Total  uint64   `json:"total_usage"`
		PerCPU []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	System uint64 `json:"system_cpu_usage"`
	Online uint64 `json:"online_cpus"`
}
type dockerMonitoringStats struct {
	Read     time.Time           `json:"read"`
	CPU      dockerMonitoringCPU `json:"cpu_stats"`
	Previous dockerMonitoringCPU `json:"precpu_stats"`
	Memory   struct {
		Usage *uint64           `json:"usage"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

func (s dockerMonitoringStats) cpuPercent() *float64 {
	n := s.CPU.Online
	if n == 0 {
		n = uint64(len(s.CPU.CPUUsage.PerCPU))
	}
	if n == 0 || s.Previous.System == 0 || s.CPU.System <= s.Previous.System || s.CPU.CPUUsage.Total < s.Previous.CPUUsage.Total {
		return nil
	}
	v := float64(s.CPU.CPUUsage.Total-s.Previous.CPUUsage.Total) / float64(s.CPU.System-s.Previous.System) * float64(n) * 100
	return &v
}

// ReadContainerLogs preserves stdout/stderr order and supports Docker's TTY
// and multiplexed formats. The response is capped independently of tail.
func (d *DockerBackend) ReadContainerLogs(ctx context.Context, container string, tail int, since time.Time) (*ContainerLogs, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {strconv.Itoa(tail)}}
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.Unix(), 10))
	}
	body, truncated, err := d.monitoringRead(ctx, "/containers/"+url.PathEscape(container)+"/logs?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	if len(body) >= 8 && (body[0] == 1 || body[0] == 2) && body[1] == 0 && body[2] == 0 && body[3] == 0 {
		for len(body) >= 8 {
			n := uint64(binary.BigEndian.Uint32(body[4:8]))
			body = body[8:]
			if n > uint64(len(body)) {
				text.Write(body)
				truncated = true
				break
			}
			text.Write(body[:n])
			body = body[n:]
		}
	} else {
		text.Write(body)
	}
	return &ContainerLogs{Text: text.String(), Truncated: truncated}, nil
}
