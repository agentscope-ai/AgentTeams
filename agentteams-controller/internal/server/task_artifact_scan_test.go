package server

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAutomaticArtifactProjectionPreservesPublicationChecks(t *testing.T) {
	data, err := os.ReadFile("testdata/task_artifact_sensitivity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Content   string `json:"content"`
		Sensitive bool   `json:"sensitive"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		if got := artifactSensitive("artifact.txt", "text/plain", []byte(test.Content)); got != test.Sensitive {
			t.Errorf("publication policy mismatch: sensitive=%v want=%v for %q", got, test.Sensitive, test.Content)
		}
	}
	if !artifactSensitive("credentials.json", "application/json", []byte("{}")) {
		t.Fatal("sensitive output name allowed")
	}
}
