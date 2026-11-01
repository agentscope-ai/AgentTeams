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

func TestParseMCLsJSONStripsUnexpectedSlashFromFile(t *testing.T) {
	infos, err := parseMCLsJSON(`{"type":"file","size":2,"title":"inner.txt/","modified":"2026-11-01T02:33:00Z"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "inner.txt" {
		t.Fatalf("infos = %+v, want the file name without a trailing slash", infos)
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
