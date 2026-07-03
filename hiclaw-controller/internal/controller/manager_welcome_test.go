package controller

import (
	"context"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestReconcileManagerWelcome_ThreadsSoloOperator verifies
// ManagerReconciler.SoloOperator is forwarded into the
// service.ManagerWelcomeRequest passed to SendManagerWelcomeMessage, so the
// provisioner can select the non-interview welcome variant. Backend is left
// nil so managerBackend() short-circuits to the readiness gates, which the
// mock provisioner defaults to "ready" (see MockManagerProvisioner.IsManagerJoinedDM
// / IsManagerLLMAuthReady default true,nil).
func TestReconcileManagerWelcome_ThreadsSoloOperator(t *testing.T) {
	for _, solo := range []bool{true, false} {
		mgr := &v1beta1.Manager{}
		mgr.Name = "default"
		mgr.Status.RoomID = "!room:localhost"
		mgr.Status.WelcomeSent = false

		scheme := runtime.NewScheme()
		if err := v1beta1.AddToScheme(scheme); err != nil {
			t.Fatalf("register scheme: %v", err)
		}
		testClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(mgr).
			WithStatusSubresource(&v1beta1.Manager{}).
			Build()
		mockProv := mocks.NewMockManagerProvisioner()

		r := &ManagerReconciler{
			Client:       testClient,
			Provisioner:  mockProv,
			UserLanguage: "en",
			UserTimezone: "America/New_York",
			SoloOperator: solo,
		}

		scope := &managerScope{manager: mgr}

		if _, err := r.reconcileManagerWelcome(context.Background(), scope); err != nil {
			t.Fatalf("reconcileManagerWelcome (solo=%v): %v", solo, err)
		}

		calls := mockProv.WelcomeCallsSnapshot()
		if len(calls) != 1 {
			t.Fatalf("solo=%v: SendManagerWelcomeMessage called %d times, want 1", solo, len(calls))
		}
		if calls[0].SoloOperator != solo {
			t.Fatalf("solo=%v: ManagerWelcomeRequest.SoloOperator = %v, want %v", solo, calls[0].SoloOperator, solo)
		}
		if calls[0].Language != "en" || calls[0].Timezone != "America/New_York" {
			t.Fatalf("solo=%v: ManagerWelcomeRequest Language/Timezone = %q/%q, want en/America/New_York",
				solo, calls[0].Language, calls[0].Timezone)
		}
	}
}
