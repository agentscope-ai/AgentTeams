package initializer

import (
	"context"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestEnsureAdminHuman(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "reuse"}[existing], func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = v1beta1.AddToScheme(scheme)
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if existing {
				builder = builder.WithObjects(&v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default"}, Spec: v1beta1.HumanSpec{Username: "admin", PermissionLevel: 1, Note: "keep"}})
			}
			c := builder.Build()
			init := &Initializer{Client: c, Config: Config{AdminUser: "admin", Namespace: "default", MatrixDomain: "matrix.local"}}
			for range 2 {
				if err := init.ensureAdminHuman(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var humans v1beta1.HumanList
			if err := c.List(context.Background(), &humans, client.InNamespace("default")); err != nil {
				t.Fatal(err)
			}
			if len(humans.Items) != 1 {
				t.Fatalf("want one Human, got %d", len(humans.Items))
			}
			h := humans.Items[0]
			if h.Labels[v1beta1.LabelSystemAdmin] != "true" || h.Spec.PermissionLevel != 1 || h.Spec.EffectiveUsername(h.Name) != "admin" {
				t.Fatalf("unexpected Human: %+v", h)
			}
			if existing && (h.Name != "owner" || h.Spec.Note != "keep") {
				t.Fatal("existing Human was replaced")
			}
		})
	}
}

func TestAdminHumanDoesNotRebindOrPromote(t *testing.T) {
	for _, spec := range []v1beta1.HumanSpec{{Username: "other", PermissionLevel: 1}, {Username: "admin", PermissionLevel: 2}} {
		scheme := runtime.NewScheme()
		_ = v1beta1.AddToScheme(scheme)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "admin", Namespace: "default"}, Spec: spec}).Build()
		init := &Initializer{Client: c, Config: Config{AdminUser: "admin", Namespace: "default", MatrixDomain: "matrix.local"}}
		if err := init.ensureAdminHuman(context.Background()); err == nil {
			t.Fatal("expected identity/role conflict")
		}
	}
}
