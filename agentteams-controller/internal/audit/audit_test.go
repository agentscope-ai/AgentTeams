package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func newTestClient(t *testing.T, sc oss.StorageClient) *Client {
	t.Helper()
	c := NewClient(sc)
	// Shrink retries so conflict tests stay fast.
	c.retryBackoffs = [3]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	return c
}

func sampleEvent() Event {
	return Event{
		Who:        "admin",
		Role:       "admin",
		When:       time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC),
		Target:     "maizong",
		Action:     "capability_grant",
		Capability: "approval_policy",
		After:      []string{"approval_policy"},
	}
}

func TestRecordCreatesObjectAndWritesParseableLine(t *testing.T) {
	sc := ossfake.NewMemory()
	c := newTestClient(t, sc)

	c.Record(context.Background(), sampleEvent())

	data, err := sc.GetObject(context.Background(), "audit/2026-09-12.jsonl")
	if err != nil {
		t.Fatalf("object not created: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 line, got %d", len(lines))
	}
	var got Event
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line not parseable: %v", err)
	}
	if got.Who != "admin" || got.Role != "admin" || got.Target != "maizong" ||
		got.Action != "capability_grant" || got.Capability != "approval_policy" ||
		len(got.After) != 1 || got.After[0] != "approval_policy" {
		t.Fatalf("fields incomplete or wrong: %+v", got)
	}
	if got.When.UTC().Format("2006-01-02") != "2026-09-12" {
		t.Fatalf("when = %v, want 2026-09-12", got.When)
	}
}

func TestRecordAppendsPreservingEarlierLines(t *testing.T) {
	sc := ossfake.NewMemory()
	c := newTestClient(t, sc)

	c.Record(context.Background(), sampleEvent())
	first, _ := sc.GetObject(context.Background(), "audit/2026-09-12.jsonl")

	ev := sampleEvent()
	ev.Action = "capability_revoke"
	ev.Capability = "approval_policy"
	c.Record(context.Background(), ev)

	data, err := sc.GetObject(context.Background(), "audit/2026-09-12.jsonl")
	if err != nil {
		t.Fatalf("get after append: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if []byte(lines[0]) == nil || string(first[:len(lines[0])]) != lines[0] {
		t.Fatalf("earlier line corrupted: %q", lines[0])
	}
	var second Event
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second line not parseable: %v", err)
	}
	if second.Action != "capability_revoke" {
		t.Fatalf("second line action = %q", second.Action)
	}
}

// conflictOnce fails PutObjectIfMatch a fixed number of times before
// delegating — a deterministic stand-in for a concurrent writer.
type conflictOnce struct {
	oss.StorageClient
	fails int
}

func (c *conflictOnce) PutObjectIfMatch(ctx context.Context, key string, data []byte, match string) error {
	if c.fails > 0 {
		c.fails--
		return oss.ErrPreconditionFailed
	}
	return c.StorageClient.PutObjectIfMatch(ctx, key, data, match)
}

func TestRecordRetriesOnETagConflict(t *testing.T) {
	mem := ossfake.NewMemory()
	// Prime the object so the second record takes the IfMatch path.
	c := newTestClient(t, mem)
	c.Record(context.Background(), sampleEvent())

	sc := &conflictOnce{StorageClient: mem, fails: 2}
	c2 := newTestClient(t, sc)
	ev := sampleEvent()
	ev.Action = "capability_revoke"
	c2.Record(context.Background(), ev)

	data, err := mem.GetObject(context.Background(), "audit/2026-09-12.jsonl")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("event lost after 2 conflicts: %d lines, want 2", len(lines))
	}
}

func TestRecordGivesUpAfterRetriesWithoutPanic(t *testing.T) {
	mem := ossfake.NewMemory()
	c := newTestClient(t, mem)
	c.Record(context.Background(), sampleEvent())
	before, _ := mem.GetObject(context.Background(), "audit/2026-09-12.jsonl")

	sc := &conflictOnce{StorageClient: mem, fails: 99}
	c2 := newTestClient(t, sc)
	ev := sampleEvent()
	ev.Action = "capability_revoke"
	c2.Record(context.Background(), ev) // must not panic; layer-2 error is logged only

	after, err := mem.GetObject(context.Background(), "audit/2026-09-12.jsonl")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("object mutated after exhausted retries; append is atomic (all-or-nothing)")
	}
}

func TestRecordConcurrentNoLostLines(t *testing.T) {
	sc := ossfake.NewMemory()
	c := newTestClient(t, sc)
	when := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Record(context.Background(), Event{
				Who: "admin", Role: "admin", When: when,
				Action: "capability_grant", Capability: "approval_policy",
				Target: fmt.Sprintf("human-%d", i),
			})
		}(i)
	}
	wg.Wait()

	data, err := sc.GetObject(context.Background(), "audit/2026-09-12.jsonl")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != n {
		t.Fatalf("lost lines under concurrency: %d, want %d", got, n)
	}
}

// TestEventJSONHasClosedSchema is the secret-hygiene pin (#1220 §8 "never
// record secret values"): the event schema is a closed key set with no
// free-text field large enough to smuggle a credential, and Before/After
// carry capability names from the closed value set.
func TestEventJSONHasClosedSchema(t *testing.T) {
	ev := Event{
		Who: "admin", Role: "admin",
		When:       time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC),
		Target:     "maizong",
		TargetTeam: "market-team",
		Action:     "capability_grant",
		Capability: "approval_policy",
		Before:     []string{}, // omitempty: an empty before is a pure grant (key absent by design)
		After:      []string{"approval_policy"},
		Detail:     "granted via human update",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// "before" is omitted for a pure grant (omitempty); every other key is
	// present. No key outside the documented set may appear.
	wantKeys := []string{"who", "role", "when", "target", "targetTeam", "action", "capability", "after", "detail"}
	if len(m) != len(wantKeys) {
		t.Fatalf("event JSON key set changed (secret-hygiene pin): %v", m)
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %v", k, m)
		}
	}
}

func TestRecordNilStorageStillLogsWithoutPanic(t *testing.T) {
	c := NewClient(nil)
	c.Record(context.Background(), sampleEvent()) // must not panic
}
