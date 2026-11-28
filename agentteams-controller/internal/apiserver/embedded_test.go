package apiserver

import (
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestPackagedAgentApprovalSchemas(t *testing.T) {
	for _, dir := range []string{"../../config/crd", "../../../helm/agentteams/crds"} {
		for _, resource := range []string{"workers", "managers"} {
			name := filepath.Join(dir, resource+".agentteams.io.yaml")
			t.Run(name, func(t *testing.T) {
				data, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				var crd apiextensionsv1.CustomResourceDefinition
				if err := yaml.UnmarshalStrict(data, &crd); err != nil {
					t.Fatal(err)
				}
				for _, version := range crd.Spec.Versions {
					if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
						t.Fatal("missing resource schema")
					}
					fields := version.Schema.OpenAPIV3Schema.Properties["spec"].Properties
					approval := fields["approvalMode"]
					if approval.Type != "string" || len(approval.Enum) != 2 ||
						string(approval.Enum[0].Raw) != `"ask"` || string(approval.Enum[1].Raw) != `"allow"` {
						t.Fatalf("invalid approvalMode schema: %+v", approval)
					}
					if fields["mcpServers"].Type != "array" {
						t.Fatal("MCP assignments must remain a spec field")
					}
				}
			})
		}
	}
}
