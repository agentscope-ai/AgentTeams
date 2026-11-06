package oss

import (
	"strings"
	"testing"
)

// Real `mc ls --json` shape (mc RELEASE.2025-08-13): NDJSON, one object per
// line, the full object name in `title` (spaces intact), common prefixes as
// "dir" entries, RFC3339 `modified` with fractional seconds.
const mcLsSpaceFileLine = `{"type":"file","size":6,"title":"meeting notes.txt","modified":"2026-11-01T02:33:00.000Z","etag":"d41d8cd98f00b204e9800998ecf8427e"}`

func TestParseMCLsJSONPreservesSpacesInNames(t *testing.T) {
	infos, err := parseMCLsJSON(mcLsSpaceFileLine+"\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(infos), infos)
	}
	if infos[0].Name != "meeting notes.txt" {
		t.Fatalf("Name = %q, want the full name with the space", infos[0].Name)
	}
	if infos[0].UpdatedAt != "2026-11-01T02:33:00Z" {
		t.Fatalf("UpdatedAt = %q, want RFC3339 UTC", infos[0].UpdatedAt)
	}
}

func TestParseMCLsJSONNormalizesDirectoryTrailingSlash(t *testing.T) {
	lines := []string{
		`{"type":"dir","size":0,"title":"my dir/","modified":"2026-11-01T02:33:00Z"}`,
		`{"type":"dir","size":0,"title":"my dir","modified":"2026-11-01T02:33:00Z"}`,
	}
	for _, line := range lines {
		infos, err := parseMCLsJSON(line, "")
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if len(infos) != 1 || infos[0].Name != "my dir/" {
			t.Fatalf("%s: infos = %+v, want the single normalized directory name", line, infos)
		}
	}
}

// A key ending in "/" is a directory, whatever `type` says. MinIO and RustFS
// 1.0.4 both report directories as {"type":"file","size":0} with a trailing
// slash on the key (CI job 112062391396 listed "my dir" as a zero-byte file and
// the space-bearing directory vanished from the listing), so the slash - not
// the type field - decides.
func TestParseMCLsJSONTrailingSlashIsADirectory(t *testing.T) {
	infos, err := parseMCLsJSON(`{"type":"file","size":0,"key":"my dir/","modified":"2026-11-01T02:33:00Z"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "my dir/" {
		t.Fatalf("infos = %+v, want the directory name with its trailing slash", infos)
	}
}

func TestParseMCLsJSONEmptyOutputIsEmpty(t *testing.T) {
	for _, out := range []string{"", "\n"} {
		infos, err := parseMCLsJSON(out, "")
		if err != nil {
			t.Fatalf("%q: %v", out, err)
		}
		if len(infos) != 0 {
			t.Fatalf("%q: len = %d, want 0", out, len(infos))
		}
	}
}

func TestParseMCLsJSONMalformedLineFailsLoudly(t *testing.T) {
	for _, out := range []string{
		mcLsSpaceFileLine + "\nnot json",
		"not json",
		`{"type":"file",}`,
	} {
		if _, err := parseMCLsJSON(out, ""); err == nil {
			t.Fatalf("%q: expected a parse error, got none", out)
		}
	}
}

func TestParseMCLsJSONBadModifiedDegradesToEmpty(t *testing.T) {
	infos, err := parseMCLsJSON(`{"type":"file","size":6,"title":"a.txt","modified":"not-a-time"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].UpdatedAt != "" {
		t.Fatalf("infos = %+v, want UpdatedAt empty on an unparseable timestamp", infos)
	}
}

// TestParseMCLsJSONCarriesSizeAndETag pins which listing entries carry the
// object's own size/etag: direct files do; directory entries — real and
// the ones synthesized from nested keys — always report 0/"".
func TestParseMCLsJSONCarriesSizeAndETag(t *testing.T) {
	out := strings.Join([]string{
		`{"type":"file","size":5,"key":"humans/alice/files/a.txt","title":"humans/alice/files/a.txt","modified":"2026-11-01T02:33:00.000Z","etag":"d41d8cd98f00b204e9800998ecf8427e"}`,
		`{"type":"dir","size":0,"title":"my dir/","modified":"2026-11-01T02:33:00Z"}`,
		`{"type":"file","size":42,"key":"humans/alice/files/docs/b.txt","title":"humans/alice/files/docs/b.txt","modified":"2026-11-01T02:33:00Z","etag":"abc123"}`,
	}, "\n")
	infos, err := parseMCLsJSON(out, "humans/alice/files/")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(infos), infos)
	}
	if infos[0].Name != "a.txt" || infos[0].Size != 5 || infos[0].ETag != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("direct file entry = %+v, want its own size and etag", infos[0])
	}
	if infos[1].Name != "my dir/" || infos[1].Size != 0 || infos[1].ETag != "" {
		t.Fatalf("directory entry = %+v, want 0/\"\"", infos[1])
	}
	if infos[2].Name != "docs/" || infos[2].Size != 0 || infos[2].ETag != "" {
		t.Fatalf("synthesized directory entry = %+v, must not inherit the nested file's numbers", infos[2])
	}
}

func TestParseMCLsJSONKeepsEntryOrder(t *testing.T) {
	out := strings.Join([]string{
		mcLsSpaceFileLine,
		`{"type":"dir","size":0,"title":"my dir/","modified":"2026-11-01T02:34:00Z"}`,
		`{"type":"file","size":2,"title":"plain.txt","modified":"2026-11-01T02:35:00Z"}`,
	}, "\n")
	infos, err := parseMCLsJSON(out, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"meeting notes.txt", "my dir/", "plain.txt"}
	if len(infos) != len(want) {
		t.Fatalf("len = %d, want %d: %+v", len(infos), len(want), infos)
	}
	for i, w := range want {
		if infos[i].Name != w {
			t.Fatalf("infos[%d].Name = %q, want %q (entry order must be preserved)", i, infos[i].Name, w)
		}
	}
}
