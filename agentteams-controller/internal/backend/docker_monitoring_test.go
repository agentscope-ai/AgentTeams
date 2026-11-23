package backend

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDockerMonitoringMetrics(t *testing.T) {
	for _, tc := range []struct {
		name, cache string
		limit       uint64
	}{{"cgroup-v2", "inactive_file", 0}, {"cgroup-v1", "total_inactive_file", 4096}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("monitoring must be read-only")
				}
				switch r.URL.Path {
				case "/containers/custom-worker-dev/json":
					fmt.Fprintf(w, `{"State":{"Status":"running","StartedAt":"2026-11-14T00:00:00Z","OOMKilled":false},"RestartCount":2,"HostConfig":{"Memory":%d},"Config":{"Env":["PASSWORD=secret"]}}`, tc.limit)
				case "/containers/custom-worker-dev/stats":
					if r.URL.Query().Get("stream") != "false" {
						t.Error("stats must not stream")
					}
					fmt.Fprintf(w, `{"read":"2026-11-14T01:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":35000},"system_cpu_usage":200000,"online_cpus":8},"precpu_stats":{"cpu_usage":{"total_usage":10000},"system_cpu_usage":100000},"memory_stats":{"usage":1024,"limit":99999,"stats":{"%s":256}}}`, tc.cache)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			m, err := newTestDockerBackend(t, srv.URL).ReadContainerMetrics(context.Background(), "custom-worker-dev")
			if err != nil {
				t.Fatal(err)
			}
			if m.CPUPercent == nil || *m.CPUPercent != 200 {
				t.Fatalf("CPU = %v; expected two cores (200%%)", m.CPUPercent)
			}
			if m.MemoryUsageBytes == nil || *m.MemoryUsageBytes != 768 {
				t.Fatalf("cache must be excluded: %+v", m)
			}
			if tc.limit == 0 && m.MemoryLimitBytes != nil {
				t.Fatal("daemon memory is not a container limit")
			}
			if tc.limit > 0 && (m.MemoryLimitBytes == nil || *m.MemoryLimitBytes != tc.limit) {
				t.Fatal("explicit limit missing")
			}
			if m.RestartCount != 2 || m.SampledAt == nil || m.StartedAt == nil {
				t.Fatalf("missing lifecycle/sample fields: %+v", m)
			}
		})
	}
}

func TestDockerMonitoringUnavailableAndStopped(t *testing.T) {
	for _, state := range []string{"running", "exited"} {
		t.Run(state, func(t *testing.T) {
			statsCalls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/json") {
					fmt.Fprintf(w, `{"State":{"Status":%q,"OOMKilled":true,"ExitCode":137}}`, state)
					return
				}
				statsCalls++
				http.Error(w, "PASSWORD=must-not-leak", 500)
			}))
			defer srv.Close()
			m, err := newTestDockerBackend(t, srv.URL).ReadContainerMetrics(context.Background(), "dev")
			if err != nil {
				t.Fatal(err)
			}
			if m.CPUPercent != nil || m.MemoryUsageBytes != nil || !m.OOMKilled || m.ExitCode != 137 {
				t.Fatalf("unknown usage must not become zero: %+v", m)
			}
			if state == "exited" && statsCalls != 0 {
				t.Fatal("stopped containers cannot supply live stats")
			}
			if state == "running" && (m.UsageError == "" || strings.Contains(m.UsageError, "PASSWORD")) {
				t.Fatal("usage failure must be explicit and sanitized")
			}
		})
	}
	var s dockerMonitoringStats
	s.CPU.Online = 8
	s.CPU.System = 100
	s.Previous.System = 200
	if s.cpuPercent() != nil {
		t.Fatal("reset counters are not zero usage")
	}
	s.CPU.System = 300
	s.Previous.System = 0
	if s.cpuPercent() != nil {
		t.Fatal("missing baseline must stay unavailable")
	}
}

func TestDockerMonitoringLogs(t *testing.T) {
	frame := func(stream byte, text string) []byte {
		b := make([]byte, 8)
		b[0] = stream
		binary.BigEndian.PutUint32(b[4:], uint32(len(text)))
		return append(b, []byte(text)...)
	}
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprint(tty), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("tail") != "300" || r.URL.Query().Get("since") != "60" || r.Method != "GET" {
					t.Errorf("wrong log query: %s", r.URL)
				}
				if tty {
					fmt.Fprint(w, "first\nerror\nlast\n")
					return
				}
				w.Write(frame(1, "first\n"))
				w.Write(frame(2, "error\n"))
				w.Write(frame(1, "last\n"))
			}))
			defer srv.Close()
			logs, err := newTestDockerBackend(t, srv.URL).ReadContainerLogs(context.Background(), "worker-dev", 300, time.Unix(60, 0))
			if err != nil || logs.Text != "first\nerror\nlast\n" || logs.Truncated {
				t.Fatalf("ordered logs: %+v, %v", logs, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(frame(1, strings.Repeat("x", monitoringMaxBytes+100)))
	}))
	defer srv.Close()
	logs, err := newTestDockerBackend(t, srv.URL).ReadContainerLogs(context.Background(), "dev", 1, time.Time{})
	if err != nil || !logs.Truncated || len(logs.Text) > monitoringMaxBytes {
		t.Fatalf("oversized frame must be bounded: %v", err)
	}
}
