package outbox

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/adapter"
)

// --- fixtures ---

var dispatcherNow = time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)

type provisionCall struct {
	name        string
	generation  int64
	spec        json.RawMessage
	incarnation string
}

type deprovisionCall struct {
	name        string
	generation  int64
	incarnation string
}

// fakeEventClient records deliveries and returns the configured errors.
type fakeEventClient struct {
	provisionErrs  []error // consumed one per Provision call; empty = success
	deprovisionErr error
	provisions     []provisionCall
	deprovisions   []deprovisionCall
}

func (f *fakeEventClient) Provision(_ context.Context, name string, generation int64, spec json.RawMessage, incarnation string) error {
	f.provisions = append(f.provisions, provisionCall{name: name, generation: generation, spec: spec, incarnation: incarnation})
	if len(f.provisionErrs) == 0 {
		return nil
	}
	err := f.provisionErrs[0]
	f.provisionErrs = f.provisionErrs[1:]
	return err
}

func (f *fakeEventClient) Deprovision(_ context.Context, name string, generation int64, incarnation string) error {
	f.deprovisions = append(f.deprovisions, deprovisionCall{name: name, generation: generation, incarnation: incarnation})
	return f.deprovisionErr
}

func newTestDispatcher(t *testing.T, client EventClient) (*Dispatcher, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	d := &Dispatcher{
		DB:       db,
		Client:   client,
		Interval: time.Hour, // the Run test must not fire a second tick
		Now:      func() time.Time { return dispatcherNow },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return d, mock
}

var pendingTestColumns = []string{"id", "worker_name", "generation", "event_type", "payload_json", "attempts"}

func pendingRows(rows ...[]any) *sqlmock.Rows {
	r := sqlmock.NewRows(pendingTestColumns)
	for _, row := range rows {
		values := make([]driver.Value, len(row))
		for i, v := range row {
			values[i] = v
		}
		r.AddRow(values...)
	}
	return r
}

func expectSelect(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(selectPendingSQL)).
		WithArgs(dispatcherNow, int64(defaultOutboxBatch)).
		WillReturnRows(rows)
}

// --- tests ---

func TestTickPublishesProvision(t *testing.T) {
	client := &fakeEventClient{}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(11), "w1", int64(3), "provision", []byte(`{"name":"w1","generation":3,"spec":{"model":"m"}}`), int64(0),
	}))
	mock.ExpectExec(regexp.QuoteMeta(markPublishedSQL)).
		WithArgs(dispatcherNow, dispatcherNow, int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	published, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if published != 1 {
		t.Fatalf("published = %d, want 1", published)
	}
	if len(client.provisions) != 1 {
		t.Fatalf("Provision calls = %d, want 1", len(client.provisions))
	}
	call := client.provisions[0]
	if call.name != "w1" || call.generation != 3 || string(call.spec) != `{"model":"m"}` {
		t.Fatalf("Provision(%q, %d, %s), want (w1, 3, the payload spec)", call.name, call.generation, call.spec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickFailureSchedulesBackoff(t *testing.T) {
	client := &fakeEventClient{provisionErrs: []error{errors.New("boom")}}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(12), "w1", int64(4), "update", []byte(`{"name":"w1","generation":4,"spec":{}}`), int64(0),
	}))
	// First failure: 5s * 2^(1-1) = 5s.
	mock.ExpectExec(regexp.QuoteMeta(markFailedSQL)).
		WithArgs(dispatcherNow.Add(5*time.Second), "boom", dispatcherNow, int64(12)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	published, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickBackoffDoubles(t *testing.T) {
	client := &fakeEventClient{provisionErrs: []error{errors.New("boom")}}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(13), "w1", int64(2), "provision", []byte(`{"name":"w1","generation":2,"spec":{}}`), int64(2),
	}))
	// Third failure: 5s * 2^(3-1) = 20s.
	mock.ExpectExec(regexp.QuoteMeta(markFailedSQL)).
		WithArgs(dispatcherNow.Add(20*time.Second), "boom", dispatcherNow, int64(13)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickExhaustedAttemptsGoDead(t *testing.T) {
	client := &fakeEventClient{provisionErrs: []error{errors.New("boom")}}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(14), "w1", int64(5), "provision", []byte(`{"name":"w1","generation":5,"spec":{}}`), int64(4),
	}))
	// The fifth failed attempt is the dead-letter threshold (config.MaxOutboxAttempts).
	mock.ExpectExec(regexp.QuoteMeta(markDeadSQL)).
		WithArgs("boom", dispatcherNow, int64(14)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	published, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickDeprovisionNotFoundIsPublished(t *testing.T) {
	client := &fakeEventClient{
		deprovisionErr: fmt.Errorf("adapter: deprovision %q: %w: controller status 404", "w1", adapter.ErrNotFound),
	}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(15), "w1", int64(7), "deprovision", []byte(`{"name":"w1","generation":7}`), int64(1),
	}))
	mock.ExpectExec(regexp.QuoteMeta(markPublishedSQL)).
		WithArgs(dispatcherNow, dispatcherNow, int64(15)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	published, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if published != 1 {
		t.Fatalf("published = %d, want 1 (a missing worker is already deprovisioned)", published)
	}
	if len(client.deprovisions) != 1 || client.deprovisions[0].name != "w1" || client.deprovisions[0].generation != 7 {
		t.Fatalf("Deprovision calls = %+v, want (w1, 7)", client.deprovisions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickBadPayloadGoesDead(t *testing.T) {
	client := &fakeEventClient{}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(16), "w1", int64(1), "provision", []byte(`not-json`), int64(0),
	}))
	mock.ExpectExec(regexp.QuoteMeta(markDeadSQL)).
		WithArgs(sqlmock.AnyArg(), dispatcherNow, int64(16)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	published, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
	if len(client.provisions) != 0 {
		t.Fatal("an undecodable payload must not be delivered")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickUnknownEventTypeGoesDead(t *testing.T) {
	client := &fakeEventClient{}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(17), "w1", int64(1), "drain", []byte(`{"name":"w1","generation":1}`), int64(0),
	}))
	mock.ExpectExec(regexp.QuoteMeta(markDeadSQL)).
		WithArgs(sqlmock.AnyArg(), dispatcherNow, int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(client.provisions) != 0 || len(client.deprovisions) != 0 {
		t.Fatal("an unknown event type must not be delivered")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickTruncatesLastError(t *testing.T) {
	long := strings.Repeat("x", maxLastError+500)
	client := &fakeEventClient{provisionErrs: []error{errors.New(long)}}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(18), "w1", int64(1), "provision", []byte(`{"name":"w1","generation":1,"spec":{}}`), int64(4),
	}))
	want := strings.Repeat("x", maxLastError)
	mock.ExpectExec(regexp.QuoteMeta(markDeadSQL)).
		WithArgs(want, dispatcherNow, int64(18)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickContinuesAfterSingleFailure(t *testing.T) {
	// The first event fails to deliver, the second publishes: the batch
	// must not stop on a delivery failure.
	client := &fakeEventClient{provisionErrs: []error{errors.New("boom"), nil}}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows(
		[]any{int64(19), "w1", int64(1), "provision", []byte(`{"name":"w1","generation":1,"spec":{}}`), int64(0)},
		[]any{int64(20), "w2", int64(1), "provision", []byte(`{"name":"w2","generation":1,"spec":{}}`), int64(0)},
	))
	mock.ExpectExec(regexp.QuoteMeta(markFailedSQL)).
		WithArgs(dispatcherNow.Add(5*time.Second), "boom", dispatcherNow, int64(19)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(markPublishedSQL)).
		WithArgs(dispatcherNow, dispatcherNow, int64(20)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	published, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if published != 1 {
		t.Fatalf("published = %d, want 1 (the second event still publishes)", published)
	}
	if len(client.provisions) != 2 {
		t.Fatalf("Provision calls = %d, want 2 (one per event)", len(client.provisions))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestTickStatusUpdateFailureReturnsError(t *testing.T) {
	client := &fakeEventClient{}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows([]any{
		int64(21), "w1", int64(1), "provision", []byte(`{"name":"w1","generation":1,"spec":{}}`), int64(0),
	}))
	mock.ExpectExec(regexp.QuoteMeta(markPublishedSQL)).
		WillReturnError(errors.New("update failed"))

	published, err := d.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mark event 21 published") {
		t.Fatalf("Tick error = %v, want it to name the failed status update", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}
}

func TestRunTicksImmediatelyAndStopsOnCancel(t *testing.T) {
	client := &fakeEventClient{}
	d, mock := newTestDispatcher(t, client)
	expectSelect(mock, pendingRows())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	// The immediate tick consumes the select expectation; wait for it so
	// the test cannot cancel before the first poll happens.
	waitForCondition(t, func() bool { return mock.ExpectationsWereMet() == nil })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func waitForCondition(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
