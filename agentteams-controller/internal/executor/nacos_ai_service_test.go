package executor

import (
	"archive/zip"
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
