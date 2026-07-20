package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/accessresolver"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credprovider"
)

type fakeProvider struct {
	lastReq credprovider.IssueRequest
	resp    *credprovider.IssueResponse
	err     error
}

func (f *fakeProvider) Issue(_ context.Context, req credprovider.IssueRequest) (*credprovider.IssueResponse, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// GetKubeconfig is a stub to satisfy the credprovider.Client interface; the
// credentials package tests do not exercise the kubeconfig path.
func (f *fakeProvider) GetKubeconfig(_ context.Context, _ string) (*credprovider.KubeconfigResponse, error) {
	return nil, errors.New("not implemented")
}

const ns = "hiclaw"

func newFakeK8sClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func rawJSON(t *testing.T, v any) *apiextensionsv1.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &apiextensionsv1.JSON{Raw: b}
}

func TestIssueForCaller_WorkerDefaultEntries(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "alice"
	worker.Namespace = ns
	c := newFakeK8sClient(t, worker)
	resolver := accessresolver.New(c, ns, "test-bucket", "", auth.DefaultResourcePrefix)

	fake := &fakeProvider{resp: &credprovider.IssueResponse{
		AccessKeyID:     "STS.test-ak",
		AccessKeySecret: "test-sk",
		SecurityToken:   "test-token",
		ExpiresInSec:    3600,
	}}
	svc := NewSTSService(STSConfig{
		OSSBucket:   "test-bucket",
		OSSEndpoint: "oss-cn-hangzhou.aliyuncs.com",
	}, resolver, fake)

	tok, err := svc.IssueForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "alice", WorkerName: "alice",
	})
	if err != nil {
		t.Fatalf("IssueForCaller: %v", err)
	}
	if tok.AccessKeyID != "STS.test-ak" || tok.OSSBucket != "test-bucket" {
		t.Fatalf("unexpected token: %+v", tok)
	}
	if tok.OSSEndpoint != "oss-cn-hangzhou.aliyuncs.com" {
		t.Fatalf("OSSEndpoint = %q, want %q (must come from STSConfig, not provider response)",
			tok.OSSEndpoint, "oss-cn-hangzhou.aliyuncs.com")
	}

	if _, err := uuid.Parse(fake.lastReq.SessionName); err != nil {
		t.Fatalf("session = %q, want UUID: %v", fake.lastReq.SessionName, err)
	}
	if len(fake.lastReq.SessionName) != 36 {
		t.Fatalf("session length = %d, want 36", len(fake.lastReq.SessionName))
	}
	// Standalone workers now default to a single object-storage entry
	// covering agents/<name>/ + agents/<name>/* + shared/ + shared/*
	// (both RW), mirroring the
	// embedded MinIO policy.
	if len(fake.lastReq.Entries) != 1 {
		t.Fatalf("expected 1 default entry, got %d", len(fake.lastReq.Entries))
	}
	got := fake.lastReq.Entries[0]
	if got.Scope.Bucket != "test-bucket" {
		t.Fatalf("bucket not resolved in %+v", got.Scope)
	}
	wantPrefixes := map[string]bool{
		"agents/alice/":  false,
		"agents/alice/*": false,
		"shared/":        false,
		"shared/*":       false,
	}
	for _, p := range got.Scope.Prefixes {
		if _, ok := wantPrefixes[p]; ok {
			wantPrefixes[p] = true
		}
	}
	for p, seen := range wantPrefixes {
		if !seen {
			t.Fatalf("missing prefix %q in %+v", p, got.Scope.Prefixes)
		}
	}
	wantPerms := map[string]bool{"read": false, "write": false, "list": false, "delete": false}
	for _, perm := range got.Permissions {
		if _, ok := wantPerms[perm]; ok {
			wantPerms[perm] = true
		}
	}
	for perm, seen := range wantPerms {
		if !seen {
			t.Fatalf("missing permission %q in %+v", perm, got.Permissions)
		}
	}
}

func TestIssueForCaller_WorkerCustomEntries(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "bob"
	worker.Namespace = ns
	worker.Spec.AccessEntries = []v1beta1.AccessEntry{
		{
			Service:     credprovider.ServiceObjectStorage,
			Permissions: []string{"read"},
			Scope: rawJSON(t, map[string]any{
				"bucketRef": "workspace",
				"prefixes":  []string{"readonly/*"},
			}),
		},
	}
	c := newFakeK8sClient(t, worker)
	resolver := accessresolver.New(c, ns, "test-bucket", "", auth.DefaultResourcePrefix)

	fake := &fakeProvider{resp: &credprovider.IssueResponse{
		AccessKeyID: "ak", AccessKeySecret: "sk", ExpiresInSec: 900,
	}}
	svc := NewSTSService(STSConfig{OSSBucket: "test-bucket"}, resolver, fake)

	if _, err := svc.IssueForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "bob", WorkerName: "bob",
	}); err != nil {
		t.Fatalf("IssueForCaller: %v", err)
	}
	if len(fake.lastReq.Entries) != 1 || fake.lastReq.Entries[0].Scope.Prefixes[0] != "readonly/*" {
		t.Fatalf("custom entries not propagated: %+v", fake.lastReq.Entries)
	}
	if perms := fake.lastReq.Entries[0].Permissions; len(perms) != 1 || perms[0] != "read" {
		t.Fatalf("permissions not propagated: %+v", perms)
	}
}

func TestIssueForCaller_ProviderError(t *testing.T) {
	worker := &v1beta1.Worker{}
	worker.Name = "alice"
	worker.Namespace = ns
	c := newFakeK8sClient(t, worker)
	resolver := accessresolver.New(c, ns, "b", "", auth.DefaultResourcePrefix)
	svc := NewSTSService(STSConfig{OSSBucket: "b"}, resolver, &fakeProvider{err: errors.New("boom")})

	if _, err := svc.IssueForCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleWorker, Username: "alice", WorkerName: "alice",
	}); err == nil {
		t.Fatal("expected provider error to propagate")
	}
}

func TestConfigured(t *testing.T) {
	if NewSTSService(STSConfig{}, nil, nil).Configured() {
		t.Fatal("empty service should not be configured")
	}
	if NewSTSService(STSConfig{}, accessresolver.New(newFakeK8sClient(t), ns, "b", "", auth.DefaultResourcePrefix), nil).Configured() {
		t.Fatal("service without provider should not be configured")
	}
	if NewSTSService(STSConfig{}, nil, &fakeProvider{}).Configured() {
		t.Fatal("service without resolver should not be configured")
	}
}
