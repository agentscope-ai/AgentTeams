package controller

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func setCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
}

func conditionIsTrue(conditions []metav1.Condition, conditionType string) bool {
	cond := meta.FindStatusCondition(conditions, conditionType)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

func setReadyFromDependencies(conditions *[]metav1.Condition, observedGeneration int64, deps ...string) {
	for _, dep := range deps {
		if !conditionIsTrue(*conditions, dep) {
			setCondition(conditions, v1beta1.ConditionReady, metav1.ConditionFalse, "Reconciling", "Waiting for "+dep+".", observedGeneration)
			return
		}
	}
	setCondition(conditions, v1beta1.ConditionReady, metav1.ConditionTrue, "Ready", "All reconcile steps are ready.", observedGeneration)
}
