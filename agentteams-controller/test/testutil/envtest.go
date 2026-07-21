package testutil

import (
	"path/filepath"
	"runtime"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Scheme returns a runtime.Scheme with all types needed for integration tests.
func Scheme() *apiruntime.Scheme {
	s := apiruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1beta1.AddToScheme(s))
	return s
}

// CRDPath returns the absolute path to the CRD directory.
func CRDPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "config", "crd")
}

// NewTestEnv creates a configured envtest.Environment with AgentTeams CRDs loaded.
func NewTestEnv() *envtest.Environment {
	return &envtest.Environment{
		CRDDirectoryPaths:     []string{CRDPath()},
		ErrorIfCRDPathMissing: true,
	}
}
