package service

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "k8s.io/client-go/kubernetes/fake"
)

// TestSecretCredentialStore_StampsControllerLabel verifies the controller
// name provided at construction is copied onto every credential Secret
// under the agentteams.io/controller key so that multi-instance deployments in
// one namespace can filter their own credential artifacts. Also verifies
// that the Secret's decorative "app" label is derived from
// ResourcePrefix.WorkerAppLabel() — historically this was hardcoded to
// "hiclaw" and drifted from the Pod / SA "app" value.
func TestSecretCredentialStore_StampsControllerLabel(t *testing.T) {
	client := fakeclient.NewSimpleClientset()
	store := &SecretCredentialStore{
		Client:         client,
		Namespace:      "hiclaw",
		ControllerName: "ctl-a",
		ResourcePrefix: auth.DefaultResourcePrefix,
	}

	creds := &WorkerCredentials{
		MatrixPassword: "pw",
		MinIOPassword:  "miniopw",
		GatewayKey:     "gw",
	}
	if err := store.Save(context.Background(), "alice", creds); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sec, err := client.CoreV1().Secrets("hiclaw").Get(context.Background(), "hiclaw-creds-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got := sec.Labels[v1beta1.LabelController]; got != "ctl-a" {
		t.Fatalf("expected controller label ctl-a, got %q (labels=%v)", got, sec.Labels)
	}
	if sec.Labels["agentteams.io/worker"] != "alice" {
		t.Fatalf("expected worker label alice, got %q", sec.Labels["agentteams.io/worker"])
	}
	if got, want := sec.Labels["app"], "agentteams-worker"; got != want {
		t.Fatalf("expected app label %q (derived from ResourcePrefix.WorkerAppLabel), got %q (labels=%v)", want, got, sec.Labels)
	}
}

// TestSecretCredentialStore_AppLabelHonorsResourcePrefix verifies a custom
// tenant prefix flows through to the Secret's decorative "app" label so the
// value stays aligned with Pod / SA app values produced for the same
// tenant.
func TestSecretCredentialStore_AppLabelHonorsResourcePrefix(t *testing.T) {
	client := fakeclient.NewSimpleClientset()
	store := &SecretCredentialStore{
		Client:         client,
		Namespace:      "hiclaw",
		ControllerName: "ctl-a",
		ResourcePrefix: auth.ResourcePrefix("acme-"),
	}

	if err := store.Save(context.Background(), "bob", &WorkerCredentials{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sec, err := client.CoreV1().Secrets("hiclaw").Get(context.Background(), "hiclaw-creds-bob", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got, want := sec.Labels["app"], "acme-worker"; got != want {
		t.Fatalf("expected app label %q, got %q (labels=%v)", want, got, sec.Labels)
	}
}

func TestProvisionerLoadWorkerCredentialsMigratesLegacyRuntimeSecret(t *testing.T) {
	client := fakeclient.NewSimpleClientset()
	store := &SecretCredentialStore{
		Client:         client,
		Namespace:      "hiclaw",
		ControllerName: "ctl-a",
		ResourcePrefix: auth.DefaultResourcePrefix,
	}
	legacy := &WorkerCredentials{
		MatrixPassword: "pw",
		MinIOPassword:  "miniopw",
		GatewayKey:     "gw",
		MatrixToken:    "token",
	}
	if err := store.Save(context.Background(), "leader", legacy); err != nil {
		t.Fatalf("save legacy credentials: %v", err)
	}

	p := NewProvisioner(ProvisionerConfig{Creds: store})
	creds, err := p.loadWorkerCredentials(context.Background(), "team-a-worker-leader", "leader")
	if err != nil {
		t.Fatalf("loadWorkerCredentials: %v", err)
	}
	if creds == nil || creds.GatewayKey != "gw" {
		t.Fatalf("migrated creds=%+v, want gateway key gw", creds)
	}

	if _, err := client.CoreV1().Secrets("hiclaw").Get(context.Background(), "hiclaw-creds-team-a-worker-leader", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected CR-name credential secret after migration: %v", err)
	}
	if _, err := client.CoreV1().Secrets("hiclaw").Get(context.Background(), "hiclaw-creds-leader", metav1.GetOptions{}); err != nil {
		t.Fatalf("legacy runtime-name credential secret should remain untouched: %v", err)
	}
}
