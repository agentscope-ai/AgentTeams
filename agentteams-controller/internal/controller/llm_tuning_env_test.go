package controller

import (
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// TestResolveLlmTuningEnv pins the projection rules for the startup-only LLM
// tuning envs: worker-over-team precedence, empty = unset, and value
// validation (invalid values are skipped, never fatal).
func TestResolveLlmTuningEnv(t *testing.T) {
	type testCase struct {
		name    string
		spec    v1beta1.WorkerSpec
		team    map[string]string
		want    map[string]string
		wantSet map[string]bool // keys that must be ABSENT
	}
	cases := []testCase{
		{
			name: "worker value wins over team default",
			spec: v1beta1.WorkerSpec{LlmMaxRetries: "7"},
			team: map[string]string{"QWENPAW_LLM_MAX_RETRIES": "5", "QWENPAW_LLM_MAX_CONCURRENT": "4"},
			want: map[string]string{
				"QWENPAW_LLM_MAX_RETRIES":    "7",
				"QWENPAW_LLM_MAX_CONCURRENT": "4",
			},
		},
		{
			name:    "all unset -> empty",
			spec:    v1beta1.WorkerSpec{},
			team:    nil,
			want:    map[string]string{},
			wantSet: map[string]bool{},
		},
		{
			name: "invalid integer (negative) skipped, valid kept",
			spec: v1beta1.WorkerSpec{LlmMaxRetries: "-1", LlmMaxQpm: "120"},
			team: map[string]string{"QWENPAW_LLM_MAX_RETRIES": "5"},
			want: map[string]string{"QWENPAW_LLM_MAX_QPM": "120"},
		},
		{
			name: "invalid float (NaN / negative / not-a-number) skipped",
			spec: v1beta1.WorkerSpec{
				LlmBackoffBase:    "-0.5",
				LlmBackoffCap:     "abc",
				LlmAcquireTimeout: "1e400",
				LlmRateLimitPause: "2.5",
			},
			team: map[string]string{"QWENPAW_LLM_BACKOFF_BASE": "0.8"},
			want: map[string]string{"QWENPAW_LLM_RATE_LIMIT_PAUSE": "2.5"},
		},
		{
			name: "worker invalid is skipped (no silent fallback to team)",
			spec: v1beta1.WorkerSpec{LlmBackoffCap: "oops"},
			team: map[string]string{"QWENPAW_LLM_BACKOFF_CAP": "60"},
			want: map[string]string{},
		},
		{
			name: "float zero is valid",
			spec: v1beta1.WorkerSpec{LlmRateLimitJitter: "0"},
			team: nil,
			want: map[string]string{"QWENPAW_LLM_RATE_LIMIT_JITTER": "0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveLlmTuningEnv(MemberContext{
				Spec:          tc.spec,
				TeamLlmTuning: tc.team,
			}, nil)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got[%s] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestValidLlmTuningValue pins the value validation against the QwenPaw
// registry value types (integer / float, non-negative, finite).
func TestValidLlmTuningValue(t *testing.T) {
	cases := []struct {
		typ, val string
		want     bool
	}{
		{"integer", "3", true},
		{"integer", "0", true},
		{"integer", "-1", false},
		{"integer", "3.5", false},
		{"integer", "x", false},
		{"float", "1.5", true},
		{"float", "0", true},
		{"float", "-0.1", false},
		{"float", "1e400", false},
		{"float", "nan", false},
		{"float", "x", false},
		{"unknown", "1", false},
	}
	for _, tc := range cases {
		if got := validLlmTuningValue(tc.typ, tc.val); got != tc.want {
			t.Fatalf("validLlmTuningValue(%q, %q) = %v, want %v", tc.typ, tc.val, got, tc.want)
		}
	}
}

// TestAllLlmTuningSet gates the team-defaults List call.
func TestAllLlmTuningSet(t *testing.T) {
	full := v1beta1.WorkerSpec{
		LlmMaxRetries: "1", LlmBackoffBase: "1", LlmBackoffCap: "1",
		LlmMaxConcurrent: "1", LlmMaxQpm: "1", LlmRateLimitPause: "1",
		LlmRateLimitJitter: "1", LlmAcquireTimeout: "1",
	}
	if !allLlmTuningSet(full) {
		t.Fatal("full spec should report all set")
	}
	gap := full
	gap.LlmMaxQpm = ""
	if allLlmTuningSet(gap) {
		t.Fatal("spec with one unset field should report not-all-set")
	}
}
