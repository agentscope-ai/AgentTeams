package app

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestSchemaHasWorkerMembers(t *testing.T) {
	withWorkerMembers := &apiextensionsv1.CustomResourceValidation{
		OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"spec": {
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"workerMembers": {Type: "array"},
					},
				},
			},
		},
	}
	if !schemaHasWorkerMembers(withWorkerMembers) {
		t.Fatalf("schemaHasWorkerMembers returned false for schema containing spec.workerMembers")
	}

	withoutWorkerMembers := &apiextensionsv1.CustomResourceValidation{
		OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"spec": {
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"leader": {Type: "object"},
					},
				},
			},
		},
	}
	if schemaHasWorkerMembers(withoutWorkerMembers) {
		t.Fatalf("schemaHasWorkerMembers returned true for schema without spec.workerMembers")
	}
}
