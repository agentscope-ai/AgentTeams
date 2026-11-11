package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "k8s.io/client-go/kubernetes/fake"
)

func TestFileCredentialStoreConcurrentSaveLoad(t *testing.T) {
	store := &FileCredentialStore{Dir: t.TempDir()}
	ctx := context.Background()
	first := &WorkerCredentials{MatrixPassword: "matrix-a", MinIOPassword: "storage-password-a", GatewayKey: "gateway-a", MatrixToken: "token-a"}
	second := &WorkerCredentials{MatrixPassword: "matrix-b", MinIOPassword: "storage-password-b", GatewayKey: "gateway-b", MatrixToken: "token-b"}
	if err := store.Save(ctx, "worker", first); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(ctx, "worker"); err != nil || got == nil || *got != *first {
		t.Fatal("serial save/load did not preserve credentials")
	}

	var writer sync.WaitGroup
	writeErr := make(chan error, 1)
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; i < 12000; i++ {
			creds := first
			if i%2 != 0 {
				creds = second
			}
			if err := store.Save(ctx, "worker", creds); err != nil {
				writeErr <- err
				return
			}
		}
	}()

	incomplete, readErrors := 0, 0
	for i := 0; i < 50000; i++ {
		got, err := store.Load(ctx, "worker")
		if err != nil {
			readErrors++
		} else if got == nil || (*got != *first && *got != *second) {
			incomplete++
		}
	}
	writer.Wait()
	close(writeErr)
	for err := range writeErr {
		t.Fatal(err)
	}
	if incomplete != 0 || readErrors != 0 {
		t.Fatalf("concurrent reads returned %d incomplete credentials and %d errors", incomplete, readErrors)
	}
}

func TestFileCredentialStoreSavePrivateFileAndCleanup(t *testing.T) {
	store := &FileCredentialStore{Dir: t.TempDir()}
	ctx := context.Background()
	creds := &WorkerCredentials{MinIOPassword: "storage-password"}
	if err := store.Save(ctx, "worker", creds); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.Dir, "worker.env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential permissions = %o, want 600", info.Mode().Perm())
	}
	if err := os.Mkdir(filepath.Join(store.Dir, "blocked.env"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, "blocked", creds); err == nil {
		t.Fatal("save over a directory should fail")
	}
	leftovers, err := filepath.Glob(filepath.Join(store.Dir, ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatal("save left temporary credential files")
	}
}

// TestSecretCredentialStore_StampsControllerLabel verifies the controller
// name provided at construction is copied onto every credential Secret
// under the agentteams.io/controller key so that multi-instance deployments in
// one namespace can filter their own credential artifacts. Also verifies
// that the Secret's decorative "app" label is derived from
// ResourcePrefix.WorkerAppLabel() — historically this was hardcoded to
// "agentteams" and drifted from the Pod / SA "app" value.
func TestSecretCredentialStore_StampsControllerLabel(t *testing.T) {
	client := fakeclient.NewSimpleClientset()
	store := &SecretCredentialStore{
		Client:         client,
		Namespace:      "agentteams",
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

	sec, err := client.CoreV1().Secrets("agentteams").Get(context.Background(), "agentteams-creds-alice", metav1.GetOptions{})
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
		Namespace:      "agentteams",
		ControllerName: "ctl-a",
		ResourcePrefix: auth.ResourcePrefix("acme-"),
	}

	if err := store.Save(context.Background(), "bob", &WorkerCredentials{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sec, err := client.CoreV1().Secrets("agentteams").Get(context.Background(), "agentteams-creds-bob", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got, want := sec.Labels["app"], "acme-worker"; got != want {
		t.Fatalf("expected app label %q, got %q (labels=%v)", want, got, sec.Labels)
	}
}

func TestProvisionerLoadWorkerCredentialsUsesCredentialResourceNameOnly(t *testing.T) {
	client := fakeclient.NewSimpleClientset()
	store := &SecretCredentialStore{
		Client:         client,
		Namespace:      "agentteams",
		ControllerName: "ctl-a",
		ResourcePrefix: auth.DefaultResourcePrefix,
	}
	runtimeNamed := &WorkerCredentials{
		MatrixPassword: "pw",
		MinIOPassword:  "miniopw",
		GatewayKey:     "gw",
		MatrixToken:    "token",
	}
	if err := store.Save(context.Background(), "leader", runtimeNamed); err != nil {
		t.Fatalf("save runtime-named credentials: %v", err)
	}

	p := NewProvisioner(ProvisionerConfig{Creds: store})
	creds, err := p.loadWorkerCredentials(context.Background(), "team-a-worker-leader")
	if err != nil {
		t.Fatalf("loadWorkerCredentials: %v", err)
	}
	if creds != nil {
		t.Fatalf("credentials=%+v, want nil for a different credential resource name", creds)
	}

	if _, err := client.CoreV1().Secrets("agentteams").Get(context.Background(), "agentteams-creds-team-a-worker-leader", metav1.GetOptions{}); err == nil {
		t.Fatal("unexpected credential migration to the Worker CR name")
	}
	if _, err := client.CoreV1().Secrets("agentteams").Get(context.Background(), "agentteams-creds-leader", metav1.GetOptions{}); err != nil {
		t.Fatalf("runtime-name credential secret should remain untouched: %v", err)
	}
}
