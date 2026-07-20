package fixtures

import (
	"fmt"
	"math/rand"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultNamespace is used for test resources.
const DefaultNamespace = "default"

// NewTestWorker creates a minimal Worker CR for testing.
func NewTestWorker(name string) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
		},
		Spec: v1beta1.WorkerSpec{
			Model:   "gpt-4o",
			Runtime: "openclaw",
		},
	}
}

// NewTestWorkerWithPhase creates a Worker CR with a pre-set status phase.
func NewTestWorkerWithPhase(name, phase string) *v1beta1.Worker {
	w := NewTestWorker(name)
	w.Status.Phase = phase
	return w
}

// NewTestWorkerWithAnnotations creates a Worker CR with annotations.
func NewTestWorkerWithAnnotations(name string, annotations map[string]string) *v1beta1.Worker {
	w := NewTestWorker(name)
	w.Annotations = annotations
	return w
}

// UniqueName returns a unique test name with a random suffix.
func UniqueName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, randString(6))
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz0123456789")

func randString(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}
