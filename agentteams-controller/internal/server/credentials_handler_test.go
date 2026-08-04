package server

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMatrixRefreshIdentitySeparatesCredentialKeyFromWorkerAlias(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-cr", Namespace: "agentteams-system"},
		Spec:       v1beta1.WorkerSpec{WorkerName: "matrix-alias"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	handler := NewCredentialsHandler(nil, nil, cl, "agentteams-system")

	credentialName, matrixUsername, err := handler.matrixRefreshIdentity(context.Background(), &auth.CallerIdentity{
		Role:       auth.RoleWorker,
		Username:   "worker-cr",
		WorkerName: "matrix-alias",
	})
	if err != nil {
		t.Fatal(err)
	}
	if credentialName != "worker-cr" || matrixUsername != "matrix-alias" {
		t.Fatalf("refresh identity=(%q,%q), want (worker-cr,matrix-alias)", credentialName, matrixUsername)
	}
}

func TestMatrixRefreshIdentityKeepsManagerIdentity(t *testing.T) {
	handler := NewCredentialsHandler(nil, nil, nil, "agentteams-system")
	credentialName, matrixUsername, err := handler.matrixRefreshIdentity(context.Background(), &auth.CallerIdentity{
		Role:     auth.RoleManager,
		Username: "manager",
	})
	if err != nil {
		t.Fatal(err)
	}
	if credentialName != "manager" || matrixUsername != "manager" {
		t.Fatalf("refresh identity=(%q,%q), want (manager,manager)", credentialName, matrixUsername)
	}
}
