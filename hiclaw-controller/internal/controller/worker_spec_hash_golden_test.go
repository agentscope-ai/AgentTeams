package controller

import (
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
)

// Golden hash vectors lock pod-recreate triggers across standalone Worker,
// Team member, and QwenPaw runtime paths (C10.7 desiredPodRevision).
func TestGoldenAppliedWorkerSpecHashVectors(t *testing.T) {
	sandbox := v1beta1.BackendRuntimeSandbox
	cases := []struct {
		name      string
		spec      v1beta1.WorkerSpec
		runtime   string
		resources *v1beta1.AgentResourceRequirements
		want      string
	}{
		{
			name: "standard-openclaw-pod",
			spec: v1beta1.WorkerSpec{
				Model:      "gpt-4",
				Image:      "agentteams/worker:v1.0.0",
				Runtime:    "copaw",
				WorkerName: "alice",
			},
			want: "a26a27255e0679d1",
		},
		{
			name: "sandbox-backend-layout-version",
			spec: v1beta1.WorkerSpec{
				Model:          "qwen-plus",
				Image:          "agentteams/worker:v1",
				BackendRuntime: &sandbox,
			},
			want: "936f650e6b7af49e",
		},
		{
			name: "qwenpaw-pod-subset",
			spec: v1beta1.WorkerSpec{
				Runtime: "qwenpaw",
				Image:   "qwenpaw:v1",
				Env:     map[string]string{"A": "1"},
			},
			runtime: "qwenpaw",
			want:    "57b9ea73f1bf3424",
		},
		{
			name: "resolved-runtime-empty-spec",
			spec: v1beta1.WorkerSpec{
				Model:      "gpt-4",
				Image:      "agentteams/worker:v1.0.0",
				WorkerName: "alice",
			},
			runtime: "openclaw",
			want:    "b0fee2ca492c1b86",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := desiredPodRevision{
				Spec:      tc.spec,
				Runtime:   tc.runtime,
				Resources: tc.resources,
			}.Hash()
			if got == "" {
				t.Fatal("hash returned empty string")
			}
			if got != tc.want {
				t.Fatalf("hash = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("resolved-runtime-matches-explicit-spec", func(t *testing.T) {
		base := v1beta1.WorkerSpec{
			Model:      "gpt-4",
			Image:      "agentteams/worker:v1.0.0",
			WorkerName: "alice",
		}
		explicit := base
		explicit.Runtime = "openclaw"
		gotEmpty := desiredPodRevision{Spec: base, Runtime: "openclaw"}.Hash()
		gotExplicit := desiredPodRevision{Spec: explicit}.Hash()
		if gotEmpty != gotExplicit {
			t.Fatalf("empty spec + resolved runtime hash %q != explicit runtime hash %q", gotEmpty, gotExplicit)
		}
	})

	t.Run("resolved-runtime-change-triggers-recreate", func(t *testing.T) {
		spec := v1beta1.WorkerSpec{
			Model:      "gpt-4",
			Image:      "agentteams/worker:v1.0.0",
			WorkerName: "alice",
		}
		openclaw := desiredPodRevision{Spec: spec, Runtime: "openclaw"}.Hash()
		copaw := desiredPodRevision{Spec: spec, Runtime: "copaw"}.Hash()
		if openclaw == copaw {
			t.Fatalf("expected different hashes for openclaw vs copaw resolved runtime")
		}
	})
}
