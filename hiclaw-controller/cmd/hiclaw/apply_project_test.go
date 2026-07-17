package main

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/server"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestApplyFromFiles_ProjectEndToEnd wires the real controller mux (with the
// Step-1 Project REST routes) behind an httptest.Server and exercises
// `hiclaw apply -f` against a fixture Project YAML — the acceptance
// criterion for docs/implementation-milestone-2.md Step 1: without the new
// routes this would 404 (cmd/hiclaw/apply.go's existence-check-GET →
// create/update flow hits /api/v1/projects).
func TestApplyFromFiles_ProjectEndToEnd(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Project{}).
		Build()

	// Auth disabled (nil authenticator) — matches embedded-mode /
	// no-REST-config behavior; RequireAuthz short-circuits to allow-all.
	authMw := authpkg.NewMiddleware(nil, nil, authpkg.NewAuthorizer(), nil, "default")

	httpServer := server.NewHTTPServer("ignored:0", server.ServerDeps{
		Client:    k8sClient,
		AuthMw:    authMw,
		Namespace: "default",
	})

	ts := httptest.NewServer(httpServer.Mux)
	defer ts.Close()

	t.Setenv("AGENTTEAMS_CONTROLLER_URL", ts.URL)
	os.Unsetenv("AGENTTEAMS_AUTH_TOKEN")
	os.Unsetenv("AGENTTEAMS_AUTH_TOKEN_FILE")

	content := `apiVersion: agentteams.io/v1beta1
kind: Project
metadata:
  name: proj-e2e
spec:
  team: alpha-team
  repos:
    - url: https://git.pawcommit.com/org/repo.git
      access: rw
`
	tmpFile, err := os.CreateTemp("", "hiclaw-project-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	tmpFile.Close()

	// First apply: existence-check GET (404) -> POST create.
	if err := applyFromFiles([]string{tmpFile.Name()}); err != nil {
		t.Fatalf("first apply (create) failed: %v", err)
	}

	var created v1beta1.Project
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "proj-e2e", Namespace: "default"}, &created); err != nil {
		t.Fatalf("project not created: %v", err)
	}
	if created.Spec.Team != "alpha-team" {
		t.Fatalf("team = %q, want alpha-team", created.Spec.Team)
	}
	if len(created.Spec.Repos) != 1 || created.Spec.Repos[0].Access != "rw" {
		t.Fatalf("repos = %+v", created.Spec.Repos)
	}

	// Second apply of the same file: existence-check GET (200) -> PUT update.
	if err := applyFromFiles([]string{tmpFile.Name()}); err != nil {
		t.Fatalf("second apply (update) failed: %v", err)
	}
}

// TestApplyFromFiles_TeamModelProviderRoundTrip is the acceptance check for
// docs/implementation-milestone-3.md Step 4: a Team YAML carrying the
// team-wide spec.modelProvider field must round-trip through `hiclaw apply`
// (buildApplyBody flattens all spec keys generically, but without the field
// on CreateTeamRequest/UpdateTeamRequest the controller would silently drop
// it on decode).
func TestApplyFromFiles_TeamModelProviderRoundTrip(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	authMw := authpkg.NewMiddleware(nil, nil, authpkg.NewAuthorizer(), nil, "default")

	httpServer := server.NewHTTPServer("ignored:0", server.ServerDeps{
		Client:    k8sClient,
		AuthMw:    authMw,
		Namespace: "default",
	})

	ts := httptest.NewServer(httpServer.Mux)
	defer ts.Close()

	t.Setenv("AGENTTEAMS_CONTROLLER_URL", ts.URL)
	os.Unsetenv("AGENTTEAMS_AUTH_TOKEN")
	os.Unsetenv("AGENTTEAMS_AUTH_TOKEN_FILE")

	content := `apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: team-modelprovider-e2e
spec:
  modelProvider: qwen
  leader:
    name: team-modelprovider-e2e-lead
`
	tmpFile, err := os.CreateTemp("", "hiclaw-team-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	tmpFile.Close()

	// First apply: existence-check GET (404) -> POST create.
	if err := applyFromFiles([]string{tmpFile.Name()}); err != nil {
		t.Fatalf("first apply (create) failed: %v", err)
	}

	var created v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "team-modelprovider-e2e", Namespace: "default"}, &created); err != nil {
		t.Fatalf("team not created: %v", err)
	}
	if created.Spec.ModelProvider != "qwen" {
		t.Fatalf("spec.modelProvider = %q, want qwen", created.Spec.ModelProvider)
	}

	// Second apply with a changed value: existence-check GET (200) -> PUT update.
	content2 := `apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: team-modelprovider-e2e
spec:
  modelProvider: dashscope
  leader:
    name: team-modelprovider-e2e-lead
`
	if err := os.WriteFile(tmpFile.Name(), []byte(content2), 0o644); err != nil {
		t.Fatalf("rewrite temp file: %v", err)
	}
	if err := applyFromFiles([]string{tmpFile.Name()}); err != nil {
		t.Fatalf("second apply (update) failed: %v", err)
	}

	var updated v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "team-modelprovider-e2e", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("team not found after update: %v", err)
	}
	if updated.Spec.ModelProvider != "dashscope" {
		t.Fatalf("spec.modelProvider after update = %q, want dashscope", updated.Spec.ModelProvider)
	}
}
