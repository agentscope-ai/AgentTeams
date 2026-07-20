package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentTeamsDBPathMigratesLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(filepath.Join(dir, "hiclaw.db")+suffix, []byte(suffix), 0600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := agentTeamsDBPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "agentteams.db")
	if got != want {
		t.Fatalf("agentTeamsDBPath() = %q, want %q", got, want)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(want + suffix); err != nil {
			t.Fatalf("migrated file %q: %v", want+suffix, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "hiclaw.db") + suffix); !os.IsNotExist(err) {
			t.Fatalf("legacy file with suffix %q still exists", suffix)
		}
	}
}
