package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// phaseSummary
// ---------------------------------------------------------------------------

func TestPhaseSummary(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"nil", nil, "0 total"},
		{"empty", []string{}, "0 total"},
		{"single ready", []string{"Ready"}, "1 total, 1 Ready"},
		{"mixed ready and failed", []string{"Ready", "Ready", "Failed"}, "3 total, 2 Ready, 1 Failed"},
		{"one of each common phase", []string{"Ready", "Failed", "Pending"}, "3 total, 1 Ready, 1 Failed, 1 Pending"},
		{"empty bucketed as Pending", []string{"", "Failed"}, "2 total, 1 Failed, 1 Pending"},
		{"unknown phase sorted alphabetically", []string{"Provisioning", "Ready"}, "2 total, 1 Ready, 1 Provisioning"},
		{"multiple unknown phases", []string{"Building", "Provisioning", "Ready"}, "3 total, 1 Ready, 1 Building, 1 Provisioning"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := phaseSummary(tt.in); got != tt.want {
				t.Errorf("phaseSummary(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// sortByHealth
// ---------------------------------------------------------------------------

func TestSortByHealth_FailedAndPendingFirst(t *testing.T) {
	ws := []workerResp{
		{Name: "alice", Phase: "Ready"},
		{Name: "bob", Phase: "Ready"},
		{Name: "charlie", Phase: "Failed"},
		{Name: "dave", Phase: "Pending"},
	}
	sorted := sortByHealth(
		ws,
		func(w workerResp) string { return w.Name },
		func(w workerResp) string { return w.Phase },
	)
	got := make([]string, 0, len(sorted))
	for _, w := range sorted {
		got = append(got, w.Name)
	}
	// Within non-Ready, alphabetical; then Ready.
	want := []string{"charlie", "dave", "alice", "bob"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sortByHealth = %v, want %v", got, want)
	}
}

func TestSortByHealth_AllReadyAlphabetical(t *testing.T) {
	ws := []workerResp{
		{Name: "zeta", Phase: "Ready"},
		{Name: "alpha", Phase: "Ready"},
		{Name: "mike", Phase: "Ready"},
	}
	sorted := sortByHealth(
		ws,
		func(w workerResp) string { return w.Name },
		func(w workerResp) string { return w.Phase },
	)
	got := []string{sorted[0].Name, sorted[1].Name, sorted[2].Name}
	want := []string{"alpha", "mike", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sortByHealth = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// pickTip
// ---------------------------------------------------------------------------

func TestPickTip_WorkerFailed(t *testing.T) {
	ws := []workerResp{{Name: "alice", Phase: "Failed", Message: "image pull failed"}}
	ts := []teamResp{}
	ms := []managerResp{}
	hs := []humanResp{}
	got := pickTip(ws, ts, ms, hs)
	if !strings.Contains(got, "alice") || !strings.Contains(got, "failed") {
		t.Errorf("expected tip to mention alice and failure, got: %q", got)
	}
	if !strings.Contains(got, "hiclaw get workers alice") {
		t.Errorf("expected tip to suggest hiclaw get command, got: %q", got)
	}
}

func TestPickTip_AllReady(t *testing.T) {
	ws := []workerResp{{Name: "alice", Phase: "Ready"}}
	ts := []teamResp{{Name: "alpha", Phase: "Ready"}}
	ms := []managerResp{{Name: "default", Phase: "Ready"}}
	hs := []humanResp{{Name: "admin", Phase: "Ready"}}
	got := pickTip(ws, ts, ms, hs)
	if !strings.Contains(got, "healthy") {
		t.Errorf("expected healthy message, got: %q", got)
	}
}

func TestPickTip_PendingSuggestsWaiting(t *testing.T) {
	ws := []workerResp{{Name: "alice", Phase: "Pending"}}
	ts := []teamResp{}
	got := pickTip(ws, ts, nil, nil)
	if !strings.Contains(got, "provisioning") {
		t.Errorf("expected provisioning hint, got: %q", got)
	}
}

// ---------------------------------------------------------------------------
// fetchOverview (uses httptest.NewServer to fake the controller)
// ---------------------------------------------------------------------------

func newFakeControllerServer(t *testing.T, callCount *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if callCount != nil {
			atomic.AddInt32(callCount, 1)
		}
		switch r.URL.Path {
		case "/api/v1/workers":
			_ = json.NewEncoder(w).Encode(workerListResp{
				Workers: []workerResp{
					{Name: "alice", Phase: "Ready", ContainerState: "running", Runtime: "openclaw", Model: "qwen3.6-plus"},
					{Name: "bob", Phase: "Ready", ContainerState: "running", Runtime: "openclaw", Model: "qwen3.6-plus"},
					{
						Name:           "charlie",
						Phase:          "Failed",
						ContainerState: "Error",
						Runtime:        "qwenpaw",
						Model:          "qwen3.6-plus",
						Message:        "image pull failed",
					},
				},
				Total: 3,
			})
		case "/api/v1/teams":
			_ = json.NewEncoder(w).Encode(teamListResp{
				Teams: []teamResp{
					{
						Name:         "alpha-team",
						Phase:        "Ready",
						LeaderName:   "alpha-lead",
						ReadyWorkers: 2,
						TotalWorkers: 2,
						WorkerNames:  []string{"dev", "qa"},
					},
				},
				Total: 1,
			})
		case "/api/v1/managers":
			_ = json.NewEncoder(w).Encode(managerListResp{
				Managers: []managerResp{
					{Name: "default", Phase: "Ready", Runtime: "openclaw", Model: "qwen3.6-plus"},
				},
				Total: 1,
			})
		case "/api/v1/humans":
			_ = json.NewEncoder(w).Encode(humanListResp{
				Humans: []humanResp{
					{Name: "admin", Phase: "Ready", DisplayName: "Admin"},
				},
				Total: 1,
			})
		case "/api/v1/version":
			_ = json.NewEncoder(w).Encode(versionResp{
				Controller: "v1.1.3",
				KubeMode:   "embedded",
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestFetchOverview_HappyPath(t *testing.T) {
	var calls int32
	server := newFakeControllerServer(t, &calls)
	defer server.Close()

	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")

	ov, err := fetchOverview()
	if err != nil {
		t.Fatalf("fetchOverview: %v", err)
	}

	if ov.Mode != "embedded" {
		t.Errorf("Mode = %q, want %q", ov.Mode, "embedded")
	}
	if ov.Controller != "v1.1.3" {
		t.Errorf("Controller = %q, want %q", ov.Controller, "v1.1.3")
	}
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Errorf("expected 5 controller calls (workers/teams/managers/humans/version), got %d", got)
	}
	if len(ov.Workers) != 3 {
		t.Fatalf("Workers = %d, want 3", len(ov.Workers))
	}
	// Failed should come first (sortByHealth)
	if ov.Workers[0].Name != "charlie" {
		t.Errorf("Workers[0].Name = %q, want charlie (Failed should be first)", ov.Workers[0].Name)
	}
	if len(ov.Teams) != 1 || ov.Teams[0].Name != "alpha-team" {
		t.Errorf("Teams unexpected: %+v", ov.Teams)
	}
	if len(ov.Managers) != 1 || ov.Managers[0].Name != "default" {
		t.Errorf("Managers unexpected: %+v", ov.Managers)
	}
	if len(ov.Humans) != 1 || ov.Humans[0].Name != "admin" {
		t.Errorf("Humans unexpected: %+v", ov.Humans)
	}

	// Phase counts should reflect the mixed phase set.
	if ov.WorkerCounts["Ready"] != 2 || ov.WorkerCounts["Failed"] != 1 {
		t.Errorf("WorkerCounts = %+v, want 2 Ready, 1 Failed", ov.WorkerCounts)
	}
}

func TestFetchOverview_ControllerError(t *testing.T) {
	// Server returns 500 for everything.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")

	_, err := fetchOverview()
	if err == nil {
		t.Fatal("expected error from fetchOverview, got nil")
	}
	if !strings.Contains(err.Error(), "fetch overview") {
		// The RunE wraps the error; fetchOverview itself returns the raw APIError.
		// Either we got the direct error or the wrapped one. Both prove failure.
		t.Logf("fetchOverview error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// printOverview / truncation
// ---------------------------------------------------------------------------

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestPrintOverview_TruncatesLargeLists(t *testing.T) {
	var workers []workerResp
	for i := 0; i < 25; i++ {
		workers = append(workers, workerResp{
			Name:  "worker-" + leftPad(i),
			Phase: "Ready",
		})
	}
	o := &overview{
		Mode:          "embedded",
		Workers:       workers,
		WorkerCounts:  phaseCounts(phasesFromWorkers(workers)),
		TeamCounts:    map[string]int{},
		ManagerCounts: map[string]int{},
		HumanCounts:   map[string]int{},
	}
	out := captureStdout(t, func() { printOverview(o) })

	if !strings.Contains(out, "Workers (25 total") {
		t.Errorf("expected count header, got: %s", out)
	}
	if !strings.Contains(out, "... and 5 more") {
		t.Errorf("expected truncation footer, got: %s", out)
	}
	// We render exactly statusMaxRows data rows for workers.
	if got := strings.Count(out, "worker-"); got != 20 {
		t.Errorf("expected 20 worker rows in output, got %d", got)
	}
}

func TestPrintOverview_EmptyLists(t *testing.T) {
	o := &overview{
		Mode:          "embedded",
		Workers:       nil,
		Teams:         nil,
		Managers:      nil,
		Humans:        nil,
		WorkerCounts:  map[string]int{},
		TeamCounts:    map[string]int{},
		ManagerCounts: map[string]int{},
		HumanCounts:   map[string]int{},
	}
	out := captureStdout(t, func() { printOverview(o) })
	if !strings.Contains(out, "Workers (0 total)") {
		t.Errorf("expected 0 total, got: %s", out)
	}
	if !strings.Contains(out, "(none)") {
		t.Errorf("expected (none) placeholder for empty lists, got: %s", out)
	}
	if !strings.Contains(out, "all resources healthy") {
		t.Errorf("expected healthy tip, got: %s", out)
	}
}

func TestPrintOverview_HidesDevController(t *testing.T) {
	o := &overview{
		Mode:          "embedded",
		Controller:    "dev",
		WorkerCounts:  map[string]int{},
		TeamCounts:    map[string]int{},
		ManagerCounts: map[string]int{},
		HumanCounts:   map[string]int{},
	}
	out := captureStdout(t, func() { printOverview(o) })
	if strings.Contains(out, "Controller:") {
		t.Errorf("Controller: dev should be hidden, got: %s", out)
	}
}

func TestPrintOverview_ShowsRealController(t *testing.T) {
	o := &overview{
		Mode:          "embedded",
		Controller:    "v1.1.3",
		WorkerCounts:  map[string]int{},
		TeamCounts:    map[string]int{},
		ManagerCounts: map[string]int{},
		HumanCounts:   map[string]int{},
	}
	out := captureStdout(t, func() { printOverview(o) })
	if !strings.Contains(out, "Controller: v1.1.3") {
		t.Errorf("expected Controller: v1.1.3, got: %s", out)
	}
}

// ---------------------------------------------------------------------------
// JSON output shape
// ---------------------------------------------------------------------------

func TestOverviewJSONShape(t *testing.T) {
	o := &overview{
		Mode:       "embedded",
		Controller: "v1.1.3",
		Workers:    []workerResp{{Name: "alice", Phase: "Ready"}},
		WorkerCounts: map[string]int{
			"Ready": 1,
		},
	}
	data, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)
	for _, key := range []string{`"mode":"embedded"`, `"controller":"v1.1.3"`, `"workers"`, `"workerCounts"`, `"Ready":1`} {
		if !strings.Contains(got, key) {
			t.Errorf("JSON missing %q: %s", key, got)
		}
	}
}

func TestRunOnce_JSON(t *testing.T) {
	server := newFakeControllerServer(t, nil)
	defer server.Close()

	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")

	// We can't easily capture stdout from printJSON (it always writes to
	// os.Stdout), so we validate the shape by marshalling ourselves.
	ov, err := fetchOverview()
	if err != nil {
		t.Fatalf("fetchOverview: %v", err)
	}
	data, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Ensure the JSON roundtrips correctly.
	var roundtrip overview
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundtrip.Mode != "embedded" {
		t.Errorf("roundtrip Mode = %q, want embedded", roundtrip.Mode)
	}
	if len(roundtrip.Workers) != 3 {
		t.Errorf("roundtrip Workers = %d, want 3", len(roundtrip.Workers))
	}
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

func leftPad(i int) string {
	const width = 2
	s := []byte("00")
	if i >= 10 {
		s[0] = byte('0' + i/10)
		s[1] = byte('0' + i%10)
	} else {
		s[1] = byte('0' + i)
	}
	return string(s)
}
