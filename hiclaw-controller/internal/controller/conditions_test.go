package controller

import (
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetConditionPreservesLastTransitionTimeWhenStatusUnchanged(t *testing.T) {
	initial := metav1.NewTime(time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC))
	conditions := []metav1.Condition{{
		Type:               v1beta1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "ConfigFailed",
		Message:            "config failed",
		ObservedGeneration: 1,
		LastTransitionTime: initial,
	}}

	setCondition(&conditions, v1beta1.ConditionReady, metav1.ConditionFalse, "StillFailing", "still failing", 2)

	cond := meta.FindStatusCondition(conditions, v1beta1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if !cond.LastTransitionTime.Equal(&initial) {
		t.Fatalf("LastTransitionTime changed on same-status update: got %s want %s",
			cond.LastTransitionTime.Format(time.RFC3339), initial.Format(time.RFC3339))
	}
	if cond.Reason != "StillFailing" || cond.Message != "still failing" || cond.ObservedGeneration != 2 {
		t.Fatalf("condition fields not updated: %+v", cond)
	}
}

func TestSetConditionChangesLastTransitionTimeWhenStatusChanges(t *testing.T) {
	initial := metav1.NewTime(time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC))
	conditions := []metav1.Condition{{
		Type:               v1beta1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "ConfigFailed",
		Message:            "config failed",
		ObservedGeneration: 1,
		LastTransitionTime: initial,
	}}

	setCondition(&conditions, v1beta1.ConditionReady, metav1.ConditionTrue, "Ready", "ready", 2)

	cond := meta.FindStatusCondition(conditions, v1beta1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if !cond.LastTransitionTime.After(initial.Time) {
		t.Fatalf("LastTransitionTime did not advance on status transition: got %s initial %s",
			cond.LastTransitionTime.Format(time.RFC3339), initial.Format(time.RFC3339))
	}
}
