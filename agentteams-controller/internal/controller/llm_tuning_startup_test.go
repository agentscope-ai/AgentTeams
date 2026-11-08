package controller

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// #1363 P2 regression (shiyiyue 10-14 review): the 8 startup-only LLM tuning
// fields must participate in the member-runtime (qwenpaw / deepseek-harness)
// pod spec hash. The values reach the QwenPaw runtime only through the
// container environment, so a change has to trigger a container recreate
// (spec-hash driven) — exactly the WorkerSpec contract "effective value
// takes effect when the container is recreated".
func TestLlmTuningFieldsAffectMemberRuntimePodHash(t *testing.T) {
	base := v1beta1.WorkerSpec{Runtime: "qwenpaw", Image: "agentteams/qwenpaw-worker:v1"}
	baseHash := hashAppliedWorkerSpecForRuntime(base, "qwenpaw")

	changes := map[string]func(*v1beta1.WorkerSpec){
		"LlmMaxRetries":      func(s *v1beta1.WorkerSpec) { s.LlmMaxRetries = "7" },
		"LlmBackoffBase":     func(s *v1beta1.WorkerSpec) { s.LlmBackoffBase = "2" },
		"LlmBackoffCap":      func(s *v1beta1.WorkerSpec) { s.LlmBackoffCap = "30" },
		"LlmMaxConcurrent":   func(s *v1beta1.WorkerSpec) { s.LlmMaxConcurrent = "20" },
		"LlmMaxQpm":          func(s *v1beta1.WorkerSpec) { s.LlmMaxQpm = "120" },
		"LlmRateLimitPause":  func(s *v1beta1.WorkerSpec) { s.LlmRateLimitPause = "10" },
		"LlmRateLimitJitter": func(s *v1beta1.WorkerSpec) { s.LlmRateLimitJitter = "0.5" },
		"LlmAcquireTimeout":  func(s *v1beta1.WorkerSpec) { s.LlmAcquireTimeout = "60" },
	}
	for name, mutate := range changes {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if got := hashAppliedWorkerSpecForRuntime(changed, "qwenpaw"); got == baseHash {
				t.Fatalf("%s change must affect the qwenpaw pod hash (would not recreate the container)", name)
			}
		})
	}
}

// #1363 P2 regression guard: the two hot stream-timeout fields must NOT
// enter the member-runtime pod hash. Changing them hot-applies through the
// QwenPaw envs API without a restart; a hash change would recreate the
// container and break the hot-update contract.
func TestLlmStreamTimeoutsDoNotAffectMemberRuntimePodHash(t *testing.T) {
	base := v1beta1.WorkerSpec{Runtime: "qwenpaw", Image: "agentteams/qwenpaw-worker:v1"}
	baseHash := hashAppliedWorkerSpecForRuntime(base, "qwenpaw")

	first := base
	first.LlmStreamFirstContentTimeout = "300"
	if got := hashAppliedWorkerSpecForRuntime(first, "qwenpaw"); got != baseHash {
		t.Fatalf("LlmStreamFirstContentTimeout change must not affect the qwenpaw pod hash (hot-applied, no restart)")
	}

	idle := base
	idle.LlmStreamIdleTimeout = "60"
	if got := hashAppliedWorkerSpecForRuntime(idle, "qwenpaw"); got != baseHash {
		t.Fatalf("LlmStreamIdleTimeout change must not affect the qwenpaw pod hash (hot-applied, no restart)")
	}
}

// #1363 P2 regression (shiyiyue 10-14 review, shiyiyue's exact case): a
// worker startup LLM tuning change (LlmMaxRetries 3 -> 7) must recreate the
// container through the spec-hash path so the new value reaches the QwenPaw
// runtime env.
func TestWorkerReconcileLlmTuningChangeRecreatesContainer(t *testing.T) {
	worker := newWorker("tuning-w", v1beta1.WorkerSpec{
		Runtime:       "qwenpaw",
		Image:         "agentteams/qwenpaw-worker:v1",
		LlmMaxRetries: "3",
	})
	worker.Generation = 1
	rig := newWorkerRig(t, worker)

	if _, _, err := rig.reconcile("tuning-w"); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	creates, deletes, _, _, _ := rig.backend.CallSnapshot()
	if len(creates) != 1 || len(deletes) != 0 {
		t.Fatalf("initial creates=%v deletes=%v, want 1 create", creates, deletes)
	}
	req, ok := rig.backend.FindCreateReq("tuning-w")
	if !ok {
		t.Fatal("no create request recorded")
	}
	if got := req.Env["QWENPAW_LLM_MAX_RETRIES"]; got != "3" {
		t.Fatalf("initial env QWENPAW_LLM_MAX_RETRIES=%q, want 3", got)
	}

	var live v1beta1.Worker
	key := types.NamespacedName{Name: "tuning-w", Namespace: "default"}
	if err := rig.client.Get(context.Background(), key, &live); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	live.Spec.LlmMaxRetries = "7"
	live.Generation = 2
	if err := rig.client.Update(context.Background(), &live); err != nil {
		t.Fatalf("update worker: %v", err)
	}

	rig.backend.ClearCalls()
	// Pass 1: specChanged + running container -> delete the stale resource
	// and requeue. Pass 2: recreate with the new env.
	for pass := 1; pass <= 2; pass++ {
		if _, _, err := rig.reconcile("tuning-w"); err != nil {
			t.Fatalf("recreate reconcile pass %d: %v", pass, err)
		}
	}
	creates, deletes, _, _, _ = rig.backend.CallSnapshot()
	if len(deletes) != 1 {
		t.Fatalf("LlmMaxRetries 3->7 must delete the stale container, deletes=%v", deletes)
	}
	if len(creates) != 1 {
		t.Fatalf("LlmMaxRetries 3->7 must recreate the container, creates=%v", creates)
	}
	req, ok = rig.backend.FindCreateReq("tuning-w")
	if !ok {
		t.Fatal("no recreate request recorded")
	}
	if got := req.Env["QWENPAW_LLM_MAX_RETRIES"]; got != "7" {
		t.Fatalf("recreated env QWENPAW_LLM_MAX_RETRIES=%q, want 7", got)
	}
}

// #1363 P2 regression (shiyiyue 10-14 review): a newly created team-member
// worker must inherit the team's startup LLM tuning defaults into its
// container env (worker-over-team merge; here the worker sets nothing).
func TestWorkerReconcileContainerEnvInheritsTeamLlmTuningDefaults(t *testing.T) {
	worker := newWorker("team-member-w", v1beta1.WorkerSpec{
		Runtime: "qwenpaw",
		Image:   "agentteams/qwenpaw-worker:v1",
	})
	worker.Annotations = map[string]string{v1beta1.AnnotationWorkerTeamName: "runtime-team"}
	worker.Generation = 1
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-cr", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			TeamName:       "runtime-team",
			WorkerMembers:  []v1beta1.TeamWorkerRef{{Name: "team-member-w", Role: "worker"}},
			LlmMaxRetries:  "5",
			LlmBackoffBase: "2",
		},
	}
	rig := newWorkerRig(t, worker, team)

	if _, _, err := rig.reconcile("team-member-w"); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	req, ok := rig.backend.FindCreateReq("team-member-w")
	if !ok {
		t.Fatal("no create request recorded")
	}
	if got := req.Env["QWENPAW_LLM_MAX_RETRIES"]; got != "5" {
		t.Fatalf("env QWENPAW_LLM_MAX_RETRIES=%q, want team default 5 (new worker must inherit)", got)
	}
	if got := req.Env["QWENPAW_LLM_BACKOFF_BASE"]; got != "2" {
		t.Fatalf("env QWENPAW_LLM_BACKOFF_BASE=%q, want team default 2", got)
	}
}

// #1363 P2: the hash-side effective-value merge (hashSpecWithLlmTuningDefaults)
// must fill only keys the worker leaves empty and never mutate the input.
func TestHashSpecWithLlmTuningDefaults(t *testing.T) {
	team := map[string]string{
		"QWENPAW_LLM_MAX_RETRIES":     "5",
		"QWENPAW_LLM_BACKOFF_BASE":    "2",
		"QWENPAW_LLM_ACQUIRE_TIMEOUT": "60",
	}

	t.Run("empty team is identity", func(t *testing.T) {
		spec := v1beta1.WorkerSpec{LlmMaxRetries: "3"}
		got := hashSpecWithLlmTuningDefaults(spec, nil)
		if got.LlmMaxRetries != "3" || got.LlmBackoffBase != "" || got.LlmAcquireTimeout != "" {
			t.Fatalf("nil team must leave the spec untouched, got %+v", got)
		}
	})

	t.Run("team fills only empty keys", func(t *testing.T) {
		spec := v1beta1.WorkerSpec{LlmMaxRetries: "3"}
		got := hashSpecWithLlmTuningDefaults(spec, team)
		if got.LlmMaxRetries != "3" {
			t.Fatalf("worker value must win, got %q", got.LlmMaxRetries)
		}
		if got.LlmBackoffBase != "2" || got.LlmAcquireTimeout != "60" {
			t.Fatalf("empty keys must inherit team defaults, got backoffBase=%q acquireTimeout=%q",
				got.LlmBackoffBase, got.LlmAcquireTimeout)
		}
		if spec.LlmMaxRetries != "3" || spec.LlmBackoffBase != "" {
			t.Fatalf("input spec must not be mutated, got %+v", spec)
		}
	})
}

// #1363 P2: when the owning team's startup LLM default changes, the
// recorded spec hash must differ from the newly computed one (specChanged
// true -> container recreate), and the member context must carry the new
// default for env projection.
func TestWorkerMemberContextTeamLlmTuningChangeTriggersSpecChange(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}
	w := &v1beta1.Worker{}
	w.Name = "solo"
	w.Namespace = "default"
	w.Spec = v1beta1.WorkerSpec{Runtime: "qwenpaw", Image: "agentteams/qwenpaw-worker:v1"}

	teamDefault5 := map[string]string{"QWENPAW_LLM_MAX_RETRIES": "5"}
	teamDefault6 := map[string]string{"QWENPAW_LLM_MAX_RETRIES": "6"}

	w.Status.SpecHash = r.workerMemberContextWithSpec(w, w.Spec, nil, "", teamDefault5).AppliedSpecHash

	same := r.workerMemberContextWithSpec(w, w.Spec, nil, "", teamDefault5)
	if same.SpecChanged {
		t.Fatal("unchanged team default must not mark the spec changed")
	}
	changed := r.workerMemberContextWithSpec(w, w.Spec, nil, "", teamDefault6)
	if !changed.SpecChanged {
		t.Fatal("team default change (5 -> 6) must mark the spec changed (container recreate)")
	}
	if got := changed.TeamLlmTuning["QWENPAW_LLM_MAX_RETRIES"]; got != "6" {
		t.Fatalf("member context must carry the team default for env projection, got %q", got)
	}
}

// #1363 P2 regression: an explicit worker LLM tuning value must beat the
// team default in the container env.
func TestWorkerReconcileContainerEnvWorkerLlmTuningBeatsTeamDefault(t *testing.T) {
	worker := newWorker("team-override-w", v1beta1.WorkerSpec{
		Runtime:       "qwenpaw",
		Image:         "agentteams/qwenpaw-worker:v1",
		LlmMaxRetries: "7",
	})
	worker.Annotations = map[string]string{v1beta1.AnnotationWorkerTeamName: "runtime-team"}
	worker.Generation = 1
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-cr", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			TeamName:      "runtime-team",
			WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "team-override-w", Role: "worker"}},
			LlmMaxRetries: "5",
		},
	}
	rig := newWorkerRig(t, worker, team)

	if _, _, err := rig.reconcile("team-override-w"); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	req, ok := rig.backend.FindCreateReq("team-override-w")
	if !ok {
		t.Fatal("no create request recorded")
	}
	if got := req.Env["QWENPAW_LLM_MAX_RETRIES"]; got != "7" {
		t.Fatalf("env QWENPAW_LLM_MAX_RETRIES=%q, want worker value 7 (beats team default 5)", got)
	}
}
