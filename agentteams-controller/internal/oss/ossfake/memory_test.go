package ossfake

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

func TestMemoryMirrorCopiesInMemoryPrefix(t *testing.T) {
	m := NewMemory()
	m.PutObject(context.Background(), "teams/t1/skills/skill-a/SKILL.md", []byte("skill"))
	m.PutObject(context.Background(), "teams/t1/skills/skill-a/scripts/run.sh", []byte("run"))

	if err := m.Mirror(context.Background(), "teams/t1/skills/skill-a/", "agents/w1/skills/skill-a/", oss.MirrorOptions{}); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	for _, key := range []string{
		"agents/w1/skills/skill-a/SKILL.md",
		"agents/w1/skills/skill-a/scripts/run.sh",
	} {
		if err := m.Stat(context.Background(), key); err != nil {
			t.Errorf("missing mirrored object %s: %v", key, err)
		}
	}
}

// TestMemoryMirrorRemoveDeletesStale is the exact-copy pin: destination
// objects without a source counterpart must be gone after Mirror with
// Remove=true, and must survive without it.
func TestMemoryMirrorRemoveDeletesStale(t *testing.T) {
	ctx := context.Background()
	seed := func() *Memory {
		m := NewMemory()
		// Source: only SKILL.md (a previously mirrored scripts/old.sh is
		// stale — the skill was updated and the file removed).
		m.PutObject(ctx, "teams/t1/skills/skill-a/SKILL.md", []byte("v2"))
		// Destination: v1 mirror with an extra file.
		m.PutObject(ctx, "agents/w1/skills/skill-a/SKILL.md", []byte("v1"))
		m.PutObject(ctx, "agents/w1/skills/skill-a/scripts/old.sh", []byte("stale"))
		return m
	}

	withRemove := seed()
	if err := withRemove.Mirror(ctx, "teams/t1/skills/skill-a/", "agents/w1/skills/skill-a/", oss.MirrorOptions{Overwrite: true, Remove: true}); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	data, err := withRemove.GetObject(ctx, "agents/w1/skills/skill-a/SKILL.md")
	if err != nil {
		t.Fatalf("GetObject SKILL.md: %v", err)
	}
	if string(data) != "v2" {
		t.Errorf("SKILL.md = %q, want v2 (overwritten)", data)
	}
	if err := withRemove.Stat(ctx, "agents/w1/skills/skill-a/scripts/old.sh"); !isNotFound(err) {
		t.Errorf("stale object survived Mirror with Remove=true (err=%v)", err)
	}

	withoutRemove := seed()
	if err := withoutRemove.Mirror(ctx, "teams/t1/skills/skill-a/", "agents/w1/skills/skill-a/", oss.MirrorOptions{Overwrite: true}); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if err := withoutRemove.Stat(ctx, "agents/w1/skills/skill-a/scripts/old.sh"); err != nil {
		t.Errorf("stale object deleted without Remove=true: %v", err)
	}
}

func TestMemoryMirrorLocalSrcDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("local skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte("local run"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewMemory()
	if err := m.Mirror(ctx, dir+"/", "agents/global/skills/skill-b/", oss.MirrorOptions{Overwrite: true}); err != nil {
		t.Fatalf("mirror local dir: %v", err)
	}
	for key, want := range map[string]string{
		"agents/global/skills/skill-b/SKILL.md":       "local skill",
		"agents/global/skills/skill-b/scripts/run.sh": "local run",
	} {
		data, err := m.GetObject(ctx, key)
		if err != nil {
			t.Errorf("missing %s: %v", key, err)
			continue
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", key, data, want)
		}
	}
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return os.IsNotExist(err)
}

// TestMemoryListObjectsRecursive pins the fake's page contract against the
// production semantics (S3 ListObjectsV2): lexicographic full-key order,
// startAfter as a strictly-greater bound, the limit as a page bound, and
// the cursor as the last key of the page ("" when exhausted). Walking with
// the returned cursors must cover every key exactly once.
func TestMemoryListObjectsRecursive(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	prefix := "humans/alice/files/"
	for _, key := range []string{
		prefix + "a.txt",
		prefix + "docs/b.txt",
		prefix + "docs/nested/c.txt",
		prefix + "x/y/z/deep.txt",
	} {
		if err := m.PutObject(ctx, key, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}

	// limit <= 0 is a contract violation, not an empty page.
	if _, _, err := m.ListObjectsRecursive(ctx, prefix, "", 0); err == nil {
		t.Fatal("limit 0: want an error")
	}

	// First page of 2: the two smallest keys, cursor = the last of them.
	infos, next, err := m.ListObjectsRecursive(ctx, prefix, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Name != "a.txt" || infos[1].Name != "docs/b.txt" {
		t.Fatalf("page 1 = %+v, want a.txt then docs/b.txt (lexicographic)", infos)
	}
	if infos[0].Size != 1 || infos[0].ETag == "" {
		t.Fatalf("page 1 entry = %+v, want size 1 and an etag", infos[0])
	}
	if next != prefix+"docs/b.txt" {
		t.Fatalf("cursor = %q, want the last full key of the page", next)
	}

	// The cursor is strictly greater: the key it names is not repeated.
	infos, next, err = m.ListObjectsRecursive(ctx, prefix, next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Name != "docs/nested/c.txt" || infos[1].Name != "x/y/z/deep.txt" {
		t.Fatalf("page 2 = %+v, want the remaining two keys once each", infos)
	}
	if next != prefix+"x/y/z/deep.txt" {
		t.Fatalf("cursor = %q, want the last full key of the page", next)
	}

	// Exhausted: an empty page and the empty cursor.
	infos, next, err = m.ListObjectsRecursive(ctx, prefix, next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 || next != "" {
		t.Fatalf("page 3 = %+v next=%q, want both empty (walk exhausted)", infos, next)
	}

	// A cursor past the end of the prefix is also an exhausted walk.
	infos, next, err = m.ListObjectsRecursive(ctx, prefix, prefix+"zzz", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 || next != "" {
		t.Fatalf("past-end walk = %+v next=%q, want both empty", infos, next)
	}
}
