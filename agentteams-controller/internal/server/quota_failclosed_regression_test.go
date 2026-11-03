package server

// Regression for #1426 P1: critical-metadata conditional writes must fail
// closed.
//
// The maintainer reproduced the original defect on real hardware (a provider
// answering If-Match with 501/NotImplemented): two independent handlers each
// uploaded 6 bytes against a 10-byte quota, both got 200, 12 bytes were
// stored, and the ledger only recorded 12 afterwards. The root cause was
// PutObjectIfMatchWithType's Stat-then-Put degradation at the quota ledger's
// atomic boundary.
//
// After the fix, the ledger (and every other critical-metadata CAS) goes
// through PutObjectIfMatchStrict, which turns a 501 into
// oss.ErrConditionalUnsupported → 503 conditional_write_unsupported.
//
// A note on the spec's literal (200, 503) assertion pair: with quota 10 and
// two 6-byte files, the second writer's FRESH ledger read (mandatory after
// losing the create race, or already current when the ledger exists) sees
// used=6, so 6+6=12 > 10 fires as 413 quota_exceeded BEFORE any conditional
// write is attempted. 503 is only reachable when the in-memory quota check
// PASSES and the strict CAS itself is rejected — exercised by
// TestQuotaFailClosed_503MappingWithAudit (a 4-byte second file, 6+4=10
// within quota) and by TestQuotaFailClosed_MaintainerInterleaving
// (a pre-existing ledger both writers read at the same revision). All three
// tests assert the substance of the spec: the over-acceptance is closed —
// a second writer can never get 200 on a 501 provider, and the store never
// holds more than the quota.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- Rig ---------------------------------------------------------------------

// ledgerBarrierOSS wraps the in-memory store and gates the first TWO
// GetObject calls on the quota ledger key: both concurrent readers block
// until both have arrived, so they are guaranteed to observe the same ledger
// revision before either writes (the spec's barrier — no sleep timing).
// Later reads pass through. A request-context timeout turns a barrier
// deadlock into a clean test failure.
type ledgerBarrierOSS struct {
	*ossfake.Memory
	key  string
	done chan struct{}
	arr  int32
}

func newLedgerBarrierOSS(store *ossfake.Memory, username string) *ledgerBarrierOSS {
	return &ledgerBarrierOSS{Memory: store, key: humanQuotaLedgerKey(username), done: make(chan struct{})}
}

func (b *ledgerBarrierOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	if key == b.key {
		if n := atomic.AddInt32(&b.arr, 1); n <= 2 {
			if n == 2 {
				close(b.done)
			}
			select {
			case <-b.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return b.Memory.GetObject(ctx, key)
}

// newQuotaRegressionRig builds the shared state for one scenario: a fake k8s
// client with alice at the given quota (quotaBytes < 0 = unlimited), and a
// memory store with the 501-provider injection optionally enabled.
func newQuotaRegressionRig(t *testing.T, quotaBytes int64, strictUnsupported bool) (*ossfake.Memory, client.Client) {
	t.Helper()
	scheme := newServerTestScheme(t)
	alice := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Alice"},
	}
	if quotaBytes >= 0 {
		q := quotaBytes
		alice.Spec.WorkspaceQuotaBytes = &q
	}
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(alice).
		Build()
	store := ossfake.NewMemory()
	store.StrictUnsupported.Store(strictUnsupported)
	return store, k8s
}

// hfPutWithTimeout runs putFile for one handler instance on the given
// payload; the request context bounds the test against a barrier deadlock.
func hfPutWithTimeout(t *testing.T, hfh *humanFilesHandler, name, path, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := hfReq(t, http.MethodPut, name, path, hfSelf(), strings.NewReader(content))
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	hfh.putFile(rec, req)
	return rec
}

// decodeHumanFileCodedErrorBody parses the {code, message} body of the
// human-file coded-error responses.
func decodeHumanFileCodedErrorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("coded error body parse: %v: %s", err, rec.Body.String())
	}
	return body.Code
}

// ledgerState reads and decodes the quota ledger object.
func ledgerState(t *testing.T, store *ossfake.Memory, username string) humanQuotaLedger {
	t.Helper()
	data, err := store.GetObject(context.Background(), humanQuotaLedgerKey(username))
	if err != nil {
		t.Fatalf("quota ledger missing: %v", err)
	}
	var l humanQuotaLedger
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatalf("quota ledger decode: %v: %s", err, data)
	}
	return l
}

// liveFileBytes returns the total bytes stored under the human's files/ area.
func liveFileBytes(t *testing.T, store *ossfake.Memory, username string) int64 {
	t.Helper()
	infos, err := store.ListObjectsDetailed(context.Background(), humanFilesPrefix(username))
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	var total int64
	for _, info := range infos {
		if info.Name == "" || strings.HasSuffix(info.Name, "/") {
			continue
		}
		meta, err := store.StatMeta(context.Background(), humanFilesPrefix(username)+info.Name)
		if err != nil {
			t.Fatalf("stat %s: %v", info.Name, err)
		}
		total += meta.Size
	}
	return total
}

// fileCount returns how many objects live under the human's files/ area.
func fileCount(t *testing.T, store *ossfake.Memory, username string) int {
	t.Helper()
	infos, err := store.ListObjectsDetailed(context.Background(), humanFilesPrefix(username))
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	n := 0
	for _, info := range infos {
		if info.Name != "" && !strings.HasSuffix(info.Name, "/") {
			n++
		}
	}
	return n
}

// --- T1: the spec scenario on a 501 provider ----------------------------------

// TestQuotaFailClosed_501ProviderTwoWriters is the maintainer's reproduction
// with the fix in place: 501 provider, quota 10, two INDEPENDENT handler
// instances racing two 6-byte uploads, barrier-synchronized on the ledger
// read. Before the fix both got 200 and 12 bytes were stored; now exactly
// one writer wins (200, via the atomic create-with-reservation) and the
// loser is rejected against the winner's ledger (413 quota_exceeded on a
// fresh read — see the file-level note on the spec's 503 literal).
func TestQuotaFailClosed_501ProviderTwoWriters(t *testing.T) {
	store, k8s := newQuotaRegressionRig(t, 10, true)
	barrier := newLedgerBarrierOSS(store, "alice")
	// Two independent handler instances over one store (the spec requires
	// two instances, not one handler called twice).
	hA := NewHumanFilesHandler(k8s, "default", barrier, audit.NewClient(barrier))
	hB := NewHumanFilesHandler(k8s, "default", barrier, audit.NewClient(barrier))

	recA := httptest.NewRecorder()
	recB := httptest.NewRecorder()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); hfPutTo(t, hA, "alice", "a.txt", "AAAAAA", recA) }()
	go func() { defer wg.Done(); hfPutTo(t, hB, "alice", "b.txt", "BBBBBB", recB) }()
	wg.Wait()

	codes := []int{recA.Code, recB.Code}
	var got200, got413 int
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			got200++
		case http.StatusRequestEntityTooLarge:
			got413++
		default:
			t.Fatalf("unexpected status %d (bodies: %s / %s)", code, recA.Body.String(), recB.Body.String())
		}
	}
	if got200 != 1 || got413 != 1 {
		t.Fatalf("want exactly one 200 and one 413, got %v (bodies: %s / %s)", codes, recA.Body.String(), recB.Body.String())
	}
	// The rejected writer must carry the stable quota_exceeded code.
	for _, rec := range []*httptest.ResponseRecorder{recA, recB} {
		if rec.Code == http.StatusRequestEntityTooLarge && decodeHumanFileCodedErrorBody(t, rec) != "quota_exceeded" {
			t.Fatalf("413 body code = %q, want quota_exceeded", rec.Body.String())
		}
	}
	// The over-acceptance is closed: one 6-byte file stored, ledger = 6.
	if n := fileCount(t, store, "alice"); n != 1 {
		t.Fatalf("files stored = %d, want 1", n)
	}
	if b := liveFileBytes(t, store, "alice"); b != 6 {
		t.Fatalf("bytes stored = %d, want 6", b)
	}
	l := ledgerState(t, store, "alice")
	if l.UsedBytes != 6 {
		t.Fatalf("ledger UsedBytes = %d, want 6", l.UsedBytes)
	}
}

// hfPutTo runs putFile and writes into the given recorder (recorder passed
// in so each goroutine owns its own).
func hfPutTo(t *testing.T, hfh *humanFilesHandler, name, path, content string, rec *httptest.ResponseRecorder) {
	t.Helper()
	req := hfReq(t, http.MethodPut, name, path, hfSelf(), strings.NewReader(content))
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	hfh.putFile(rec, req.WithContext(ctx))
}

// TestQuotaFailClosed_ControlConditionalWriteSupported is the control arm:
// the identical sequence with conditional writes SUPPORTED (no 501
// injection). Behavior must be unchanged: one 200, the second writer
// 413 quota_exceeded, 6 bytes stored, ledger = 6.
func TestQuotaFailClosed_ControlConditionalWriteSupported(t *testing.T) {
	store, k8s := newQuotaRegressionRig(t, 10, false)
	barrier := newLedgerBarrierOSS(store, "alice")
	hA := NewHumanFilesHandler(k8s, "default", barrier, audit.NewClient(barrier))
	hB := NewHumanFilesHandler(k8s, "default", barrier, audit.NewClient(barrier))

	var wg sync.WaitGroup
	wg.Add(2)
	recA := httptest.NewRecorder()
	recB := httptest.NewRecorder()
	go func() { defer wg.Done(); hfPutTo(t, hA, "alice", "a.txt", "AAAAAA", recA) }()
	go func() { defer wg.Done(); hfPutTo(t, hB, "alice", "b.txt", "BBBBBB", recB) }()
	wg.Wait()

	codes := []int{recA.Code, recB.Code}
	var got200, got413 int
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			got200++
		case http.StatusRequestEntityTooLarge:
			got413++
		default:
			t.Fatalf("unexpected status %d (bodies: %s / %s)", code, recA.Body.String(), recB.Body.String())
		}
	}
	if got200 != 1 || got413 != 1 {
		t.Fatalf("control: want exactly one 200 and one 413, got %v (bodies: %s / %s)", codes, recA.Body.String(), recB.Body.String())
	}
	if n := fileCount(t, store, "alice"); n != 1 {
		t.Fatalf("control: files stored = %d, want 1", n)
	}
	if b := liveFileBytes(t, store, "alice"); b != 6 {
		t.Fatalf("control: bytes stored = %d, want 6", b)
	}
	if l := ledgerState(t, store, "alice"); l.UsedBytes != 6 {
		t.Fatalf("control: ledger UsedBytes = %d, want 6", l.UsedBytes)
	}
}

// --- T2: the 503 mapping ------------------------------------------------------

// TestQuotaFailClosed_503MappingWithAudit covers the spec's 503 assertion:
// on a 501 provider, a write whose quota check PASSES (6+4=10, within the
// 10-byte quota) must fail closed with 503 conditional_write_unsupported,
// store nothing, leave the ledger untouched, and leave an audit line.
func TestQuotaFailClosed_503MappingWithAudit(t *testing.T) {
	store, k8s := newQuotaRegressionRig(t, 10, true)
	h := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store))

	// First write: no ledger yet → atomic create-with-reservation succeeds
	// (If-None-Match is supported), so the first upload answers 200 even on
	// a 501 provider. The follow-up commit CAS is rejected (501) and
	// swallowed by design: the token stays outstanding, usage stays charged.
	recA := hfPutWithTimeout(t, h, "alice", "a.txt", "AAAAAA")
	if recA.Code != http.StatusOK {
		t.Fatalf("first write = %d, want 200: %s", recA.Code, recA.Body.String())
	}
	if b := liveFileBytes(t, store, "alice"); b != 6 {
		t.Fatalf("first write stored %d bytes, want 6", b)
	}

	// Second write: the fresh ledger read says used=6, so 6+4=10 is within
	// quota — the in-memory check PASSES — and the strict reservation CAS is
	// where the 501 provider must fail the operation closed.
	recB := hfPutWithTimeout(t, h, "alice", "b.txt", "CCCC")
	if recB.Code != http.StatusServiceUnavailable {
		t.Fatalf("second write = %d, want 503: %s", recB.Code, recB.Body.String())
	}
	if code := decodeHumanFileCodedErrorBody(t, recB); code != "conditional_write_unsupported" {
		t.Fatalf("503 body code = %q, want conditional_write_unsupported", code)
	}
	// Nothing stored by the rejected write; the ledger is untouched.
	if n := fileCount(t, store, "alice"); n != 1 {
		t.Fatalf("files after 503 = %d, want 1 (the rejected write stored bytes)", n)
	}
	if b := liveFileBytes(t, store, "alice"); b != 6 {
		t.Fatalf("bytes after 503 = %d, want 6", b)
	}
	if l := ledgerState(t, store, "alice"); l.UsedBytes != 6 {
		t.Fatalf("ledger after 503 = %d, want 6 (unchanged)", l.UsedBytes)
	}
	// The refusal is audited.
	if !auditContainsAction(t, store, "human_file_write_blocked") {
		t.Fatal("audit trail has no human_file_write_blocked line for the 503 refusal")
	}
}

// --- T4: the maintainer's exact interleaving ----------------------------------

// TestQuotaFailClosed_MaintainerInterleaving reproduces the production
// interleaving with a PRE-EXISTING ledger: both handlers read the same
// revision (the barrier), both pass the in-memory check (0+6 ≤ 10), and
// both attempt the conditional write. Before the fix the degraded
// Stat-then-Put let both commit (2×200, 12 bytes stored); now both are
// rejected with 503 and nothing is stored.
func TestQuotaFailClosed_MaintainerInterleaving(t *testing.T) {
	store, k8s := newQuotaRegressionRig(t, 10, true)
	// Pre-existing ledger at revision {usedBytes:0} (e.g. seeded before the
	// provider was switched to one without If-Match).
	if err := store.PutObject(context.Background(), humanQuotaLedgerKey("alice"), []byte(`{"usedBytes":0}`)); err != nil {
		t.Fatal(err)
	}
	barrier := newLedgerBarrierOSS(store, "alice")
	hA := NewHumanFilesHandler(k8s, "default", barrier, audit.NewClient(barrier))
	hB := NewHumanFilesHandler(k8s, "default", barrier, audit.NewClient(barrier))

	var wg sync.WaitGroup
	wg.Add(2)
	recA := httptest.NewRecorder()
	recB := httptest.NewRecorder()
	go func() { defer wg.Done(); hfPutTo(t, hA, "alice", "a.txt", "AAAAAA", recA) }()
	go func() { defer wg.Done(); hfPutTo(t, hB, "alice", "b.txt", "BBBBBB", recB) }()
	wg.Wait()

	for _, rec := range []*httptest.ResponseRecorder{recA, recB} {
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503 for both writers (bodies: %s / %s), got %d", recA.Body.String(), recB.Body.String(), rec.Code)
		}
		if code := decodeHumanFileCodedErrorBody(t, rec); code != "conditional_write_unsupported" {
			t.Fatalf("503 body code = %q, want conditional_write_unsupported", code)
		}
	}
	// Fail closed: no bytes stored, ledger at its pre-interleaving revision.
	if n := fileCount(t, store, "alice"); n != 0 {
		t.Fatalf("files stored = %d, want 0 (the over-acceptance is closed)", n)
	}
	if b := liveFileBytes(t, store, "alice"); b != 0 {
		t.Fatalf("bytes stored = %d, want 0", b)
	}
	if l := ledgerState(t, store, "alice"); l.UsedBytes != 0 {
		t.Fatalf("ledger UsedBytes = %d, want 0 (unchanged)", l.UsedBytes)
	}
}

// auditContainsAction scans the day's audit JSONL for an event action.
func auditContainsAction(t *testing.T, store *ossfake.Memory, action string) bool {
	t.Helper()
	key := "audit/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	data, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatalf("audit JSONL missing: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev struct {
			Action string `json:"action"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Action == action {
			return true
		}
	}
	return false
}
