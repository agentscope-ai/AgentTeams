package oss

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestListObjectsDetailed_* covers the mc capability probe and the fallback
// path for mc binaries that predate `mc ls --json` (the maintainer's P1-A
// requirement: probe once, cache the definitive answer, and degrade to a
// space-preserving structured listing — never the space-splitting
// human-readable form). The fake mc below mirrors the three real-world
// behaviours:
//
//   - engine=new:   `ls --json` works (mc >= RELEASE.2025-08-13, the pinned
//     deployment version);
//   - engine=old:   `ls --json` is rejected with an unknown-flag error and
//     `find` prints one full path per line;
//   - engine=flaky: `ls --json` fails once with a transient connection
//     error (the probe), then works.
//
// Both listing paths must yield the identical one-level view of the
// space-bearing seed objects, so a fallback regression is caught by the same
// assertions as the JSON path.

const fakeListingMCScript = `#!/bin/sh
# fake mc for the listing-format probe tests. Logs each invocation to
# $FAKE_MC_LOG and serves the object keys stored (bucket-relative, one per
# line) in $FAKE_MC_STATE/objects.txt.
state="$FAKE_MC_STATE"; log="$FAKE_MC_LOG"
mkdir -p "$state"
printf '%s\n' "$*" >> "$log"

sub1="$1"; sub2="$2"; sub3="$3"
case "$sub1" in
alias)
  exit 0
  ;;
ls)
  if [ "$sub2" != "--json" ]; then
    echo "fake mc: unhandled: $*" >&2
    exit 2
  fi
  if [ "$FAKE_MC_ENGINE" = "old" ]; then
    echo "mc: <ERROR> unknown flag: --json." >&2
    exit 1
  fi
  if [ "$FAKE_MC_ENGINE" = "flaky" ] && [ "$sub3" = "agentteams/bucket" ] && [ -f "$state/probe-fail" ]; then
    rm -f "$state/probe-fail"
    echo "mc: <ERROR> connection refused." >&2
    exit 1
  fi
  rest="${sub3#agentteams/bucket/}"
  # production fullPath keeps the prefix's trailing slash; drop it here or
  # the p="$rest/" filter below double-slashes and matches nothing.
  rest="${rest%/}"
  if [ "$sub3" = "agentteams/bucket" ]; then rest=""; fi
  if [ -n "$rest" ]; then
    awk -v p="$rest/" 'index($0, p) == 1 { print substr($0, length(p) + 1) }' "$state/objects.txt"
  else
    cat "$state/objects.txt" 2>/dev/null
  fi | awk '
    {
      n = split($0, a, "/")
      seg = a[1]
      if (n > 1) {
        if (!(seg in seen)) {
          seen[seg] = 1
          printf "{\"type\":\"dir\",\"size\":0,\"title\":\"%s/\",\"modified\":\"2026-11-01T02:33:00.000Z\"}\n", seg
        }
      } else {
        printf "{\"type\":\"file\",\"size\":6,\"title\":\"%s\",\"modified\":\"2026-11-01T02:33:00.000Z\"}\n", $0
      }
    }'
  exit 0
  ;;
find)
  # argv: find <path> — the path is $2 (unlike 'ls --json <path>').
  rest="${sub2#agentteams/bucket/}"
  # production fullPath keeps the prefix's trailing slash (see the ls branch).
  rest="${rest%/}"
  if [ "$sub2" = "agentteams/bucket" ]; then rest=""; fi
  if [ -n "$rest" ]; then
    awk -v p="$rest/" 'index($0, p) == 1 { print "agentteams/bucket/" $0 }' "$state/objects.txt"
  else
    awk '{ print "agentteams/bucket/" $0 }' "$state/objects.txt" 2>/dev/null
  fi
  exit 0
  ;;
esac
echo "fake mc: unhandled: $*" >&2
exit 2
`

// listingProbeObjects are the bucket-relative keys the fake serves: a
// space-bearing file, a plain file, and a space-bearing directory with a
// nested file — the maintainer's repro shapes.
const listingProbeObjects = "human/alice/files/meeting notes.txt\n" +
	"human/alice/files/plain.txt\n" +
	"human/alice/files/my dir/inner.txt\n"

// listingProbeWant is the one-level view of listingProbeObjects under
// human/alice/files/: full names with spaces, the synthesized directory
// entry, first-seen order. Both listing paths must produce exactly this.
var listingProbeWant = []string{"meeting notes.txt", "plain.txt", "my dir/"}

// newListingProbeClient builds a MinIOClient whose MCBinary is a generated
// fake mc seeded with listingProbeObjects. For engine=flaky it also arms
// the one-shot probe failure. Returns the client and the call-log path.
func newListingProbeClient(t *testing.T, engine string) (*MinIOClient, string) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "mc-fake")
	if err := os.WriteFile(fake, []byte(fakeListingMCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "objects.txt"), []byte(listingProbeObjects), 0o644); err != nil {
		t.Fatal(err)
	}
	if engine == "flaky" {
		if err := os.WriteFile(filepath.Join(state, "probe-fail"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKE_MC_STATE", state)
	t.Setenv("FAKE_MC_LOG", filepath.Join(dir, "calls.log"))
	t.Setenv("FAKE_MC_ENGINE", engine)
	client := NewMinIOClient(Config{
		MCBinary:      fake,
		Alias:         "agentteams",
		Endpoint:      "http://rustfs:9000",
		AccessKey:     "ak",
		SecretKey:     "sk",
		Bucket:        "bucket",
		StoragePrefix: "agentteams/bucket",
	})
	return client, filepath.Join(dir, "calls.log")
}

func listingNames(t *testing.T, infos []ObjectInfo) []string {
	t.Helper()
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	return names
}

func countExact(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

func TestListObjectsDetailed_ProbesLSJSONOnceAndCaches(t *testing.T) {
	client, log := newListingProbeClient(t, "new")
	ctx := context.Background()

	for call := 1; call <= 2; call++ {
		infos, err := client.ListObjectsDetailed(ctx, "human/alice/files/")
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		if got := listingNames(t, infos); !reflect.DeepEqual(got, listingProbeWant) {
			t.Fatalf("call %d: names = %v, want %v", call, got, listingProbeWant)
		}
	}

	calls := readCalls(t, log)
	// The capability probe ran exactly once despite two listings, and both
	// listings used the JSON form — the fallback was never needed.
	if n := countExact(calls, "ls --json agentteams/bucket"); n != 1 {
		t.Fatalf("probe invocations = %d, want 1 (result cached): %v", n, calls)
	}
	// fullPath keeps the prefix's trailing slash, so the listing path is
	// logged verbatim with it.
	if n := countExact(calls, "ls --json agentteams/bucket/human/alice/files/"); n != 2 {
		t.Fatalf("json listings = %d, want 2: %v", n, calls)
	}
	if n := countExact(calls, "find agentteams/bucket/human/alice/files"); n != 0 {
		t.Fatalf("find fallback invocations = %d, want 0: %v", n, calls)
	}
}

func TestListObjectsDetailed_OldMCFallsBackToFind(t *testing.T) {
	client, log := newListingProbeClient(t, "old")
	ctx := context.Background()

	for call := 1; call <= 2; call++ {
		infos, err := client.ListObjectsDetailed(ctx, "human/alice/files/")
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		if got := listingNames(t, infos); !reflect.DeepEqual(got, listingProbeWant) {
			t.Fatalf("call %d: fallback names = %v, want %v (spaces must survive the fallback)", call, got, listingProbeWant)
		}
	}

	calls := readCalls(t, log)
	// The definitive unknown-flag probe failure is cached: one probe, then
	// the find fallback for every listing — never the space-splitting
	// human-readable ls.
	if n := countExact(calls, "ls --json agentteams/bucket"); n != 1 {
		t.Fatalf("probe invocations = %d, want 1 (negative answer cached): %v", n, calls)
	}
	if n := countExact(calls, "find agentteams/bucket/human/alice/files/"); n != 2 {
		t.Fatalf("find listings = %d, want 2: %v", n, calls)
	}
	if n := countExact(calls, "ls agentteams/bucket/human/alice/files"); n != 0 {
		t.Fatalf("human-readable ls invocations = %d, want 0: %v", n, calls)
	}
}

func TestListObjectsDetailed_TransientProbeFailureIsNotCached(t *testing.T) {
	client, log := newListingProbeClient(t, "flaky")
	ctx := context.Background()

	// First listing: the probe fails transiently (connection refused, not
	// unknown-flag), so this listing degrades to the find fallback and must
	// still preserve the full names.
	infos, err := client.ListObjectsDetailed(ctx, "human/alice/files/")
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if got := listingNames(t, infos); !reflect.DeepEqual(got, listingProbeWant) {
		t.Fatalf("call 1: names = %v, want %v", got, listingProbeWant)
	}
	// Second listing: the probe is retried (a transient failure is not
	// cached), succeeds, and the JSON path is used from then on.
	infos, err = client.ListObjectsDetailed(ctx, "human/alice/files/")
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if got := listingNames(t, infos); !reflect.DeepEqual(got, listingProbeWant) {
		t.Fatalf("call 2: names = %v, want %v", got, listingProbeWant)
	}

	calls := readCalls(t, log)
	if n := countExact(calls, "ls --json agentteams/bucket"); n != 2 {
		t.Fatalf("probe invocations = %d, want 2 (transient failure re-probed): %v", n, calls)
	}
	if n := countExact(calls, "find agentteams/bucket/human/alice/files/"); n != 1 {
		t.Fatalf("find fallback invocations = %d, want 1: %v", n, calls)
	}
	if n := countExact(calls, "ls --json agentteams/bucket/human/alice/files/"); n != 1 {
		t.Fatalf("json listings = %d, want 1 (cached after the successful probe): %v", n, calls)
	}
}

// --- parseMCFind unit tests (the fallback parsing contract) ---------------

func newParseMCFindClient() *MinIOClient {
	return NewMinIOClient(Config{
		Alias:         "agentteams",
		Bucket:        "bucket",
		StoragePrefix: "agentteams/bucket",
	})
}

func TestParseMCFind_AliasBucketForm(t *testing.T) {
	client := newParseMCFindClient()
	out := "agentteams/bucket/human/alice/files/meeting notes.txt\n" +
		"agentteams/bucket/human/alice/files/my dir/inner.txt\n"
	infos, err := client.parseMCFind(out, "human/alice/files/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"meeting notes.txt", "my dir/"}
	if got := listingNames(t, infos); !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestParseMCFind_BucketOnlyForm(t *testing.T) {
	client := newParseMCFindClient()
	out := "bucket/human/alice/files/a b.txt\nbucket/human/alice/files/x/y.txt\n"
	infos, err := client.parseMCFind(out, "human/alice/files/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a b.txt", "x/"}
	if got := listingNames(t, infos); !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestParseMCFind_BareRelativeForm(t *testing.T) {
	client := newParseMCFindClient()
	out := "a b.txt\nx/y.txt\n"
	infos, err := client.parseMCFind(out, "human/alice/files/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a b.txt", "x/"}
	if got := listingNames(t, infos); !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestParseMCFind_DeduplicatesDirectoryEntries(t *testing.T) {
	client := newParseMCFindClient()
	out := "agentteams/bucket/p/a/1.txt\n" +
		"agentteams/bucket/p/a/2.txt\n" +
		"agentteams/bucket/p/b.txt\n"
	infos, err := client.parseMCFind(out, "p/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/", "b.txt"}
	if got := listingNames(t, infos); !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestParseMCFind_RootPrefixAndEmptyOutput(t *testing.T) {
	client := newParseMCFindClient()
	infos, err := client.parseMCFind("agentteams/bucket/loose.txt\n", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"loose.txt"}
	if got := listingNames(t, infos); !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	infos, err = client.parseMCFind("", "human/alice/files/")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("names = %v, want none", listingNames(t, infos))
	}
}
