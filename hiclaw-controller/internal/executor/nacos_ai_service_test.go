package executor

import (
	"archive/zip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractSkillZipExtractsRegularFiles(t *testing.T) {
	zipPath := writeTestSkillZip(t, map[string]zipEntry{
		"demo/SKILL.md": {body: []byte("# demo\n"), mode: 0o644},
	})
	outDir := t.TempDir()

	if err := extractSkillZip(zipPath, outDir); err != nil {
		t.Fatalf("extractSkillZip: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "demo", "SKILL.md"))
	if err != nil {
		t.Fatalf("read extracted SKILL.md: %v", err)
	}
	if string(data) != "# demo\n" {
		t.Fatalf("extracted data = %q", string(data))
	}
}

func TestExtractSkillZipRejectsTraversal(t *testing.T) {
	zipPath := writeTestSkillZip(t, map[string]zipEntry{
		"../outside": {body: []byte("bad"), mode: 0o644},
	})

	err := extractSkillZip(zipPath, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsafe ZIP entry") {
		t.Fatalf("got err %v, want unsafe ZIP entry", err)
	}
}

func TestExtractSkillZipRejectsSymlink(t *testing.T) {
	zipPath := writeTestSkillZip(t, map[string]zipEntry{
		"demo/link": {body: []byte("/etc/passwd"), mode: os.ModeSymlink | 0o777},
	})

	err := extractSkillZip(zipPath, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "symlinks are not allowed") {
		t.Fatalf("got err %v, want symlink rejection", err)
	}
}

func TestGetAgentSpecRejectsTraversalResource(t *testing.T) {
	tests := []struct {
		name         string
		resourceName string
		resourceType string
	}{
		{
			name:         "relative traversal",
			resourceName: "type/../../../etc/passwd",
			resourceType: "",
		},
		{
			name:         "absolute path",
			resourceName: absoluteTraversalResourceName(),
			resourceType: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := nacosAgentSpec{
				Name:    "malicious-agent",
				Content: "{}",
				Resource: map[string]*nacosAgentSpecResource{
					"bad": {
						Name:    tt.resourceName,
						Type:    tt.resourceType,
						Content: "evil",
					},
				},
			}
			specJSON, err := json.Marshal(spec)
			if err != nil {
				t.Fatalf("marshal spec: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resp := nacosV3Response{Code: 0, Data: specJSON}
				body, err := json.Marshal(resp)
				if err != nil {
					t.Fatalf("marshal response: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(body)
			}))
			defer server.Close()

			client := &NacosAIClient{
				serverAddr: strings.TrimPrefix(server.URL, "http://"),
				namespace:  "public",
				httpClient: server.Client(),
				cred:       nacosNoneCredential{},
			}

			outputDir := t.TempDir()
			err = client.GetAgentSpec(context.Background(), "malicious-agent", outputDir, "", "")
			if err == nil || !strings.Contains(err.Error(), "unsafe resource path") {
				t.Fatalf("got err %v, want unsafe resource path error", err)
			}

			specDir := filepath.Join(outputDir, "malicious-agent")
			var written []string
			_ = filepath.Walk(outputDir, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				written = append(written, p)
				return nil
			})
			for _, p := range written {
				if !strings.HasPrefix(p, specDir) {
					t.Fatalf("file written outside specDir: %s", p)
				}
			}

			if _, err := os.Stat(filepath.Join(outputDir, "..", "etc", "passwd")); err == nil {
				t.Fatalf("expected no file written outside outputDir")
			}
		})
	}
}

// absoluteTraversalResourceName returns an OS-appropriate absolute path so
// the "absolute path" test case is meaningful on both POSIX and Windows
// (filepath.IsAbs/VolumeName treat "/etc/passwd" as relative on Windows).
func absoluteTraversalResourceName() string {
	if os.PathSeparator == '\\' {
		return `C:\Windows\System32\config`
	}
	return "/etc/passwd"
}

type zipEntry struct {
	body []byte
	mode os.FileMode
}

func writeTestSkillZip(t *testing.T, entries map[string]zipEntry) string {
	t.Helper()

	zipPath := filepath.Join(t.TempDir(), "skill.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, entry := range entries {
		header := &zip.FileHeader{Name: name}
		header.SetMode(entry.mode)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := w.Write(entry.body); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return zipPath
}
