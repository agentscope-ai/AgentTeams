package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newHumanUpdateRig(t *testing.T) *ResourceHandler {
	t.Helper()
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "market-team", Namespace: "default"}}
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "market-dev", Namespace: "default"}}
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "maizong", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:     "Mai",
			Email:           "maizong@example.com",
			PermissionLevel: 2,
			AccessibleTeams: []string{"market-team"},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, worker, human).Build()
	return NewResourceHandler(k8sClient, "default", nil, "")
}

func putHuman(t *testing.T, handler *ResourceHandler, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/humans/"+name, bytes.NewReader([]byte(body)))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	handler.UpdateHuman(rec, req)
	return rec
}

func TestUpdateHuman_LevelAndTeamsApplied(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "maizong", `{"permissionLevel":1,"accessibleTeams":["market-team"],"displayName":"Mai Zong"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.PermissionLevel != 1 {
		t.Errorf("level = %d, want 1", resp.PermissionLevel)
	}
	if resp.DisplayName != "Mai Zong" {
		t.Errorf("displayName = %q", resp.DisplayName)
	}
	// Untouched field preserved.
	if resp.Email != "maizong@example.com" {
		t.Errorf("email changed: %q", resp.Email)
	}
}

func TestUpdateHuman_PartialMergePreservesOthers(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "maizong", `{"note":"onboarding done"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.PermissionLevel != 2 || len(resp.AccessibleTeams) != 1 {
		t.Errorf("partial merge clobbered fields: level=%d teams=%v", resp.PermissionLevel, resp.AccessibleTeams)
	}
	if resp.Note != "onboarding done" {
		t.Errorf("note = %q", resp.Note)
	}
}

func TestUpdateHuman_ClearsListWithEmptyArray(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "maizong", `{"accessibleTeams":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.AccessibleTeams != nil {
		t.Errorf("expected cleared list, got %v", resp.AccessibleTeams)
	}
}

func TestUpdateHuman_CapabilitiesApplied(t *testing.T) {
	handler := newHumanUpdateRig(t)
	// Unordered + duplicated input must land deduped and sorted; other
	// fields untouched.
	rec := putHuman(t, handler, "maizong", `{"capabilities":["channel_secrets","approval_policy","channel_secrets"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"approval_policy", "channel_secrets"}
	if len(resp.Capabilities) != len(want) || resp.Capabilities[0] != want[0] || resp.Capabilities[1] != want[1] {
		t.Errorf("capabilities = %v, want %v", resp.Capabilities, want)
	}
	if len(resp.AccessibleTeams) != 1 || resp.AccessibleTeams[0] != "market-team" {
		t.Errorf("capabilities grant clobbered accessibleTeams: %v", resp.AccessibleTeams)
	}
	if resp.Email != "maizong@example.com" {
		t.Errorf("capabilities grant clobbered email: %q", resp.Email)
	}
}

func TestUpdateHuman_CapabilitiesOmittedPreserved(t *testing.T) {
	handler := newHumanUpdateRig(t)
	if rec := putHuman(t, handler, "maizong", `{"capabilities":["secret_reveal"]}`); rec.Code != http.StatusOK {
		t.Fatalf("prime: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// A later update that omits capabilities must not clear them.
	rec := putHuman(t, handler, "maizong", `{"note":"no capability change"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Capabilities) != 1 || resp.Capabilities[0] != "secret_reveal" {
		t.Errorf("omitted capabilities cleared the list: %v", resp.Capabilities)
	}
}

func TestUpdateHuman_CapabilitiesClearedWithEmptyArray(t *testing.T) {
	handler := newHumanUpdateRig(t)
	if rec := putHuman(t, handler, "maizong", `{"capabilities":["full_access"]}`); rec.Code != http.StatusOK {
		t.Fatalf("prime: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec := putHuman(t, handler, "maizong", `{"capabilities":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Capabilities != nil {
		t.Errorf("expected cleared list, got %v", resp.Capabilities)
	}
}

func TestUpdateHuman_UnknownCapabilityRejected(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "maizong", `{"capabilities":["skill_publish"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !containsAll(body, `skill_publish`, "full_access", "secret_reveal") {
		t.Errorf("error should name the unknown value and list the valid set: %s", body)
	}
	// The rejected update must not persist.
	rec = putHuman(t, handler, "maizong", `{"note":"noop"}`)
	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Capabilities != nil {
		t.Errorf("rejected capability leaked into the CR: %v", resp.Capabilities)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestUpdateHuman_InvalidLevelRejected(t *testing.T) {
	handler := newHumanUpdateRig(t)
	for _, level := range []string{"0", "4", "-1"} {
		rec := putHuman(t, handler, "maizong", `{"permissionLevel":`+level+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("level %s: expected 400, got %d: %s", level, rec.Code, rec.Body.String())
		}
	}
}

func TestUpdateHuman_MissingTeamRejected(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "maizong", `{"accessibleTeams":["ghost-team"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("ghost-team")) {
		t.Errorf("error should name the missing team: %s", rec.Body.String())
	}
}

func TestUpdateHuman_MissingWorkerRejected(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "maizong", `{"accessibleWorkers":["ghost-dev"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("ghost-dev")) {
		t.Errorf("error should name the missing worker: %s", rec.Body.String())
	}
}

func TestUpdateHuman_ExistingWorkerAllowed(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "maizong", `{"accessibleWorkers":["market-dev"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateHuman_NotFound(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "ghost", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A backend failure while listing Teams is a server error, not a 400 on the
// request — the request itself may be perfectly valid.
func TestUpdateHuman_TeamListFailureIsServerError(t *testing.T) {
	scheme := newServerTestScheme(t)
	human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "maizong", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(human).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return errors.New("k8s api timeout")
			},
		}).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")
	rec := putHuman(t, handler, "maizong", `{"accessibleTeams":["market-team"]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for backend list failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("validate human references")) {
		t.Errorf("server error should carry the validation op, got: %s", rec.Body.String())
	}
}

// Same for a Worker Get that fails with a non-NotFound backend error.
func TestUpdateHuman_WorkerGetFailureIsServerError(t *testing.T) {
	scheme := newServerTestScheme(t)
	human := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "maizong", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(human).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*v1beta1.Worker); ok {
					return errors.New("k8s api timeout")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")
	rec := putHuman(t, handler, "maizong", `{"accessibleWorkers":["market-dev"]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for backend get failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("validate human references")) {
		t.Errorf("server error should carry the validation op, got: %s", rec.Body.String())
	}
}
