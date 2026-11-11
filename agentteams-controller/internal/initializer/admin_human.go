package initializer

import (
	"context"
	"fmt"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Register the existing installation account; its Matrix lifecycle stays
// owned by the initializer, never by ordinary Human provisioning.
func (i *Initializer) ensureAdminHuman(ctx context.Context) error {
	if i.Client == nil || i.Config.AdminUser == "" {
		return nil
	}
	var humans v1beta1.HumanList
	if err := i.Client.List(ctx, &humans, client.InNamespace(i.Config.Namespace)); err != nil {
		return err
	}
	id := "@" + i.Config.AdminUser + ":" + i.Config.MatrixDomain
	for n := range humans.Items {
		h := &humans.Items[n]
		matches := h.Status.MatrixUserID == id || (h.Status.MatrixUserID == "" && h.Spec.EffectiveUsername(h.Name) == i.Config.AdminUser && h.Spec.IdentitySource == nil)
		if !matches {
			if h.Name == i.Config.AdminUser {
				return fmt.Errorf("admin Human name belongs to another Matrix identity")
			}
			continue
		}
		if h.Spec.PermissionLevel != 1 {
			return fmt.Errorf("existing admin identity has a non-admin Human role")
		}
		if h.Labels[v1beta1.LabelSystemAdmin] == "true" {
			return nil
		}
		base := h.DeepCopy()
		if h.Labels == nil {
			h.Labels = map[string]string{}
		}
		h.Labels[v1beta1.LabelSystemAdmin] = "true"
		return i.Client.Patch(ctx, h, client.MergeFrom(base))
	}
	labels := map[string]string{v1beta1.LabelSystemAdmin: "true"}
	if i.Config.ControllerName != "" {
		labels[v1beta1.LabelController] = i.Config.ControllerName
	}
	return i.Client.Create(ctx, &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: i.Config.AdminUser, Namespace: i.Config.Namespace, Labels: labels}, Spec: v1beta1.HumanSpec{Username: i.Config.AdminUser, DisplayName: i.Config.AdminUser, PermissionLevel: 1}})
}
