package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillPublishUsesControllerAndMountedIdentity(t *testing.T) {
	archive := []byte("skill zip bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/skills" || r.Header.Get("Authorization") != "Bearer manager-token" {
			t.Errorf("incorrect publication route or identity: %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if r.FormValue("scope") != "deployment" || !bytes.Equal(data, archive) {
			t.Error("publication must preserve the archive and use the deployment catalog")
		}
		_, _ = w.Write([]byte(`{"name":"travel-card","scope":"deployment","files":1,"scan":{"status":"pass"}}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "")
	token := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(token, []byte("manager-token\n"), 0600)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN_FILE", token)
	file := filepath.Join(t.TempDir(), "travel-card.zip")
	_ = os.WriteFile(file, archive, 0600)
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"skill", "publish", "-f", file})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"scope": "deployment"`) {
		t.Fatal(output.String())
	}
}

func TestSkillPublishReportsControllerRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"skill scan blocked"}`, http.StatusUnprocessableEntity)
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	file := filepath.Join(t.TempDir(), "skill.zip")
	_ = os.WriteFile(file, []byte("zip"), 0600)
	cmd := newRootCommand("agt")
	cmd.SetArgs([]string{"skill", "publish", "-f", file})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "skill scan blocked") {
		t.Fatalf("expected rejection, got %v", err)
	}
}
