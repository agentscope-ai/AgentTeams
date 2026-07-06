package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/accessresolver"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/credentials"
	"github.com/hiclaw/hiclaw-controller/internal/credprovider"
	"k8s.io/apimachinery/pkg/runtime"
)

type fakeCredentialProvider struct {
	lastReq credprovider.IssueRequest
}

func (f *fakeCredentialProvider) Issue(_ context.Context, req credprovider.IssueRequest) (*credprovider.IssueResponse, error) {
	f.lastReq = req
	return &credprovider.IssueResponse{AccessKeyID: "ak", AccessKeySecret: "sk", ExpiresInSec: 900}, nil
}

func (f *fakeCredentialProvider) GetKubeconfig(_ context.Context, _ string) (*credprovider.KubeconfigResponse, error) {
	return nil, errors.New("not implemented")
}

func newCredentialHandlerTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestCredentialsHandlerRefreshSTSAgentIdentityDataPurpose(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "cred-bot"
	worker.Namespace = "hiclaw"
	worker.Spec.AgentIdentity = &v1beta1.AgentIdentitySpec{WorkloadIdentityName: "wi-cred-bot"}
	worker.Spec.CredentialBindings = []v1beta1.CredentialBinding{{
		CredentialRef: v1beta1.CredentialRef{
			TokenVaultName:               "default",
			APIKeyCredentialProviderName: "GITHUB_TOKEN",
		},
	}}
	provider := &fakeCredentialProvider{}
	resolver := accessresolver.New(newCredentialHandlerTestClient(t, worker), "hiclaw", "test-bucket", "", auth.DefaultResourcePrefix)
	sts := credentials.NewSTSService(credentials.STSConfig{OSSBucket: "test-bucket", OSSEndpoint: "oss.example.com"}, resolver, provider)
	handler := NewCredentialsHandler(sts, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/credentials/sts?purpose=agentidentitydata", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.CallerKeyForTest(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "cred-bot", WorkerName: "cred-bot",
	}))
	rr := httptest.NewRecorder()
	handler.RefreshSTS(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(provider.lastReq.Entries) != 1 || provider.lastReq.Entries[0].Service != credprovider.ServiceAgentIdentityData {
		t.Fatalf("issue entries=%#v", provider.lastReq.Entries)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if _, ok := body["oss_bucket"]; ok {
		t.Fatalf("agentidentitydata response must not include oss_bucket: %s", rr.Body.String())
	}
	if _, ok := body["oss_endpoint"]; ok {
		t.Fatalf("agentidentitydata response must not include oss_endpoint: %s", rr.Body.String())
	}
}
