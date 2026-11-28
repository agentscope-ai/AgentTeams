package backend

import (
	"context"
	"testing"
)

func TestMissingHarnessImageDoesNotSelectOpenClaw(t *testing.T) {
	for _, runtime := range []string{RuntimeQwenPaw, RuntimeHermes, RuntimeDeepSeekHarness, RuntimeOpenHuman} {
		t.Run(runtime, func(t *testing.T) {
			ctx := context.Background()
			docker := &DockerBackend{config: DockerConfig{WorkerImage: "openclaw:legacy"}}
			k8s := NewK8sBackendWithClient(newFakeK8sCoreClient(), K8sConfig{WorkerImage: "openclaw:legacy"}, "worker-", nil)
			sandbox := newTestSandboxBackend(&fakeSandboxPlugin{})
			sandbox.config.QwenPawWorkerImage = ""
			for name, backend := range map[string]WorkerBackend{"docker": docker, "kubernetes": k8s, "sandbox": sandbox} {
				if _, err := backend.Create(ctx, CreateRequest{Name: "missing-image", Runtime: runtime}); err == nil {
					t.Errorf("%s selected an image without a configured %s image", name, runtime)
				}
			}
		})
	}
}

func TestResolveRuntime(t *testing.T) {
	cases := []struct {
		name       string
		reqRuntime string
		fallback   string
		want       string
	}{
		{"explicit_request_wins_over_fallback", RuntimeCopaw, RuntimeHermes, RuntimeCopaw},
		{"explicit_over_empty_fallback", RuntimeOpenClaw, "", RuntimeOpenClaw},
		{"empty_uses_fallback_hermes", "", RuntimeHermes, RuntimeHermes},
		{"empty_uses_fallback_copaw", "", RuntimeCopaw, RuntimeCopaw},
		{"empty_uses_fallback_qwenpaw", "", RuntimeQwenPaw, RuntimeQwenPaw},
		{"empty_and_no_fallback_uses_codex", "", "", RuntimeCodex},
		{"explicit_openclaw_preserved", RuntimeOpenClaw, RuntimeHermes, RuntimeOpenClaw},
		{"explicit_hermes_preserved", RuntimeHermes, RuntimeCopaw, RuntimeHermes},
		{"explicit_qwenpaw_preserved", RuntimeQwenPaw, RuntimeCopaw, RuntimeQwenPaw},
		{"explicit_deepseek_harness_preserved", RuntimeDeepSeekHarness, RuntimeQwenPaw, RuntimeDeepSeekHarness},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveRuntime(tc.reqRuntime, tc.fallback)
			if got != tc.want {
				t.Fatalf("ResolveRuntime(%q, %q) = %q, want %q", tc.reqRuntime, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestValidRuntime(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{RuntimeOpenClaw, true},
		{RuntimeCopaw, true},
		{RuntimeHermes, true},
		{RuntimeQwenPaw, true},
		{RuntimeDeepSeekHarness, true},
		{RuntimeCodex, true},
		{RuntimeQoder, true},
		{RuntimeClaudeCode, true},
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := ValidRuntime(tc.in); got != tc.want {
			t.Fatalf("ValidRuntime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestUsesMemberRuntimeConfig(t *testing.T) {
	for runtime, want := range map[string]bool{
		RuntimeQwenPaw:         true,
		RuntimeDeepSeekHarness: true,
		RuntimeCodex:           true,
		RuntimeQoder:           true,
		RuntimeClaudeCode:      true,
		RuntimeOpenClaw:        false,
		RuntimeCopaw:           false,
		RuntimeHermes:          false,
	} {
		if got := UsesMemberRuntimeConfig(runtime); got != want {
			t.Fatalf("UsesMemberRuntimeConfig(%q) = %v, want %v", runtime, got, want)
		}
	}
}
