package sandbox

import (
	"context"
	"testing"
)

// ── Mock plugin for registry tests ──────────────────────────────────────

type stubPlugin struct{ typeName string }

func (s *stubPlugin) Type() string { return s.typeName }
func (s *stubPlugin) Capabilities(_ ProviderConfig) ProviderCapabilities {
	return ProviderCapabilities{}
}
func (s *stubPlugin) CreateSandboxClaim(_ context.Context, _ SandboxClaimSpec, _ ProviderConfig) (SandboxHandle, error) {
	return SandboxHandle{}, nil
}
func (s *stubPlugin) DeleteSandboxClaim(_ context.Context, _ string, _ ProviderConfig) error {
	return nil
}
func (s *stubPlugin) DeleteSandbox(_ context.Context, _ string, _ ProviderConfig) error {
	return nil
}
func (s *stubPlugin) HibernateSandbox(_ context.Context, _ string, _ ProviderConfig) error {
	return nil
}
func (s *stubPlugin) ResumeSandbox(_ context.Context, _ string, _ ProviderConfig) error { return nil }
func (s *stubPlugin) GetSandboxClaimStatus(_ context.Context, _ string, _ ProviderConfig) (SandboxStatus, error) {
	return SandboxStatus{}, nil
}
func (s *stubPlugin) GetSandboxStatus(_ context.Context, _ string, _ ProviderConfig) (SandboxStatus, error) {
	return SandboxStatus{}, nil
}
func (s *stubPlugin) ListSandboxes(_ context.Context, _ map[string]string, _ ProviderConfig) ([]SandboxStatus, error) {
	return nil, nil
}
func (s *stubPlugin) Validate(_ ProviderConfig) error                       { return nil }
func (s *stubPlugin) HealthCheck(_ context.Context, _ ProviderConfig) error { return nil }

// ── Tests ───────────────────────────────────────────────────────────────

func TestPluginRegistry_RegisterAndGet(t *testing.T) {
	r := NewPluginRegistry()
	p := &stubPlugin{typeName: "test-plugin"}
	r.Register("test-plugin", p)

	got, err := r.Get("test-plugin")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.Type() != "test-plugin" {
		t.Errorf("Type() = %q, want %q", got.Type(), "test-plugin")
	}
}

func TestPluginRegistry_GetNotFound(t *testing.T) {
	r := NewPluginRegistry()

	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for unregistered plugin")
	}
}

func TestPluginRegistry_DuplicateRegisterPanics(t *testing.T) {
	r := NewPluginRegistry()
	p := &stubPlugin{typeName: "dup"}
	r.Register("dup", p)

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	r.Register("dup", p) // should panic
}

func TestPluginRegistry_MultiplePlugins(t *testing.T) {
	r := NewPluginRegistry()
	r.Register("a", &stubPlugin{typeName: "a"})
	r.Register("b", &stubPlugin{typeName: "b"})

	gotA, err := r.Get("a")
	if err != nil || gotA.Type() != "a" {
		t.Errorf("Get('a') = (%v, %v), want type 'a'", gotA, err)
	}
	gotB, err := r.Get("b")
	if err != nil || gotB.Type() != "b" {
		t.Errorf("Get('b') = (%v, %v), want type 'b'", gotB, err)
	}
}
