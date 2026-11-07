package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

// --- Per-human sync surface (#1414 P3) --------------------------------------

// hfSyncReq builds a request against one of the per-human sync routes; sub
// is "manifest" or "status". Only the "name" path value is set: the sync
// routes carry no further path parameters.
func hfSyncReq(t *testing.T, method, name, sub string, caller *authpkg.CallerIdentity, body string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1/humans/"+name+"/sync/"+sub, reader)
	req.SetPathValue("name", name)
	if caller != nil {
		req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
	}
	return req
}

// hfSyncCaller is an alice (RoleHuman) identity holding the given
// capabilities.
func hfSyncCaller(caps ...authpkg.Capability) *authpkg.CallerIdentity {
	values := make([]string, 0, len(caps))
	for _, c := range caps {
		values = append(values, string(c))
	}
	return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Capabilities: values}
}

// hfPutSync PUTs one manifest body and returns the recorder.
func hfPutSync(t *testing.T, hfh *humanFilesHandler, name string, caller *authpkg.CallerIdentity, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := hfSyncReq(t, http.MethodPut, name, "manifest", caller, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	hfh.putSyncManifest(rec, req)
	return rec
}

// hfGetSyncStatus GETs the status route and returns the recorder.
func hfGetSyncStatus(t *testing.T, hfh *humanFilesHandler, name string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	hfh.getSyncStatus(rec, hfSyncReq(t, http.MethodGet, name, "status", caller, ""))
	return rec
}

// hfSyncJSON decodes a JSON object response body.
func hfSyncJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response body: %v: %s", err, rec.Body.String())
	}
	return m
}

// hfSyncCoded asserts that rec carries a coded error body with the given
// stable code.
func hfSyncCoded(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var coded humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &coded); err != nil {
		t.Fatalf("coded error body: %v: %s", err, rec.Body.String())
	}
	if coded.Code != want {
		t.Fatalf("code = %q, want %q (%d %s)", coded.Code, want, rec.Code, rec.Body.String())
	}
}

// syncManifestBody renders a manifest write body with the given entry keys
// and tombstone paths. The client-supplied generation is a deliberate
// sentinel: the server must discard it and keep its own counter.
func syncManifestBody(entries, tombstones []string) string {
	elems := make([]string, 0, len(entries))
	for _, p := range entries {
		elems = append(elems, fmt.Sprintf(`%q:{"hash":"h-%s"}`, p, p))
	}
	toms := make([]string, 0, len(tombstones))
	for _, p := range tombstones {
		toms = append(toms, fmt.Sprintf(`{"path":%q}`, p))
	}
	return `{"version":1,"generation":99,"entries":{` + strings.Join(elems, ",") + `},"tombstones":[` + strings.Join(toms, ",") + `]}`
}

// --- Round trip ---------------------------------------------------------------

func TestHumanSync_RoundTrip(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)

	// First write: If-None-Match: * creates the manifest at generation 1.
	body := syncManifestBody([]string{"files/a.txt", "files/sub/b.md"}, []string{"files/gone.txt"})
	rec := hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("first write: %d %s", rec.Code, rec.Body.String())
	}
	putResp := hfSyncJSON(t, rec)
	etagV1, _ := putResp["etag"].(string)
	if etagV1 == "" {
		t.Fatalf("first write response has no etag: %s", rec.Body.String())
	}
	if putResp["generation"] != float64(1) {
		t.Fatalf("first write generation = %v, want 1", putResp["generation"])
	}
	if putResp["updatedAt"] == "" {
		t.Fatalf("first write response has no updatedAt: %s", rec.Body.String())
	}

	// GET returns the stored bytes verbatim under the same ETag.
	req := hfSyncReq(t, http.MethodGet, "alice", "manifest", alice, "")
	rec = httptest.NewRecorder()
	hfh.getSyncManifest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get manifest: %d %s", rec.Code, rec.Body.String())
	}
	// The response ETag is the quoted form; the write-base JSON etag is bare.
	if got, want := rec.Header().Get("ETag"), `"`+etagV1+`"`; got != want {
		t.Fatalf("GET ETag = %q, want the quoted write etag %q", got, want)
	}
	stored := hfSyncJSON(t, rec)
	if stored["version"] != float64(1) || stored["generation"] != float64(1) {
		t.Fatalf("stored version/generation = %v/%v, want 1/1", stored["version"], stored["generation"])
	}
	if stored["updatedBy"] != "alice" {
		t.Fatalf("stored updatedBy = %v, want alice", stored["updatedBy"])
	}
	entries := stored["entries"].(map[string]any)
	if len(entries) != 2 {
		t.Fatalf("stored entries = %v, want 2", entries)
	}
	tombstones := stored["tombstones"].([]any)
	if len(tombstones) != 1 {
		t.Fatalf("stored tombstones = %v, want 1", tombstones)
	}

	// The status endpoint summarizes the same manifest.
	st := hfSyncJSON(t, hfGetSyncStatus(t, hfh, "alice", alice))
	if st["entries"] != float64(2) || st["tombstones"] != float64(1) || st["generation"] != float64(1) {
		t.Fatalf("status = %v, want entries=2 tombstones=1 generation=1", st)
	}
	if st["updatedBy"] != "alice" {
		t.Fatalf("status updatedBy = %v, want alice", st["updatedBy"])
	}

	// A second write on the returned ETag: the client's sentinel
	// generation (99) must be discarded in favor of the server counter.
	rec = hfPutSync(t, hfh, "alice", alice, syncManifestBody([]string{"files/a.txt"}, []string{"files/gone.txt", "files/sub/b.md"}), map[string]string{"If-Match": etagV1})
	if rec.Code != http.StatusOK {
		t.Fatalf("second write: %d %s", rec.Code, rec.Body.String())
	}
	if got := hfSyncJSON(t, rec)["generation"]; got != float64(2) {
		t.Fatalf("second write generation = %v, want 2", got)
	}

	// Every accepted write is audited with its counts and generation.
	var writes []string
	for _, line := range readAuditLines(t, store) {
		if decodeAuditLine(t, line)["action"] == "human_manifest_write" {
			writes = append(writes, line)
		}
	}
	if len(writes) != 2 {
		t.Fatalf("human_manifest_write audit lines = %d, want 2: %v", len(writes), writes)
	}
	first := decodeAuditLine(t, writes[0])
	if first["who"] != "alice" || first["target"] != "alice" {
		t.Fatalf("first audit line who/target = %v/%v, want alice/alice", first["who"], first["target"])
	}
	if detailField(t, writes[0], "entries") != float64(2) || detailField(t, writes[0], "tombstones") != float64(1) || detailField(t, writes[0], "generation") != float64(1) {
		t.Fatalf("first audit detail = %v, want entries=2 tombstones=1 generation=1", first["detail"])
	}
	if detailField(t, writes[1], "entries") != float64(1) || detailField(t, writes[1], "tombstones") != float64(2) || detailField(t, writes[1], "generation") != float64(2) {
		t.Fatalf("second audit detail = %s, want entries=1 tombstones=2 generation=2", writes[1])
	}
}

// --- Conditional writes -------------------------------------------------------

func TestHumanSync_ConditionalWrites(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	body := syncManifestBody(nil, nil)

	// A write must carry exactly one conditional header.
	rec := hfPutSync(t, hfh, "alice", alice, body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no conditional header: got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	hfSyncCoded(t, rec, "conditional_write_required")

	rec = hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-Match": "stale", "If-None-Match": "*"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("both conditional headers: got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	hfSyncCoded(t, rec, "conditional_write_required")

	rec = hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-None-Match": "abc"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("If-None-Match other than *: got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	hfSyncCoded(t, rec, "conditional_write_required")

	// The first write creates the manifest.
	rec = hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("first write: %d %s", rec.Code, rec.Body.String())
	}
	etagV1 := hfSyncJSON(t, rec)["etag"].(string)

	// Repeating the first write fails.
	rec = hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("repeat first write: got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	hfSyncCoded(t, rec, "manifest_exists")

	// If-Match against a manifest that does not exist yet fails.
	bob := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Capabilities: []string{string(authpkg.CapabilityFileSync)}}
	rec = hfPutSync(t, hfh, "bob", bob, body, map[string]string{"If-Match": etagV1})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("If-Match on absent manifest: got %d, want 412: %s", rec.Code, rec.Body.String())
	}
	hfSyncCoded(t, rec, "etag_mismatch")

	// A stale If-Match fails once a newer generation is stored.
	rec = hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-Match": etagV1})
	if rec.Code != http.StatusOK {
		t.Fatalf("bump write: %d %s", rec.Code, rec.Body.String())
	}
	etagV2 := hfSyncJSON(t, rec)["etag"].(string)
	if etagV2 == etagV1 {
		t.Fatal("etag did not change between distinct writes")
	}
	rec = hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-Match": etagV1})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: got %d, want 412: %s", rec.Code, rec.Body.String())
	}
	hfSyncCoded(t, rec, "etag_mismatch")

	// The current ETag succeeds.
	rec = hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-Match": etagV2})
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh If-Match: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// --- Size limit ----------------------------------------------------------------

func TestHumanSync_SizeLimit(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	bob := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Capabilities: []string{string(authpkg.CapabilityFileSync)}}

	// A body padded to exactly the 2 MiB cap is still a valid manifest and
	// is accepted; one byte over is 413.
	head := `{"version":1,"entries":{"files/a.txt":"`
	tail := `"}}`
	pad := humanManifestMaxBytes - len(head) - len(tail)
	exact := head + strings.Repeat("x", pad) + tail
	if len(exact) != humanManifestMaxBytes {
		t.Fatalf("test setup: len(exact) = %d", len(exact))
	}
	rec := hfPutSync(t, hfh, "alice", alice, exact, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("exact-limit write: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	over := head + strings.Repeat("x", pad+1) + tail
	rec = hfPutSync(t, hfh, "bob", bob, over, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit write: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	hfSyncCoded(t, rec, "manifest_too_large")

	// The rejected write stored nothing.
	req := hfSyncReq(t, http.MethodGet, "bob", "manifest", bob, "")
	rec = httptest.NewRecorder()
	hfh.getSyncManifest(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("manifest read after 413: got %d, want 404", rec.Code)
	}
}

// --- Structure validation -------------------------------------------------------

func TestHumanSync_InvalidManifest(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	cases := []struct {
		name string
		body string
	}{
		{"entry outside the files/ scope", `{"version":1,"entries":{"notes.txt":{}}}`},
		{"entry memory/ prefix", `{"version":1,"entries":{"memory/x.md":{}}}`},
		{"entry digest/ prefix", `{"version":1,"entries":{"digest/x":{}}}`},
		{"entry .trash/ prefix", `{"version":1,"entries":{".trash/x":{}}}`},
		{"entry .sync/ prefix", `{"version":1,"entries":{".sync/state":{}}}`},
		{"entry traversal out of files/", `{"version":1,"entries":{"files/../memory/x":{}}}`},
		{"bare files/ prefix", `{"version":1,"entries":{"files/":{}}}`},
		{"tombstone outside the files/ scope", `{"version":1,"tombstones":[{"path":"memory/x"}]}`},
		{"unsupported version", `{"version":2,"entries":{}}`},
		{"missing version", `{"entries":{"files/a.txt":{}}}`},
		{"tombstone that is not an object", `{"version":1,"tombstones":["files/a.txt"]}`},
		{"invalid JSON", `{nope`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := hfPutSync(t, hfh, "alice", alice, tc.body, map[string]string{"If-None-Match": "*"})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
			}
			hfSyncCoded(t, rec, "invalid_manifest")
			// Validation rejects before any write: the manifest is absent.
			req := hfSyncReq(t, http.MethodGet, "alice", "manifest", alice, "")
			rec = httptest.NewRecorder()
			hfh.getSyncManifest(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("manifest read after 400: got %d, want 404", rec.Code)
			}
		})
	}
}

// --- Access matrix ---------------------------------------------------------------

func TestHumanSync_AccessMatrix(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	// Seed alice's manifest so an allowed read has content to protect.
	seed := hfPutSync(t, hfh, "alice", hfSyncCaller(authpkg.CapabilityFileSync), syncManifestBody(nil, nil), map[string]string{"If-None-Match": "*"})
	if seed.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", seed.Code, seed.Body.String())
	}

	bobCapable := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Capabilities: []string{string(authpkg.CapabilityFileSync)}}
	cases := []struct {
		name   string
		target string
		caller *authpkg.CallerIdentity
		want   int
	}{
		{"admin reads any", "alice", hfAdmin(), http.StatusOK},
		{"admin reads another", "bob", hfAdmin(), http.StatusOK},
		{"manager reads any", "alice", hfManager(), http.StatusOK},
		{"self with file_sync", "alice", hfSyncCaller(authpkg.CapabilityFileSync), http.StatusOK},
		{"self with full_access", "alice", hfSyncCaller(authpkg.CapabilityFullAccess), http.StatusOK},
		{"self without capability", "alice", hfSelf(), http.StatusForbidden},
		{"other human even with file_sync", "alice", bobCapable, http.StatusNotFound},
		{"team leader", "alice", hfLeader(), http.StatusForbidden},
		{"worker", "alice", hfWorker(), http.StatusForbidden},
		{"no caller identity", "alice", nil, http.StatusNotFound},
		{"admin on a missing human", "ghost", hfAdmin(), http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := hfGetSyncStatus(t, hfh, tc.target, tc.caller)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// The write route enforces the same grant.
	rec := hfPutSync(t, hfh, "alice", hfSelf(), syncManifestBody(nil, nil), map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self write without capability: got %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// Anti-probing: the 404 a foreign human gets on an existing area is
	// byte-identical to the one for a non-existent human.
	recA := hfGetSyncStatus(t, hfh, "alice", hfBob())
	recG := hfGetSyncStatus(t, hfh, "ghost", hfBob())
	if recA.Code != http.StatusNotFound || recG.Code != http.StatusNotFound {
		t.Fatalf("codes = %d/%d, want 404/404", recA.Code, recG.Code)
	}
	if recA.Body.String() != recG.Body.String() {
		t.Fatalf("404 bodies differ:\n%s\nvs\n%s", recA.Body.String(), recG.Body.String())
	}
}

// --- Concurrency -----------------------------------------------------------------

func TestHumanSync_ConcurrentWrites(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)

	rec := hfPutSync(t, hfh, "alice", alice, syncManifestBody([]string{"files/a.txt"}, nil), map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	etagV1 := hfSyncJSON(t, rec)["etag"].(string)

	// Two writers race on the same base ETag: the per-human lease
	// serializes them, so exactly one wins and the loser's If-Match sees
	// the bumped ETag.
	bodyA := syncManifestBody([]string{"files/a.txt", "files/A.txt"}, nil)
	bodyB := syncManifestBody([]string{"files/a.txt", "files/B.txt"}, nil)
	results := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i, body := range []string{bodyA, bodyB} {
		wg.Add(1)
		go func(i int, body string) {
			defer wg.Done()
			results[i] = hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-Match": etagV1})
		}(i, body)
	}
	wg.Wait()

	var winners int
	var winner *httptest.ResponseRecorder
	for _, r := range results {
		switch r.Code {
		case http.StatusOK:
			winners++
			winner = r
		case http.StatusPreconditionFailed:
			hfSyncCoded(t, r, "etag_mismatch")
		default:
			t.Fatalf("concurrent write: got %d: %s", r.Code, r.Body.String())
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if got := hfSyncJSON(t, winner)["generation"]; got != float64(2) {
		t.Fatalf("winner generation = %v, want 2", got)
	}
	etagV2 := hfSyncJSON(t, winner)["etag"].(string)
	if etagV2 == etagV1 {
		t.Fatal("etag did not advance")
	}

	// Generation keeps counting from the winner's base.
	rec = hfPutSync(t, hfh, "alice", alice, syncManifestBody([]string{"files/a.txt"}, nil), map[string]string{"If-Match": etagV2})
	if rec.Code != http.StatusOK {
		t.Fatalf("sequential write: %d %s", rec.Code, rec.Body.String())
	}
	if got := hfSyncJSON(t, rec)["generation"]; got != float64(3) {
		t.Fatalf("sequential generation = %v, want 3", got)
	}

	// The status endpoint agrees on the final generation.
	st := hfSyncJSON(t, hfGetSyncStatus(t, hfh, "alice", alice))
	if st["generation"] != float64(3) {
		t.Fatalf("status generation = %v, want 3", st["generation"])
	}
}

// --- Zero summary ------------------------------------------------------------------

func TestHumanSync_StatusZeroSummary(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	// A human without a manifest gets the zero summary with 200, not an
	// error.
	st := hfSyncJSON(t, hfGetSyncStatus(t, hfh, "alice", hfSyncCaller(authpkg.CapabilityFileSync)))
	if st["entries"] != float64(0) || st["tombstones"] != float64(0) || st["generation"] != float64(0) {
		t.Fatalf("zero summary = %v, want all zeros", st)
	}
	if st["updatedAt"] != "" || st["updatedBy"] != "" {
		t.Fatalf("zero summary updatedAt/updatedBy = %v/%v, want empty", st["updatedAt"], st["updatedBy"])
	}
}
