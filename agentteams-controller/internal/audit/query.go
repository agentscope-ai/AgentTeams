// Read side of the dual-layer audit store (#1220 §8, #1245): a paged,
// filterable query over the append-only daily objects the Client appends
// to. Read-only — it never writes, so there is no lock contention with
// the writer.
//
// Degradation contract: a storage read failure or a malformed object line
// fails the whole request (typed errors below, surfaced as 502 by the
// HTTP layer). A missing daily object is normal (no events recorded that
// day) and yields zero events, not an error.
package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// ErrStorageUnavailable marks a durable-store read failure (object get or
// prefix list). Callers must surface it as a server error, never as an
// empty result.
var ErrStorageUnavailable = errors.New("audit: storage unavailable")

// ErrMalformedObject marks a daily object containing a line that does not
// decode as an Event. Callers must reject the whole request rather than
// return a partial (silently truncated) list.
var ErrMalformedObject = errors.New("audit: malformed audit object")

// ErrRangeTooLarge marks a from/to range wider than the scan limit.
var ErrRangeTooLarge = errors.New("audit: time range too large")

// maxScannedDays bounds how many daily objects one request may scan when
// the range is explicit.
const maxScannedDays = 366

// KindOf maps an event action to its audit kind — the categories of the
// #1220 §8 event table: capability grant/revoke, approval_level change,
// channel credential write, external source add/modify. Unknown actions
// (future event kinds) pass through unchanged, so they stay filterable by
// their exact name.
func KindOf(action string) string {
	switch {
	case strings.HasPrefix(action, "capability_"):
		return "capability"
	case strings.HasPrefix(action, "approval_level"):
		return "approval_level"
	case strings.HasPrefix(action, "channel"):
		return "channel"
	case strings.HasPrefix(action, "source"):
		return "source"
	default:
		return action
	}
}

// Cursor is a keyset pagination cursor: the position of the last event in
// the page it terminates. The total order is newest-first over
// (When, date, seq); within a daily object seq is the 1-based line number
// of the event, which is stable because objects are append-only.
type Cursor struct {
	Date string `json:"date"` // UTC day of the event (YYYY-MM-DD)
	Seq  int    `json:"seq"`  // 1-based line number within that day's object
	Ts   string `json:"ts"`   // event timestamp, RFC3339
}

// Encode serializes the cursor for the ?cursor= query parameter.
func (c *Cursor) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeCursor parses a ?cursor= value produced by Encode.
func DecodeCursor(s string) (*Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("invalid cursor encoding")
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New("invalid cursor payload")
	}
	if c.Date == "" || c.Seq < 1 || c.Ts == "" {
		return nil, errors.New("invalid cursor payload: date, seq and ts are required")
	}
	// Validate the field formats at decode time so the handler can answer
	// 400 for malformed client input before any storage scan (a bad ts
	// would otherwise surface as a server error from List).
	if _, err := time.Parse(time.RFC3339Nano, c.Ts); err != nil {
		return nil, errors.New("invalid cursor payload: ts must be RFC3339")
	}
	if _, err := time.Parse("2006-01-02", c.Date); err != nil {
		return nil, errors.New("invalid cursor payload: date must be YYYY-MM-DD")
	}
	return &c, nil
}

// QueryOptions selects and pages a set of audit events.
type QueryOptions struct {
	Team   string    // filter: TargetTeam equality; empty = no filter
	Kind   string    // filter: KindOf(Action) equality; empty = no filter
	From   time.Time // inclusive lower bound on the event timestamp; zero = unbounded
	To     time.Time // inclusive upper bound on the event timestamp; zero = unbounded
	Limit  int       // page size, must be >= 1
	Cursor *Cursor   // resume strictly after this position; nil = from the newest
}

// QueryResult is one newest-first page of events.
type QueryResult struct {
	Events     []Event
	NextCursor *Cursor // non-nil only when the page is full (more may follow)
}

// Query reads the durable audit store.
type Query struct {
	sc oss.StorageClient
}

// NewQuery creates a read-side query over sc.
func NewQuery(sc oss.StorageClient) *Query {
	return &Query{sc: sc}
}

// List returns one newest-first page of events matching opts.
func (q *Query) List(ctx context.Context, opts QueryOptions) (*QueryResult, error) {
	if opts.Limit < 1 {
		return nil, fmt.Errorf("limit must be >= 1")
	}
	keys, err := q.dailyKeys(ctx, opts.From, opts.To)
	if err != nil {
		return nil, err
	}

	var events []storedEvent
	for _, key := range keys {
		data, gerr := q.sc.GetObject(ctx, key)
		switch {
		case gerr == nil:
		case errors.Is(gerr, os.ErrNotExist):
			continue // nothing recorded that day
		default:
			return nil, fmt.Errorf("%w: get %s: %v", ErrStorageUnavailable, key, gerr)
		}
		parsed, err := parseDailyObject(key, data)
		if err != nil {
			return nil, err
		}
		events = append(events, parsed...)
	}

	filtered := make([]storedEvent, 0, len(events))
	for i := range events {
		e := &events[i]
		if opts.Team != "" && e.TargetTeam != opts.Team {
			continue
		}
		if opts.Kind != "" && KindOf(e.Action) != opts.Kind {
			continue
		}
		if !opts.From.IsZero() && e.When.Before(opts.From) {
			continue
		}
		if !opts.To.IsZero() && e.When.After(opts.To) {
			continue
		}
		filtered = append(filtered, *e)
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].sortsAfter(&filtered[j])
	})

	if opts.Cursor != nil {
		cursorTs, err := time.Parse(time.RFC3339Nano, opts.Cursor.Ts)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor timestamp %q: %v", opts.Cursor.Ts, err)
		}
		kept := make([]storedEvent, 0, len(filtered))
		for i := range filtered {
			e := &filtered[i]
			if e.isAtOrAfterCursor(cursorTs, opts.Cursor.Date, opts.Cursor.Seq) {
				continue
			}
			kept = append(kept, *e)
		}
		filtered = kept
	}

	out := &QueryResult{Events: make([]Event, 0, len(filtered))}
	end := len(filtered)
	if end > opts.Limit {
		end = opts.Limit
	}
	for i := 0; i < end; i++ {
		out.Events = append(out.Events, filtered[i].Event)
	}
	if end == opts.Limit && len(filtered) > opts.Limit {
		last := filtered[end-1]
		out.NextCursor = &Cursor{
			Date: last.Date,
			Seq:  last.Seq,
			Ts:   last.When.UTC().Format(time.RFC3339Nano),
		}
	}
	return out, nil
}

// storedEvent is a decoded event with its position in the daily object.
type storedEvent struct {
	Event
	Date string // UTC day of the event
	Seq  int    // 1-based line number within the object
}

// sortsAfter reports whether e comes strictly after o in newest-first
// order: When descending, then day descending, then line number descending.
func (e *storedEvent) sortsAfter(o *storedEvent) bool {
	if !e.When.Equal(o.When) {
		return e.When.After(o.When)
	}
	if e.Date != o.Date {
		return e.Date > o.Date
	}
	return e.Seq > o.Seq
}

// isAtOrAfterCursor reports whether e sits at or after the cursor position
// (i.e. it was already returned on an earlier page and must be excluded).
func (e *storedEvent) isAtOrAfterCursor(cursorTs time.Time, cursorDate string, cursorSeq int) bool {
	if !e.When.Equal(cursorTs) {
		return e.When.After(cursorTs)
	}
	if e.Date != cursorDate {
		return e.Date > cursorDate
	}
	return e.Seq >= cursorSeq
}

// dailyKeys returns the daily object keys to scan, ascending. With both
// bounds set the day range is computed directly; with an unbounded side
// the audit prefix is listed and filtered.
func (q *Query) dailyKeys(ctx context.Context, from, to time.Time) ([]string, error) {
	fromSet, toSet := !from.IsZero(), !to.IsZero()
	var f, t time.Time
	if fromSet {
		f = startOfDay(from)
	}
	if toSet {
		t = startOfDay(to)
	}

	if fromSet && toSet {
		// Compare the original instants: the day truncation below must not
		// hide within-day ordering (from=11:00Z & to=10:00Z on the same
		// day truncates to equal days and would otherwise scan and return
		// an empty 200).
		if from.After(to) {
			return nil, fmt.Errorf("%w: from is after to", ErrRangeTooLarge)
		}
		days := int(t.Sub(f).Hours()/24) + 1
		if days > maxScannedDays {
			return nil, fmt.Errorf("%w: %d days exceeds the %d-day scan limit", ErrRangeTooLarge, days, maxScannedDays)
		}
		keys := make([]string, 0, days)
		for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
			keys = append(keys, dayKey(d))
		}
		return keys, nil
	}

	names, err := q.sc.ListObjects(ctx, jsonlPrefix)
	if err != nil {
		return nil, fmt.Errorf("%w: list %s: %v", ErrStorageUnavailable, jsonlPrefix, err)
	}
	var keys []string
	for _, name := range names {
		// ListObjects returns names RELATIVE to the prefix (production
		// contract), so the daily object name is "2006-01-02.jsonl" without
		// the audit/ prefix. dayKey below re-attaches it for GetObject.
		dayName, ok := strings.CutSuffix(name, ".jsonl")
		if !ok {
			continue
		}
		d, err := time.Parse("2006-01-02", dayName)
		if err != nil {
			continue // not a daily object
		}
		d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		if fromSet && d.Before(f) {
			continue
		}
		if toSet && d.After(t) {
			continue
		}
		keys = append(keys, dayKey(d))
	}
	sort.Strings(keys)
	return keys, nil
}

// parseDailyObject decodes one daily object into positioned events. Any
// line that does not decode as an Event rejects the whole object:
// returning a partial list would silently drop history.
func parseDailyObject(key string, data []byte) ([]storedEvent, error) {
	var out []storedEvent
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("%w: %s line %d: %v", ErrMalformedObject, key, i+1, err)
		}
		if ev.When.IsZero() {
			return nil, fmt.Errorf("%w: %s line %d: missing timestamp", ErrMalformedObject, key, i+1)
		}
		out = append(out, storedEvent{
			Event: ev,
			Date:  ev.When.UTC().Format("2006-01-02"),
			Seq:   len(out) + 1,
		})
	}
	return out, nil
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func dayKey(day time.Time) string {
	return jsonlPrefix + day.Format("2006-01-02") + ".jsonl"
}
