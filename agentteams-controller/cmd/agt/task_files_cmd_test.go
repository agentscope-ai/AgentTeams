package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTaskFileDownloadVerifiesBeforePublishingLocalPath(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "corrupt"}[corrupt], func(t *testing.T) {
			requests := 0
			data := []byte("verified report\n")
			digest := sha256.Sum256(data)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/api/v1/tasks/t1/files" || r.URL.Query().Get("name") != "report.md" || r.URL.Query().Get("version") != "v1" {
					t.Errorf("unexpected resource: %s", r.URL)
				}
				w.Header().Set("X-Content-SHA256", hex.EncodeToString(digest[:]))
				w.Header().Set("ETag", `"v1"`)
				if corrupt {
					_, _ = w.Write([]byte("wrong file"))
				} else {
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
			target := filepath.Join(t.TempDir(), "report.md")
			cmd := newRootCommand("agt")
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetArgs([]string{"task", "files", "get", "t1", "--name", "report.md", "--version", "v1", "--output", target})
			err := cmd.Execute()
			if requests != 1 {
				t.Fatalf("requests=%d, want one", requests)
			}
			if corrupt {
				if err == nil {
					t.Fatal("corrupt download accepted")
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("failed download exposed at target: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("file=%q err=%v", got, err)
			}
			var result struct {
				LocalPath string `json:"localPath"`
				SHA256    string `json:"sha256"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.LocalPath != target || result.SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("result=%s err=%v", output.String(), err)
			}
		})
	}
}

func TestTaskFileReadChecksNameWithImmutableArtifactIdentity(t *testing.T) {
	for _, name := range []string{"app.py", "wrong.py"} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				query := r.URL.Query()
				if query.Get("artifactId") != "artifact-1" || query.Get("name") != name || query.Get("version") != "v1" {
					t.Errorf("lost identity fields: %s", r.URL)
				}
				if name != "app.py" {
					http.Error(w, "artifact name mismatch", http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(`{"taskId":"t1","name":"app.py","content":"print(1)","version":"v1"}`))
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
			cmd := newRootCommand("agt")
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"task", "files", "read", "t1", "--artifact", "artifact-1", "--name", name, "--version", "v1"})
			err := cmd.Execute()
			if requests != 1 || (err != nil) != (name != "app.py") {
				t.Fatalf("requests=%d err=%v output=%s", requests, err, out.String())
			}
		})
	}
}
