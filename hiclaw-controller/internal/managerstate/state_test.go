package managerstate_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiclaw/hiclaw-controller/internal/managerstate"
)

func testStore(t *testing.T) (*managerstate.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	return &managerstate.Store{Path: path}, path
}

func TestInitAndAddFinite(t *testing.T) {
	store, _ := testStore(t)
	out, err := store.Init()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "OK: state.json ready") {
		t.Fatalf("init output: %q", out)
	}
	out, err = store.AddFinite("T1", "Do the thing", "worker1", "!room1:x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "OK: added finite task T1") {
		t.Fatalf("add output: %q", out)
	}
}

func TestAddFiniteDuplicateSkips(t *testing.T) {
	store, _ := testStore(t)
	_, _ = store.AddFinite("T1", "Same", "w1", "r1", "", "")
	out, err := store.AddFinite("T1", "Same", "w1", "r1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if out != "SKIP: task T1 already in active_tasks" {
		t.Fatalf("got %q", out)
	}
}

func TestAddFiniteSuffixCollision(t *testing.T) {
	store, _ := testStore(t)
	_, _ = store.AddFinite("T1", "Original", "w1", "r1", "", "")
	out, err := store.AddFinite("T1", "Different title", "w2", "r2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "T1-2") {
		t.Fatalf("expected suffix id, got %q", out)
	}
}

func TestCompleteAndList(t *testing.T) {
	store, _ := testStore(t)
	_, _ = store.AddFinite("T1", "T", "w1", "r1", "", "")
	out, err := store.Complete("T1")
	if err != nil || !strings.Contains(out, "OK: removed task T1") {
		t.Fatalf("complete: %q err=%v", out, err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "No active tasks.") {
		t.Fatalf("list: %q", list)
	}
}

func TestParseArgsRejectsUnknownFlag(t *testing.T) {
	_, err := managerstate.ParseArgs([]string{"--action", "list", "--bogus", "x"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if !strings.Contains(err.Error(), "Unknown argument: --bogus") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunParsesLegacyAdd(t *testing.T) {
	store, _ := testStore(t)
	args, err := managerstate.ParseArgs([]string{
		"--action", "add", "--type", "finite",
		"--task-id", "T1", "--title", "T", "--assigned-to", "w1", "--room-id", "r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := managerstate.Run(store, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "OK: added finite task T1") {
		t.Fatalf("got %q", out)
	}
}

func TestDefaultPathUsesManagerStateEnv(t *testing.T) {
	t.Setenv("AGENTTEAMS_MANAGER_STATE_FILE", "/tmp/custom/state.json")
	got := managerstate.DefaultPath()
	if got != "/tmp/custom/state.json" {
		t.Fatalf("DefaultPath() = %q, want /tmp/custom/state.json", got)
	}
}

func TestDefaultPathPrefersHOMEOverUserHomeDir(t *testing.T) {
	t.Setenv("AGENTTEAMS_MANAGER_STATE_FILE", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := managerstate.DefaultPath()
	want := filepath.Join(home, "state.json")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}
