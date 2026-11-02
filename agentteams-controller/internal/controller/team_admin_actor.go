package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller/humanidentity"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resolveTeamAdminActor resolves the Matrix identity and access token of
// the TeamAdmin (a Human CR) configured on a team. Shared by:
//
//   - the team reconciler, which creates the team rooms and reconciles
//     their membership AS that admin, and
//   - the human reconciler, which must write room state (power levels)
//     inside TeamAdmin-owned rooms — the homeserver admin is deliberately
//     NOT a member of those rooms, so a read/write under the default
//     admin identity is rejected with M_FORBIDDEN.
//
// Returns (teamAdminActor{}, nil) when the team has no Admin configured.
// The token comes from the identity source's EnsureUserToken (a Matrix
// login when the password is stored), so a TeamAdmin whose password is
// unavailable surfaces an error and the caller degrades (default actor /
// retry next cycle) instead of guessing.
func resolveTeamAdminActor(ctx context.Context, c client.Client, prov service.HumanProvisioner, t *v1beta1.Team) (teamAdminActor, error) {
	if t.Spec.Admin == nil {
		return teamAdminActor{}, nil
	}
	if strings.TrimSpace(t.Spec.Admin.Name) == "" {
		return teamAdminActor{}, fmt.Errorf("team admin human name is required")
	}

	var human v1beta1.Human
	key := client.ObjectKey{Name: t.Spec.Admin.Name, Namespace: t.Namespace}
	if err := c.Get(ctx, key, &human); err != nil {
		return teamAdminActor{}, fmt.Errorf("load team admin human %s/%s: %w", key.Namespace, key.Name, err)
	}

	identity, err := humanidentity.ResolveHuman(&human.Spec, human.Name, humanidentity.Deps{Provisioner: prov})
	if err != nil {
		return teamAdminActor{}, fmt.Errorf("resolve team admin human %s/%s identity: %w", key.Namespace, key.Name, err)
	}
	matrixUserID := human.Status.MatrixUserID
	if matrixUserID == "" {
		if human.Spec.IdentitySource != nil {
			return teamAdminActor{}, fmt.Errorf("team admin human %s/%s uses an external identity source but is not provisioned yet",
				key.Namespace, key.Name)
		}
		matrixUserID = identity.MatrixUserID
	}
	if matrixUserID != identity.MatrixUserID {
		return teamAdminActor{}, fmt.Errorf("team admin human %s/%s status.matrixUserID %q does not match resolved identity %q",
			key.Namespace, key.Name, matrixUserID, identity.MatrixUserID)
	}
	if t.Spec.Admin.MatrixUserID != "" && t.Spec.Admin.MatrixUserID != matrixUserID {
		return teamAdminActor{}, fmt.Errorf("team admin matrixUserId %q does not match Human %s/%s matrix user %q",
			t.Spec.Admin.MatrixUserID, key.Namespace, key.Name, matrixUserID)
	}
	if identity.ManagesInitialPassword && !prov.MatrixAppServiceEnabled() && human.Status.InitialPassword == "" {
		return teamAdminActor{}, fmt.Errorf("team admin human %s/%s has no initial password; cannot obtain Matrix token",
			key.Namespace, key.Name)
	}

	token, err := identity.Source.EnsureUserToken(ctx, &human.Spec, &human.Status, human.Name)
	if err != nil {
		return teamAdminActor{}, fmt.Errorf("login as team admin human %s/%s: %w", key.Namespace, key.Name, err)
	}
	if token == "" {
		return teamAdminActor{}, fmt.Errorf("team admin human %s/%s has no Matrix token", key.Namespace, key.Name)
	}
	return teamAdminActor{
		MatrixUserID: matrixUserID,
		Token:        token,
		Username:     identity.MatrixLocalpart,
	}, nil
}
