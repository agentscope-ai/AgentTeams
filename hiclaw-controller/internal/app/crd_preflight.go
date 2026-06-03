package app

import (
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

const teamsCRDName = "teams.hiclaw.io"

func (a *App) initCRDPreflight(ctx context.Context) error {
	if !a.cfg.AutoMigrateTeams {
		return nil
	}
	return ensureTeamCRDSupportsWorkerMembers(ctx, a.restCfg)
}

func ensureTeamCRDSupportsWorkerMembers(ctx context.Context, restCfg *rest.Config) error {
	if restCfg == nil {
		return fmt.Errorf("auto team migration requires Kubernetes REST config; set HICLAW_AUTO_MIGRATE_TEAMS=false to disable migration")
	}

	clientset, err := apiextensionsclientset.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create apiextensions client for auto team migration preflight: %w", err)
	}
	crd, err := clientset.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, teamsCRDName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("auto team migration requires CRD %s with spec.workerMembers; upgrade CRDs first or set HICLAW_AUTO_MIGRATE_TEAMS=false: %w", teamsCRDName, err)
	}

	for _, version := range crd.Spec.Versions {
		if version.Name != "v1beta1" {
			continue
		}
		if schemaHasWorkerMembers(version.Schema) {
			return nil
		}
		return fmt.Errorf("auto team migration requires CRD %s/v1beta1 schema to include spec.workerMembers; upgrade CRDs first or set HICLAW_AUTO_MIGRATE_TEAMS=false", teamsCRDName)
	}
	return fmt.Errorf("auto team migration requires CRD %s to serve v1beta1 with spec.workerMembers; upgrade CRDs first or set HICLAW_AUTO_MIGRATE_TEAMS=false", teamsCRDName)
}

func schemaHasWorkerMembers(schema *apiextensionsv1.CustomResourceValidation) bool {
	if schema == nil || schema.OpenAPIV3Schema == nil {
		return false
	}
	specSchema, ok := schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		return false
	}
	_, ok = specSchema.Properties["workerMembers"]
	return ok
}
