package app

import (
	"reflect"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

func TestInClusterCacheByObjectScopesExecutionSandboxes(t *testing.T) {
	byObject := inClusterCacheByObject("ctl-a")
	var sandboxCache cache.ByObject
	found := false
	for object, config := range byObject {
		if reflect.TypeOf(object) == reflect.TypeOf(&v1beta1.ExecutionSandbox{}) {
			sandboxCache = config
			found = true
			break
		}
	}
	if !found {
		t.Fatal("ExecutionSandbox is absent from the in-cluster controller cache scope")
	}
	if sandboxCache.Label == nil {
		t.Fatal("ExecutionSandbox cache selector is nil")
	}
	if !sandboxCache.Label.Matches(labels.Set{v1beta1.LabelController: "ctl-a"}) {
		t.Fatal("ExecutionSandbox cache rejects the owning controller label")
	}
	for _, foreign := range []labels.Set{
		{},
		{v1beta1.LabelController: "ctl-b"},
	} {
		if sandboxCache.Label.Matches(foreign) {
			t.Fatalf("ExecutionSandbox cache accepts foreign labels %#v", foreign)
		}
	}

	// Keep Service and NetworkPolicy caches global because unrelated platform
	// infrastructure shares those types; their owned watches are predicate-scoped.
	for object := range byObject {
		switch object.(type) {
		case *corev1.Service, *networkingv1.NetworkPolicy:
			t.Fatalf("shared child type %T was globally cache-filtered", object)
		}
	}
}
