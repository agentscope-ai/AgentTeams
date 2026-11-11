package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type refreshManagerMatrix struct{ matrix.Client }

func (refreshManagerMatrix) LoginAppServiceUser(_ context.Context, name string) (string, error) {
	return "matrix-" + name, nil
}

type refreshManagerStore struct {
	service.CredentialStore
	owner, saved string
	value        *service.WorkerCredentials
}

func (s *refreshManagerStore) Load(_ context.Context, name string) (*service.WorkerCredentials, error) {
	if name == s.owner {
		return s.value, nil
	}
	return nil, nil
}

func (s *refreshManagerStore) Save(_ context.Context, name string, value *service.WorkerCredentials) error {
	s.saved, s.value = name, value
	return nil
}

func TestCanonicalManagerRefreshResolvesControllerCredentialOwner(t *testing.T) {
	for _, name := range []string{"default", "named-manager"} {
		t.Run(name, func(t *testing.T) {
			m := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{v1beta1.LabelController: "ctl"}}}
			other := &v1beta1.Manager{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns", Labels: map[string]string{v1beta1.LabelController: "other-ctl"}}}
			c := fake.NewClientBuilder().WithScheme(newLifecycleTestScheme(t)).WithObjects(m, other).Build()
			store := &refreshManagerStore{owner: name, value: &service.WorkerCredentials{MatrixToken: "old"}}
			p := service.NewProvisioner(service.ProvisionerConfig{Matrix: refreshManagerMatrix{}, MatrixConfig: matrix.Config{AppServiceEnabled: true}, Creds: store})
			h := NewCredentialsHandler(nil, p)
			h.client, h.namespace, h.controllerName = c, "ns", "ctl"
			r := httptest.NewRequest(http.MethodPost, "/api/v1/credentials/matrix-token", nil)
			r = r.WithContext(context.WithValue(r.Context(), auth.CallerKeyForTest(), &auth.CallerIdentity{Role: auth.RoleManager, Username: "manager"}))
			w := httptest.NewRecorder()
			h.RefreshMatrixToken(w, r)
			if w.Code != http.StatusOK || store.saved != name || store.value.MatrixToken != "matrix-manager" {
				t.Fatalf("refresh: status=%d owner=%q token=%q", w.Code, store.saved, store.value.MatrixToken)
			}
		})
	}
}
