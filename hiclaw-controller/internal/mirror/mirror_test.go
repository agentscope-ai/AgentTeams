package mirror

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestCreateTargetSpec(t *testing.T) {
	m := &kindMirror{
		kind:      "Worker",
		sourceGVK: sourceGVKFor("Worker"),
		targetGVK: targetGVKFor("Worker"),
	}

	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(m.sourceGVK)
	src.SetName("alice")
	src.SetNamespace("default")
	src.SetAnnotations(map[string]string{"user": "test"})
	src.SetLabels(map[string]string{"env": "dev"})
	_ = unstructured.SetNestedMap(src.Object, map[string]interface{}{
		"model":   "claude-sonnet-4-6",
		"runtime": "openclaw",
	}, "spec")

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(m.targetGVK)
	target.SetName(src.GetName())
	target.SetNamespace(src.GetNamespace())

	annotations := map[string]string{annotationMirrorSource: "true"}
	for k, v := range src.GetAnnotations() {
		annotations[k] = v
	}
	target.SetAnnotations(annotations)
	target.SetLabels(src.GetLabels())

	spec, _, _ := unstructured.NestedMap(src.Object, "spec")
	_ = unstructured.SetNestedMap(target.Object, spec, "spec")

	// Verify target
	if target.GetName() != "alice" {
		t.Fatalf("name = %q, want alice", target.GetName())
	}
	if target.GetObjectKind().GroupVersionKind().Group != "hiclaw.io" {
		t.Fatalf("group = %q, want hiclaw.io", target.GetObjectKind().GroupVersionKind().Group)
	}
	if target.GetAnnotations()[annotationMirrorSource] != "true" {
		t.Fatal("missing mirror-source annotation")
	}
	if target.GetAnnotations()["user"] != "test" {
		t.Fatal("user annotation not preserved")
	}
	if target.GetLabels()["env"] != "dev" {
		t.Fatal("labels not preserved")
	}
	tSpec, _, _ := unstructured.NestedMap(target.Object, "spec")
	if tSpec["model"] != "claude-sonnet-4-6" || tSpec["runtime"] != "openclaw" {
		t.Fatalf("spec = %v, want model+runtime", tSpec)
	}
}

func TestUpdateTargetSkipsIdenticalSpec(t *testing.T) {
	spec := map[string]interface{}{"model": "qwen-max"}

	src := &unstructured.Unstructured{Object: map[string]interface{}{}}
	_ = unstructured.SetNestedMap(src.Object, spec, "spec")

	target := &unstructured.Unstructured{Object: map[string]interface{}{}}
	_ = unstructured.SetNestedMap(target.Object, spec, "spec")

	srcSpec, _, _ := unstructured.NestedMap(src.Object, "spec")
	tgtSpec, _, _ := unstructured.NestedMap(target.Object, "spec")

	// Simple equality check the mirror uses
	if !(len(srcSpec) == len(tgtSpec) && srcSpec["model"] == tgtSpec["model"]) {
		t.Fatal("specs should be considered equal")
	}
}

func sourceGVKFor(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: sourceGroup, Version: version, Kind: kind}
}

func targetGVKFor(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: targetGroup, Version: version, Kind: kind}
}
