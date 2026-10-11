package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// seedEvent marshals one audit line the way the writer produces it.
func seedEvent(t *testing.T, ev Event) string {
	t.Helper()
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal seed event: %v", err)
	}
	return string(line)
}

var testDay = time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)

// failingStorage wraps a working client and fails GetObject on one key.
type failingStorage struct {
	oss.StorageClient
	failOn string
}

func (f failingStorage) GetObject(ctx context.Context, key string) ([]byte, error) {
	if key == f.failOn {
		return nil, errors.New("simulated storage outage")
	}
	return f.StorageClient.GetObject(ctx, key)
}

func TestQueryNewestFirstAndPagination(t *testing.T) {
	sc := ossfake.NewMemory()
	ctx := context.Background()
	d1 := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	// Object 1: two events on day 1 (seq 1, 2).
	content := seedEvent(t, Event{Who: "admin", Role: "admin", When: d1, Target: "h1", Action: "capability_grant", Capability: "approval_policy"}) + "\n" +
		seedEvent(t, Event{Who: "admin", Role: "admin", When: d1.Add(time.Hour), Target: "h2", Action: "capability_revoke", Capability: "external_sources"}) + "\n"
	if err := sc.PutObject(ctx, "audit/2026-09-13.jsonl", []byte(content)); err != nil {
		t.Fatal(err)
	}
	// Object 2: one event on day 2.
	if err := sc.PutObject(ctx, "audit/2026-09-14.jsonl", []byte(seedEvent(t, Event{Who: "admin", Role: "admin", When: d2, Target: "h3", Action: "capability_grant", Capability: "channel_secrets"})+"\n")); err != nil {
		t.Fatal(err)
	}
	// Object 3: one event on day 3.
	if err := sc.PutObject(ctx, "audit/2026-09-15.jsonl", []byte(seedEvent(t, Event{Who: "admin", Role: "admin", When: d3, Target: "h4", Action: "capability_grant", Capability: "full_access"})+"\n")); err != nil {
		t.Fatal(err)
	}

	q := NewQuery(sc)

	// Page 1: the two newest events, with a cursor.
	res, err := q.List(ctx, QueryOptions{Limit: 2})
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(res.Events) != 2 || res.NextCursor == nil {
		t.Fatalf("page 1: want 2 events + cursor, got %d events, cursor=%v", len(res.Events), res.NextCursor)
	}
	if res.Events[0].Target != "h4" || res.Events[1].Target != "h3" {
		t.Fatalf("page 1 order wrong: got %s, %s", res.Events[0].Target, res.Events[1].Target)
	}

	// Page 2 via cursor: the remaining two, oldest last.
	res, err = q.List(ctx, QueryOptions{Limit: 2, Cursor: res.NextCursor})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("page 2: want 2 events, got %d", len(res.Events))
	}
	if res.Events[0].Target != "h2" || res.Events[1].Target != "h1" {
		t.Fatalf("page 2 order wrong: got %s, %s", res.Events[0].Target, res.Events[1].Target)
	}
	if res.NextCursor != nil {
		t.Fatalf("page 2 consumed all remaining events and must not carry a cursor, got %v", res.NextCursor)
	}

	// Page 3: empty.
	res, err = q.List(ctx, QueryOptions{Limit: 2, Cursor: &Cursor{Date: "2026-09-13", Seq: 1, Ts: "2026-09-13T10:00:00Z"}})
	if err != nil {
		t.Fatalf("list page 3: %v", err)
	}
	if len(res.Events) != 0 || res.NextCursor != nil {
		t.Fatalf("page 3: want empty, got %d events", len(res.Events))
	}
}

func TestQueryFilters(t *testing.T) {
	sc := ossfake.NewMemory()
	ctx := context.Background()
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	lines := []string{
		seedEvent(t, Event{Who: "admin", Role: "admin", When: base, Target: "w1", TargetTeam: "alpha", Action: "capability_grant", Capability: "approval_policy"}),
		seedEvent(t, Event{Who: "admin", Role: "admin", When: base.Add(time.Minute), Target: "w2", TargetTeam: "beta", Action: "approval_level_change", Detail: "ask -> off"}),
		seedEvent(t, Event{Who: "admin", Role: "admin", When: base.Add(2 * time.Minute), Target: "w1", TargetTeam: "alpha", Action: "channel_credential_write", Detail: "field: matrix.accessToken"}),
		seedEvent(t, Event{Who: "admin", Role: "admin", When: base.Add(3 * time.Minute), Target: "w1", TargetTeam: "alpha", Action: "source_add", Detail: "nacos://redacted"}),
	}
	if err := sc.PutObject(ctx, "audit/2026-09-14.jsonl", []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		t.Fatal(err)
	}
	q := NewQuery(sc)

	// Team filter.
	res, err := q.List(ctx, QueryOptions{Team: "alpha", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("team=alpha: want 3, got %d", len(res.Events))
	}
	for _, ev := range res.Events {
		if ev.TargetTeam != "alpha" {
			t.Fatalf("team filter leaked %s", ev.TargetTeam)
		}
	}

	// Kind filter (derived categories). Each kind has exactly one event in
	// the seed, on different teams — so filter on kind alone here (the team
	// filter is covered above).
	for _, kind := range []string{"capability", "approval_level", "channel", "source"} {
		res, err := q.List(ctx, QueryOptions{Kind: kind, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Events) != 1 || res.Events[0].Action != kindAction(kind) {
			t.Fatalf("kind=%s: want 1 matching event, got %d", kind, len(res.Events))
		}
	}

	// Time bounds are inclusive.
	res, err = q.List(ctx, QueryOptions{From: base.Add(time.Minute), To: base.Add(2 * time.Minute), Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("from/to inclusive: want 2, got %d", len(res.Events))
	}

	// Unknown kind matches nothing.
	res, err = q.List(ctx, QueryOptions{Kind: "no_such_kind", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 0 {
		t.Fatalf("unknown kind: want 0, got %d", len(res.Events))
	}
}

func kindAction(kind string) string {
	switch kind {
	case "capability":
		return "capability_grant"
	case "approval_level":
		return "approval_level_change"
	case "channel":
		return "channel_credential_write"
	case "source":
		return "source_add"
	}
	return ""
}

func TestQueryMissingDayIsEmptyNotError(t *testing.T) {
	sc := ossfake.NewMemory()
	ctx := context.Background()
	d := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	if err := sc.PutObject(ctx, "audit/2026-09-14.jsonl", []byte(seedEvent(t, Event{Who: "admin", Role: "admin", When: d, Target: "h1", Action: "capability_grant"})+"\n")); err != nil {
		t.Fatal(err)
	}
	q := NewQuery(sc)
	// Range spanning a day with no object: the missing day is skipped.
	res, err := q.List(ctx, QueryOptions{
		From:  time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("missing day must not error: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(res.Events))
	}
}

func TestQueryMalformedLineFailsWholeRequest(t *testing.T) {
	sc := ossfake.NewMemory()
	ctx := context.Background()
	good := seedEvent(t, Event{Who: "admin", Role: "admin", When: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC), Target: "h1", Action: "capability_grant"})
	content := good + "\n{not json\n"
	if err := sc.PutObject(ctx, "audit/2026-09-14.jsonl", []byte(content)); err != nil {
		t.Fatal(err)
	}
	q := NewQuery(sc)
	_, err := q.List(ctx, QueryOptions{Limit: 50})
	if !errors.Is(err, ErrMalformedObject) {
		t.Fatalf("want ErrMalformedObject, got: %v", err)
	}
}

func TestQueryMalformedLineMissingTimestamp(t *testing.T) {
	sc := ossfake.NewMemory()
	ctx := context.Background()
	if err := sc.PutObject(ctx, "audit/2026-09-14.jsonl", []byte(`{"who":"admin","action":"capability_grant"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	q := NewQuery(sc)
	_, err := q.List(ctx, QueryOptions{Limit: 50})
	if !errors.Is(err, ErrMalformedObject) {
		t.Fatalf("want ErrMalformedObject for missing timestamp, got: %v", err)
	}
}

func TestQueryStorageFailureIs502Material(t *testing.T) {
	sc := failingStorage{StorageClient: ossfake.NewMemory(), failOn: "audit/2026-09-14.jsonl"}
	q := NewQuery(sc)
	_, err := q.List(context.Background(), QueryOptions{
		From:  time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		Limit: 50,
	})
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("want ErrStorageUnavailable, got: %v", err)
	}
}

func TestQueryUnboundedEnumeratesPrefix(t *testing.T) {
	sc := ossfake.NewMemory()
	ctx := context.Background()
	d := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	if err := sc.PutObject(ctx, "audit/2026-09-14.jsonl", []byte(seedEvent(t, Event{Who: "admin", Role: "admin", When: d, Target: "h1", Action: "capability_grant"})+"\n")); err != nil {
		t.Fatal(err)
	}
	// A non-daily object under the prefix must be ignored.
	if err := sc.PutObject(ctx, "audit/notes.txt", []byte("not a daily object\n")); err != nil {
		t.Fatal(err)
	}
	q := NewQuery(sc)
	res, err := q.List(ctx, QueryOptions{Limit: 50})
	if err != nil {
		t.Fatalf("unbounded list: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(res.Events))
	}
}

func TestQueryRangeTooLarge(t *testing.T) {
	sc := ossfake.NewMemory()
	q := NewQuery(sc)
	// 366 days (both endpoints inclusive) is the scan limit: it must pass.
	if _, err := q.List(context.Background(), QueryOptions{
		From:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Limit: 50,
	}); err != nil {
		t.Fatalf("366-day range is the limit and must pass, got: %v", err)
	}
	// 367 days exceeds it.
	_, err := q.List(context.Background(), QueryOptions{
		From:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Limit: 50,
	})
	if !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("want ErrRangeTooLarge for 367 days, got: %v", err)
	}
	_, err = q.List(context.Background(), QueryOptions{
		From:  time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		Limit: 50,
	})
	if !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("want ErrRangeTooLarge for from>to, got: %v", err)
	}
	// Same-day reversal: the original instants decide, not the day
	// truncation (from=11:00Z & to=10:00Z on one day).
	_, err = q.List(context.Background(), QueryOptions{
		From:  time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		Limit: 50,
	})
	if !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("want ErrRangeTooLarge for same-day from>to, got: %v", err)
	}
}

func TestQuerySameTimestampTiebreakBySeq(t *testing.T) {
	sc := ossfake.NewMemory()
	ctx := context.Background()
	d := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	// Two events with identical timestamps: append order (seq) decides.
	content := seedEvent(t, Event{Who: "admin", Role: "admin", When: d, Target: "first", Action: "capability_grant"}) + "\n" +
		seedEvent(t, Event{Who: "admin", Role: "admin", When: d, Target: "second", Action: "capability_grant"}) + "\n"
	if err := sc.PutObject(ctx, "audit/2026-09-14.jsonl", []byte(content)); err != nil {
		t.Fatal(err)
	}
	q := NewQuery(sc)

	// Newest first: "second" (later line) before "first".
	res, err := q.List(ctx, QueryOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Target != "second" || res.NextCursor == nil {
		t.Fatalf("page 1 wrong: %d events, cursor=%v", len(res.Events), res.NextCursor)
	}

	// Page 2: the other twin only.
	res, err = q.List(ctx, QueryOptions{Limit: 1, Cursor: res.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Target != "first" || res.NextCursor != nil {
		t.Fatalf("page 2 wrong: %d events, cursor=%v", len(res.Events), res.NextCursor)
	}

	// Page 3: empty.
	res, err = q.List(ctx, QueryOptions{Limit: 1, Cursor: &Cursor{Date: "2026-09-14", Seq: 1, Ts: d.UTC().Format(time.RFC3339Nano)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 0 || res.NextCursor != nil {
		t.Fatalf("page 3: want empty, got %d events", len(res.Events))
	}
}

func TestDecodeCursorRoundTrip(t *testing.T) {
	c := &Cursor{Date: "2026-09-14", Seq: 7, Ts: "2026-09-14T10:00:00Z"}
	enc, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodeCursor(enc)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if dec.Date != c.Date || dec.Seq != c.Seq || dec.Ts != c.Ts {
		t.Fatalf("round trip mismatch: %+v vs %+v", dec, c)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"!!!not-base64!!!",
		"YWJj",                                               // valid base64, not JSON
		`{"date":"2026-09-14"}`,                              // missing seq/ts
		`{"seq":3,"ts":"2026-09-14T10:00:00Z"}`,              // missing date
		`{"date":"2026-09-14","seq":1,"ts":"bad"}`,           // unparseable ts
		`{"date":"bad","seq":1,"ts":"2026-09-14T10:00:00Z"}`, // unparseable date
	} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Errorf("DecodeCursor(%q) should fail", bad)
		}
	}
}

func TestKindOfCategories(t *testing.T) {
	cases := map[string]string{
		"capability_grant":         "capability",
		"capability_revoke":        "capability",
		"approval_level_change":    "approval_level",
		"channel_credential_write": "channel",
		"source_add":               "source",
		"source_update":            "source",
		"future_action":            "future_action",
	}
	for action, want := range cases {
		if got := KindOf(action); got != want {
			t.Errorf("KindOf(%s) = %s, want %s", action, got, want)
		}
	}
}

func TestQueryLimitMustBePositive(t *testing.T) {
	q := NewQuery(ossfake.NewMemory())
	if _, err := q.List(context.Background(), QueryOptions{Limit: 0}); err == nil {
		t.Error("limit 0 should fail")
	}
}

func TestDayKeyAndStartOfDay(t *testing.T) {
	if got := dayKey(testDay); got != "audit/2026-09-14.jsonl" {
		t.Fatalf("dayKey = %s", got)
	}
	if got := startOfDay(time.Date(2026, 9, 14, 23, 59, 59, 999, time.UTC)); !got.Equal(testDay) {
		t.Fatalf("startOfDay = %v, want %v", got, testDay)
	}
	// dayKey must embed the UTC date (the writer keys objects the same way).
	dayName := dayKey(time.Date(2025, 12, 31, 5, 0, 0, 0, time.UTC))
	want := "audit/" + time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC).Format("2006-01-02") + ".jsonl"
	if dayName != want {
		t.Fatalf("dayKey = %s, want %s", dayName, want)
	}
}
