package oss

import "testing"

// Real `mc stat --json` shape (mc cmd/stat.go statMessage): the S3 response
// headers live in the canonicalized metadata map; there is no top-level
// contentType field. This is the shape MinIO and RustFS both produce.
const mcStatWithMetadata = `{
  "status": "success",
  "name": "agentteams-storage/humans/alice/files/a.txt",
  "lastModified": "2026-10-30T21:00:00Z",
  "size": 0,
  "etag": "d41d8cd98f00b204e9800998ecf8427e",
  "type": "file",
  "metadata": {
    "Content-Type": "application/vnd.agentteams.tombstone",
    "X-Amz-Meta-Owner": "alice"
  }
}`

func TestParseMCStatJSONReadsMetadataContentType(t *testing.T) {
	meta, err := parseMCStatJSON(mcStatWithMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ContentType != "application/vnd.agentteams.tombstone" {
		t.Fatalf("ContentType = %q, want the metadata Content-Type", meta.ContentType)
	}
	if meta.Size != 0 || meta.ETag == "" {
		t.Fatalf("meta = %+v, want size/etag parsed", meta)
	}
}

func TestParseMCStatJSONAcceptsLowercaseAndMetaKeys(t *testing.T) {
	cases := map[string]string{
		"lowercase":        `{"lastModified":"2026-10-30T21:00:00Z","size":1,"metadata":{"content-type":"text/plain"}}`,
		"x-amz-meta":       `{"lastModified":"2026-10-30T21:00:00Z","size":1,"metadata":{"X-Amz-Meta-Content-Type":"text/csv"}}`,
		"top-level legacy": `{"lastModified":"2026-10-30T21:00:00Z","size":1,"contentType":"application/json"}`,
	}
	want := map[string]string{
		"lowercase":        "text/plain",
		"x-amz-meta":       "text/csv",
		"top-level legacy": "application/json",
	}
	for name, payload := range cases {
		meta, err := parseMCStatJSON(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if meta.ContentType != want[name] {
			t.Fatalf("%s: ContentType = %q, want %q", name, meta.ContentType, want[name])
		}
	}
}

func TestParseMCStatJSONWithoutMetadataLeavesContentTypeEmpty(t *testing.T) {
	meta, err := parseMCStatJSON(`{"lastModified":"2026-10-30T21:00:00Z","size":3,"etag":"abc"}`)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ContentType != "" {
		t.Fatalf("ContentType = %q, want empty", meta.ContentType)
	}
}
