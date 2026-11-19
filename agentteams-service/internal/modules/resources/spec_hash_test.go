package resources

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCanonicalSpecJSONEmpty(t *testing.T) {
	b, err := CanonicalSpecJSON(Spec{})
	if err != nil {
		t.Fatalf("CanonicalSpecJSON: %v", err)
	}
	// container_managed is always emitted: false is a valid value.
	if got, want := string(b), `{"container_managed":false}`; got != want {
		t.Fatalf("CanonicalSpecJSON(empty) = %s, want %s", got, want)
	}
}

func TestCanonicalSpecJSONOmitsEmptyValues(t *testing.T) {
	s := Spec{
		WorkerName:    "",
		TeamSlug:      "",
		Model:         "",
		Skills:        []string{},
		Env:           map[string]string{},
		RemoteSkills:  nil,
		MCPServers:    json.RawMessage(""),
		Expose:        json.RawMessage(nil),
		ChannelPolicy: nil,
		Resources:     nil,
	}
	b, err := CanonicalSpecJSON(s)
	if err != nil {
		t.Fatalf("CanonicalSpecJSON: %v", err)
	}
	if got, want := string(b), `{"container_managed":false}`; got != want {
		t.Fatalf("CanonicalSpecJSON = %s, want %s", got, want)
	}
}

func TestCanonicalSpecJSONSortedAndNormalized(t *testing.T) {
	s := Spec{
		WorkerName: "worker-a",
		Model:      "qwen-max",
		Skills:     []string{"b", "a"},
		MCPServers: json.RawMessage(`{ "z": 1, "a": [2, 3] }`),
		Env:        map[string]string{"B": "2", "A": "1"},
	}
	b, err := CanonicalSpecJSON(s)
	if err != nil {
		t.Fatalf("CanonicalSpecJSON: %v", err)
	}
	// map keys sorted, JSON column whitespace/key order normalised.
	want := `{"container_managed":false,"env_json":{"A":"1","B":"2"},"mcp_servers_json":{"a":[2,3],"z":1},"model":"qwen-max","skills_json":["b","a"],"worker_name":"worker-a"}`
	if got := string(b); got != want {
		t.Fatalf("CanonicalSpecJSON = %s, want %s", got, want)
	}
}

func TestCanonicalSpecJSONExcludesInternalColumns(t *testing.T) {
	b, err := CanonicalSpecJSON(Spec{WorkerName: "worker-a", ContainerManaged: true})
	if err != nil {
		t.Fatalf("CanonicalSpecJSON: %v", err)
	}
	for _, excluded := range []string{
		`"generation"`,
		`"spec_hash"`,
		`"version"`,
		`"created_at"`,
		`"updated_at"`,
		`"deleted_at"`,
		`"observed_`,
	} {
		if bytes.Contains(b, []byte(excluded)) {
			t.Errorf("canonical spec must not contain %s: %s", excluded, b)
		}
	}
}

func TestSpecHashJSONOrderInsensitive(t *testing.T) {
	// The same JSON content with different whitespace / key order must
	// hash identically.
	a := Spec{MCPServers: json.RawMessage(`{"b":2,"a":1}`)}
	b := Spec{MCPServers: json.RawMessage(`{ "a": 1, "b": 2 }`)}
	ha, err := SpecHash(a)
	if err != nil {
		t.Fatalf("SpecHash(a): %v", err)
	}
	hb, err := SpecHash(b)
	if err != nil {
		t.Fatalf("SpecHash(b): %v", err)
	}
	if ha != hb {
		t.Fatalf("key order/whitespace changed the hash: %s != %s", ha, hb)
	}
}

func TestSpecHashIsStableSHA256Hex(t *testing.T) {
	h, err := SpecHash(Spec{Model: "qwen-max"})
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	if len(h) != 64 {
		t.Fatalf("len(SpecHash) = %d, want 64", len(h))
	}
	if strings.Trim(h, "0123456789abcdef") != "" {
		t.Fatalf("SpecHash = %q, want lowercase hex", h)
	}
	again, err := SpecHash(Spec{Model: "qwen-max"})
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	if again != h {
		t.Fatalf("SpecHash is not deterministic: %s != %s", again, h)
	}
}

func TestSpecHashChangesWhenSpecChanges(t *testing.T) {
	base := Spec{Model: "qwen-max", WorkerName: "worker-a"}
	changed := base
	changed.Model = "qwen-plus"
	h1, err := SpecHash(base)
	if err != nil {
		t.Fatalf("SpecHash(base): %v", err)
	}
	h2, err := SpecHash(changed)
	if err != nil {
		t.Fatalf("SpecHash(changed): %v", err)
	}
	if h1 == h2 {
		t.Fatal("changing model must change SpecHash")
	}
}

func TestSpecHashContainerManagedAlwaysPresent(t *testing.T) {
	falseHash, err := SpecHash(Spec{ContainerManaged: false})
	if err != nil {
		t.Fatalf("SpecHash(false): %v", err)
	}
	trueHash, err := SpecHash(Spec{ContainerManaged: true})
	if err != nil {
		t.Fatalf("SpecHash(true): %v", err)
	}
	if falseHash == trueHash {
		t.Fatal("container_managed=false and true must hash differently")
	}
	b, err := CanonicalSpecJSON(Spec{ContainerManaged: true})
	if err != nil {
		t.Fatalf("CanonicalSpecJSON: %v", err)
	}
	if !strings.Contains(string(b), `"container_managed":true`) {
		t.Fatalf("canonical spec must carry container_managed=true: %s", b)
	}
}

func TestSpecHashStatusAndInternalFieldsIgnored(t *testing.T) {
	// Worker carries status/internal columns that Spec must never see.
	base := &Worker{
		Model:         "qwen-max",
		ObservedPhase: "Pending",
		Generation:    1,
		Version:       1,
	}
	other := *base
	other.ObservedPhase = "Running"
	other.ObservedContainerState = "running"
	other.MatrixUserID = "@bot:example.test"
	other.Generation = 7
	other.Version = 9
	other.SpecHash = "stale"
	deleted := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	other.DeletedAt = &deleted

	h1, err := SpecHash(base.Spec())
	if err != nil {
		t.Fatalf("SpecHash(base): %v", err)
	}
	h2, err := SpecHash(other.Spec())
	if err != nil {
		t.Fatalf("SpecHash(other): %v", err)
	}
	if h1 != h2 {
		t.Fatalf("status/internal fields changed SpecHash: %s != %s", h1, h2)
	}
}

func TestSpecHashSkills(t *testing.T) {
	nilSkills, err := SpecHash(Spec{Skills: nil})
	if err != nil {
		t.Fatalf("SpecHash(nil): %v", err)
	}
	emptySkills, err := SpecHash(Spec{Skills: []string{}})
	if err != nil {
		t.Fatalf("SpecHash(empty): %v", err)
	}
	if nilSkills != emptySkills {
		t.Fatal("nil and empty skills must hash identically (both omitted)")
	}
	withSkills, err := SpecHash(Spec{Skills: []string{"skill-a"}})
	if err != nil {
		t.Fatalf("SpecHash(with): %v", err)
	}
	if withSkills == nilSkills {
		t.Fatal("adding a skill must change SpecHash")
	}
}

func TestSpecHashInvalidJSON(t *testing.T) {
	cases := []Spec{
		{MCPServers: json.RawMessage(`{`)},
		{RemoteSkills: json.RawMessage(`[1,]`)},
		{Expose: json.RawMessage(`not-json`)},
		{ChannelPolicy: json.RawMessage(`{"a":}`)},
	}
	for i, s := range cases {
		if _, err := SpecHash(s); err == nil {
			t.Errorf("case %d: SpecHash accepted invalid JSON", i)
		}
	}
}
