package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- Test rig --------------------------------------------------------------

// newHumanFilesRig builds the handler against a fake k8s client (humans
// alice, bob, and carol — carol's storage username is "carol-x"), an
// in-memory OSS store, and a live audit client on the same store.
func newHumanFilesRig(t *testing.T) (*humanFilesHandler, *ossfake.Memory, client.Client) {
	t.Helper()
	scheme := newServerTestScheme(t)
	alice := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Alice"},
	}
	bob := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Bob"},
	}
	carol := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "carol", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Carol", Username: "carol-x"},
	}
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(alice, bob, carol).
		Build()
	store := ossfake.NewMemory()
	hfh := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store))
	return hfh, store, k8s
}

func hfCaller(role, username string) *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: role, Username: username}
}

func hfSelf() *authpkg.CallerIdentity    { return hfCaller(authpkg.RoleHuman, "alice") }
func hfBob() *authpkg.CallerIdentity     { return hfCaller(authpkg.RoleHuman, "bob") }
func hfAdmin() *authpkg.CallerIdentity   { return hfCaller(authpkg.RoleAdmin, "admin") }
func hfManager() *authpkg.CallerIdentity { return hfCaller(authpkg.RoleManager, "manager") }
func hfLeader() *authpkg.CallerIdentity  { return hfCaller(authpkg.RoleTeamLeader, "leader-1") }
func hfWorker() *authpkg.CallerIdentity  { return hfCaller(authpkg.RoleWorker, "worker-1") }

// hfReq builds a request with the human-files path values and an injected
// caller identity. path may be "" (the files root).
func hfReq(t *testing.T, method, name, path string, caller *authpkg.CallerIdentity, body io.Reader) *http.Request {
	t.Helper()
	target := "/api/v1/humans/" + name + "/files"
	if path != "" {
		target += "/" + path
	}
	req := httptest.NewRequest(method, target, body)
	req.SetPathValue("name", name)
	req.SetPathValue("path", path)
	if caller != nil {
		req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
	}
	return req
}

// hfTrashReq builds a request against the trash subroutes; id is set as the
// "id" path value.
func hfTrashReq(t *testing.T, method, name, id string, caller *authpkg.CallerIdentity, query string) *http.Request {
	t.Helper()
	req := hfReq(t, method, name, "", caller, nil)
	req.SetPathValue("id", id)
	if query != "" {
		req.URL.RawQuery = query
	}
	return req
}

func hfPutFile(t *testing.T, hfh *humanFilesHandler, name, path string, caller *authpkg.CallerIdentity, content string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	hfh.putFile(rec, hfReq(t, http.MethodPut, name, path, caller, strings.NewReader(content)))
	return rec
}

// decodeAuditLine parses one JSONL line into a generic map.
func decodeAuditLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("audit line parse: %v: %s", err, line)
	}
	return m
}

func detailField(t *testing.T, line string, key string) any {
	t.Helper()
	m := decodeAuditLine(t, line)
	var d map[string]any
	if err := json.Unmarshal([]byte(m["detail"].(string)), &d); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	v, ok := d[key]
	if !ok {
		t.Fatalf("detail missing %q: %s", key, m["detail"])
	}
	return v
}

// --- Path validation --------------------------------------------------------

func TestValidateHumanFilePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"root", "", "", true},
		{"plain file", "a.txt", "a.txt", true},
		{"nested", "docs/notes.md", "docs/notes.md", true},
		{"dir form strips trailing slash", "docs/", "docs", true},
		{"dotfile allowed", ".env.local", ".env.local", true},
		{"nfc precomposed", "café.md", "café.md", true},
		{"nfd normalized to nfc", "cafe\u0301.md", "café.md", true},
		{"absolute rejected", "/etc/passwd", "", false},
		{"dotdot rejected", "a/../b", "", false},
		{"dot segment rejected", "a/./b", "", false},
		{"empty segment rejected", "a//b", "", false},
		{"backslash rejected", "a\\b", "", false},
		{"control char rejected", "a\x00b", "", false},
		{"path too long", strings.Repeat("x", 1025), "", false},
		{"segment too long", "a/" + strings.Repeat("x", 256), "", false},
		{"reserved .trash rejected", ".trash/a.txt", "", false},
		{"reserved .trash exact", ".trash", "", false},
		{"reserved .sync rejected", ".sync/state.json", "", false},
		{"windows reserved CON", "CON", "", false},
		{"windows reserved with ext", "con.txt", "", false},
		{"windows reserved COM1 case-insensitive", "COM1", "", false},
		{"windows reserved LPT9 ext", "LPT9.log", "", false},
		{"not reserved with suffix", "myCON.txt", "myCON.txt", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateHumanFilePath(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("validateHumanFilePath(%q) unexpected error: %v", tc.in, err)
				}
				if got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			} else {
				if err == nil {
					t.Fatalf("validateHumanFilePath(%q) expected error, got %q", tc.in, got)
				}
			}
		})
	}
}

// --- List shape -------------------------------------------------------------

func TestHumanFiles_ListShape(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	ctx := context.Background()

	// Seed the area: a root file, a directory with a file, a nested
	// directory, and a subdirectory whose only content is the .keep marker.
	for _, put := range []struct{ path, content string }{
		{"a.txt", "hello"},
		{"docs/b.txt", "world!"},
		{"docs/nested/c.txt", "x"},
		{"sub/x.txt", "y"},
	} {
		rec := hfPutFile(t, hfh, "alice", put.path, hfAdmin(), put.content)
		if rec.Code != http.StatusOK {
			t.Fatalf("seed %s: %d %s", put.path, rec.Code, rec.Body.String())
		}
	}
	if err := store.PutObject(ctx, "humans/alice/files/sub/.keep", []byte("")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list root: %d %s", rec.Code, rec.Body.String())
	}
	var root humanFileListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if root.Path != "files/" {
		t.Fatalf("path = %q, want files/", root.Path)
	}
	if len(root.Entries) != 3 {
		t.Fatalf("root entries = %+v, want a.txt + docs + sub", root.Entries)
	}
	wantRoot := map[string]struct {
		typ  string
		size int64
	}{
		"a.txt": {"file", 5},
		"docs":  {"directory", 0},
		"sub":   {"directory", 0},
	}
	for i, e := range root.Entries {
		if i > 0 && root.Entries[i-1].Name >= e.Name {
			t.Fatalf("entries not sorted: %+v", root.Entries)
		}
		w, ok := wantRoot[e.Name]
		if !ok {
			t.Fatalf("unexpected entry %q (marker leaked?): %+v", e.Name, root.Entries)
		}
		if e.Type != w.typ {
			t.Fatalf("entry %q type = %q, want %q", e.Name, e.Type, w.typ)
		}
		if e.Size != w.size {
			t.Fatalf("entry %q size = %d, want %d", e.Name, e.Size, w.size)
		}
		if e.UpdatedAt == "" {
			t.Fatalf("entry %q missing updatedAt", e.Name)
		}
	}

	// One level down: b.txt file + nested directory.
	rec = httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "docs/", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list docs: %d %s", rec.Code, rec.Body.String())
	}
	var docs humanFileListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &docs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if docs.Path != "files/docs/" {
		t.Fatalf("path = %q, want files/docs/", docs.Path)
	}
	if len(docs.Entries) != 2 || docs.Entries[0].Name != "b.txt" || docs.Entries[0].Type != "file" || docs.Entries[0].Size != 6 ||
		docs.Entries[1].Name != "nested" || docs.Entries[1].Type != "directory" {
		t.Fatalf("docs entries = %+v", docs.Entries)
	}

	// An empty directory lists as an empty array (not null).
	rec = httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "empty/", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list empty: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Fatalf("empty dir body = %s, want \"entries\":[]", rec.Body.String())
	}
}

// --- Access: self, admin, manager, and the 404 wall --------------------------

func TestHumanFiles_SelfAccess(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	rec := hfPutFile(t, hfh, "alice", "notes.txt", hfSelf(), "hello self")
	if rec.Code != http.StatusOK {
		t.Fatalf("self put: %d %s", rec.Code, rec.Body.String())
	}
	var putResp struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("decode put: %v", err)
	}
	if putResp.Path != "files/notes.txt" || putResp.Size != 10 || putResp.ETag == "" {
		t.Fatalf("put response = %+v", putResp)
	}

	rec = httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "notes.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("self get: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello self" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag header on file read")
	}

	// A human whose Matrix username matches the CR's EffectiveUsername is
	// self; the metadata name alone is not enough.
	rec = hfPutFile(t, hfh, "carol", "a.txt", hfCaller(authpkg.RoleHuman, "carol-x"), "x")
	if rec.Code != http.StatusOK {
		t.Fatalf("carol-x self put: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "carol", "a.txt", hfCaller(authpkg.RoleHuman, "carol-x"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("carol-x self get: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHumanFiles_ForeignCallersGet404(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	// Give alice a file first so a leak would be detectable.
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "secret"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d", rec.Code)
	}

	deny := []struct {
		name   string
		caller *authpkg.CallerIdentity
	}{
		{"another human", hfBob()},
		{"human with username mismatch", hfCaller(authpkg.RoleHuman, "carol")},
		{"team leader", hfLeader()},
		{"worker", hfWorker()},
		{"unauthenticated (no identity in context)", nil},
	}
	for _, tc := range deny {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", tc.caller, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET: got %d, want 404: %s", rec.Code, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			hfh.putFile(rec, hfReq(t, http.MethodPut, "alice", "a.txt", tc.caller, strings.NewReader("x")))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("PUT: got %d, want 404", rec.Code)
			}
			rec = httptest.NewRecorder()
			hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "a.txt", tc.caller, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("DELETE: got %d, want 404", rec.Code)
			}
			rec = httptest.NewRecorder()
			hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", tc.caller, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("trash list: got %d, want 404", rec.Code)
			}
		})
	}

	// Existence probing: bob's response must be identical to the response
	// for a non-existent human.
	r1 := httptest.NewRecorder()
	hfh.getFile(r1, hfReq(t, http.MethodGet, "alice", "a.txt", hfBob(), nil))
	r2 := httptest.NewRecorder()
	hfh.getFile(r2, hfReq(t, http.MethodGet, "ghost", "a.txt", hfBob(), nil))
	if r1.Code != r2.Code || r1.Body.String() != r2.Body.String() {
		t.Fatalf("probe leak: existing=%d %q vs missing=%d %q",
			r1.Code, r1.Body.String(), r2.Code, r2.Body.String())
	}

	// An admin asking for a non-existent human also gets 404.
	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "ghost", "", hfAdmin(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin + ghost: got %d, want 404", rec.Code)
	}
}

func TestHumanFiles_AdminAndManagerAccess(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	for _, caller := range []*authpkg.CallerIdentity{hfAdmin(), hfManager()} {
		who := caller.Username
		t.Run(who, func(t *testing.T) {
			rec := hfPutFile(t, hfh, "alice", "by-"+who+".txt", caller, "from "+who)
			if rec.Code != http.StatusOK {
				t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "by-"+who+".txt", caller, nil))
			if rec.Code != http.StatusOK || rec.Body.String() != "from "+who {
				t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "", caller, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
			}
			// Soft delete + restore are within manager/admin write access.
			rec = httptest.NewRecorder()
			hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "by-"+who+".txt", caller, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- Trash lifecycle ----------------------------------------------------------

func TestHumanFiles_TrashLifecycle(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	ctx := context.Background()

	// Soft delete: file moves to .trash/<id>/.
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "hello"); rec.Code != http.StatusOK {
		t.Fatalf("put: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	var delResp struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	id1 := delResp.TrashID
	if !trashIDRegex.MatchString(id1) {
		t.Fatalf("trash id %q does not match the id format", id1)
	}
	// Original is gone; trash content + metadata are present.
	if _, err := store.GetObject(ctx, "humans/alice/files/a.txt"); err == nil {
		t.Fatal("original still present after soft delete")
	}
	content, err := store.GetObject(ctx, "humans/alice/.trash/"+id1+"/content")
	if err != nil || string(content) != "hello" {
		t.Fatalf("trash content missing or wrong: %v %q", err, content)
	}
	metaData, err := store.GetObject(ctx, "humans/alice/.trash/"+id1+"/meta.json")
	if err != nil {
		t.Fatal("trash meta.json missing")
	}
	var meta trashMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("meta parse: %v", err)
	}
	if meta.Path != "files/a.txt" || meta.Size != 5 || meta.Caller != "alice" || meta.Role != "human" || meta.DeletedAt == "" {
		t.Fatalf("meta = %+v", meta)
	}

	// Trash listing shows the entry with its metadata.
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash: %d %s", rec.Code, rec.Body.String())
	}
	var trashResp struct {
		Entries []trashEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trashResp); err != nil {
		t.Fatalf("decode trash: %v", err)
	}
	if len(trashResp.Entries) != 1 {
		t.Fatalf("trash entries = %+v, want 1", trashResp.Entries)
	}
	e := trashResp.Entries[0]
	if e.ID != id1 || e.Path != "files/a.txt" || e.Size != 5 || e.Caller != "alice" || e.Role != "human" || e.DeletedAt == "" {
		t.Fatalf("trash entry = %+v", e)
	}

	// Restore puts the file back and removes the trash entry.
	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", id1, hfSelf(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("restored file: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list after restore: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trashResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(trashResp.Entries) != 0 {
		t.Fatalf("trash should be empty after restore: %+v", trashResp.Entries)
	}

	// Restore conflict: the target exists again.
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "new"); rec.Code != http.StatusOK {
		t.Fatalf("re-put: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-delete: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatal(err)
	}
	id2 := delResp.TrashID
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "conflict"); rec.Code != http.StatusOK {
		t.Fatalf("conflict put: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", id2, hfSelf(), ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("restore over existing: got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var conflict humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Code != "path_conflict" {
		t.Fatalf("conflict code = %q, want path_conflict", conflict.Code)
	}
	// A non-admin cannot force the overwrite.
	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", id2, hfManager(), "overwrite=1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("manager overwrite: got %d, want 403", rec.Code)
	}
	// The admin can.
	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", id2, hfAdmin(), "overwrite=1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin overwrite restore: %d %s", rec.Code, rec.Body.String())
	}
	data, err := store.GetObject(ctx, "humans/alice/files/a.txt")
	if err != nil || string(data) != "new" {
		t.Fatalf("overwritten file = %q (err %v), want trashed content %q", data, err, "new")
	}

	// Admin purge removes an entry for good.
	if rec := hfPutFile(t, hfh, "alice", "b.txt", hfSelf(), "bye"); rec.Code != http.StatusOK {
		t.Fatalf("put b: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "b.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete b: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatal(err)
	}
	id3 := delResp.TrashID
	rec = httptest.NewRecorder()
	hfh.purgeTrash(rec, hfTrashReq(t, http.MethodDelete, "alice", id3, hfAdmin(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("purge: %d %s", rec.Code, rec.Body.String())
	}
	var purged struct {
		ID string `json:"purged"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &purged); err != nil {
		t.Fatal(err)
	}
	if purged.ID != id3 {
		t.Fatalf("purged = %q, want %q", purged.ID, id3)
	}
	if err := store.Stat(ctx, "humans/alice/.trash/"+id3+"/content"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purged entry still present (err=%v)", err)
	}
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list after purge: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trashResp); err != nil {
		t.Fatal(err)
	}
	if len(trashResp.Entries) != 0 {
		t.Fatalf("trash should be empty after purge: %+v", trashResp.Entries)
	}
}

func TestHumanFiles_PurgeIsAdminOnly(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "c.txt", hfSelf(), "x"); rec.Code != http.StatusOK {
		t.Fatalf("put: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "c.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	var delResp struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatal(err)
	}
	id := delResp.TrashID

	for _, tc := range []struct {
		name   string
		caller *authpkg.CallerIdentity
		want   int
	}{
		{"self", hfSelf(), http.StatusForbidden},
		{"manager", hfManager(), http.StatusForbidden},
		{"another human", hfBob(), http.StatusNotFound},
		{"team leader", hfLeader(), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			hfh.purgeTrash(rec, hfTrashReq(t, http.MethodDelete, "alice", id, tc.caller, ""))
			if rec.Code != tc.want {
				t.Fatalf("purge: got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	// The entry survives the denied attempts.
	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash: %d", rec.Code)
	}
	var trashResp struct {
		Entries []trashEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trashResp); err != nil {
		t.Fatal(err)
	}
	if len(trashResp.Entries) != 1 || trashResp.Entries[0].ID != id {
		t.Fatalf("entry should survive denied purges: %+v", trashResp.Entries)
	}
}

// --- ETag concurrency ---------------------------------------------------------

func TestHumanFiles_IfMatchConflict(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	rec := hfPutFile(t, hfh, "alice", "shared.txt", hfSelf(), "v1")
	if rec.Code != http.StatusOK {
		t.Fatalf("put v1: %d", rec.Code)
	}
	var putResp struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatal(err)
	}
	etagV1 := putResp.ETag
	// A concurrent writer bumps the ETag.
	rec = hfPutFile(t, hfh, "alice", "shared.txt", hfAdmin(), "v2")
	if rec.Code != http.StatusOK {
		t.Fatalf("put v2: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatal(err)
	}
	etagV2 := putResp.ETag
	if etagV1 == etagV2 {
		t.Fatal("etag did not change between distinct contents")
	}

	putWithMatch := func(caller *authpkg.CallerIdentity, content, match string) *httptest.ResponseRecorder {
		req := hfReq(t, http.MethodPut, "alice", "shared.txt", caller, strings.NewReader(content))
		req.Header.Set("If-Match", match)
		r := httptest.NewRecorder()
		hfh.putFile(r, req)
		return r
	}

	// Stale ETag -> 412 with the stable code.
	rec = putWithMatch(hfSelf(), "v3", etagV1)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: got %d, want 412: %s", rec.Code, rec.Body.String())
	}
	var coded humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &coded); err != nil {
		t.Fatal(err)
	}
	if coded.Code != "etag_mismatch" {
		t.Fatalf("code = %q, want etag_mismatch", coded.Code)
	}

	// Current ETag -> success.
	rec = putWithMatch(hfSelf(), "v3", etagV2)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh If-Match: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// If-Match against a file that does not exist yet -> 412.
	rec = putWithMatch(hfSelf(), "fresh", "anything")
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("If-Match on absent file: got %d, want 412: %s", rec.Code, rec.Body.String())
	}
}

// --- Size limit -----------------------------------------------------------------

func TestHumanFiles_SizeLimit(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	hfh.maxFileBytes = 10

	rec := hfPutFile(t, hfh, "alice", "big.txt", hfSelf(), "0123456789A") // 11 bytes
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit put: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	var coded humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &coded); err != nil {
		t.Fatal(err)
	}
	if coded.Code != "size_exceeded" {
		t.Fatalf("code = %q, want size_exceeded", coded.Code)
	}
	// Exactly at the limit is allowed.
	rec = hfPutFile(t, hfh, "alice", "exact.txt", hfSelf(), "0123456789")
	if rec.Code != http.StatusOK {
		t.Fatalf("at-limit put: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// --- Audit trail -----------------------------------------------------------------

func TestHumanFiles_AuditEvents(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)

	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "hello"); rec.Code != http.StatusOK {
		t.Fatalf("put: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	var delResp struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatal(err)
	}
	id := delResp.TrashID
	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", id, hfSelf(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: %d", rec.Code)
	}
	// Purge needs one more delete.
	rec = httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-delete: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatal(err)
	}
	id2 := delResp.TrashID
	rec = httptest.NewRecorder()
	hfh.purgeTrash(rec, hfTrashReq(t, http.MethodDelete, "alice", id2, hfAdmin(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("purge: %d", rec.Code)
	}

	lines := readAuditLines(t, store)
	// write + delete + restore + delete + purge = 5 lines.
	if len(lines) != 5 {
		t.Fatalf("want 5 audit lines, got %d: %s", len(lines), strings.Join(lines, " | "))
	}

	check := func(i int, who, role, action string) map[string]any {
		t.Helper()
		m := decodeAuditLine(t, lines[i])
		if m["who"] != who || m["role"] != role || m["target"] != "alice" || m["action"] != action {
			t.Fatalf("line %d = who=%v role=%v target=%v action=%v; want who=%s role=%s target=alice action=%s",
				i+1, m["who"], m["role"], m["target"], m["action"], who, role, action)
		}
		if m["when"] == nil || m["when"] == "" {
			t.Fatalf("line %d missing when: %s", i+1, lines[i])
		}
		return m
	}

	check(0, "alice", "human", "human_file_write")
	if v := detailField(t, lines[0], "path"); v != "files/a.txt" {
		t.Fatalf("write detail path = %v", v)
	}
	if v := detailField(t, lines[0], "size"); v != float64(5) {
		t.Fatalf("write detail size = %v, want 5", v)
	}
	if v := detailField(t, lines[0], "etag"); v == "" || v == nil {
		t.Fatalf("write detail missing etag: %s", lines[0])
	}

	check(1, "alice", "human", "human_file_delete")
	if v := detailField(t, lines[1], "path"); v != "files/a.txt" {
		t.Fatalf("delete detail path = %v", v)
	}
	if v := detailField(t, lines[1], "trashId"); v != id {
		t.Fatalf("delete detail trashId = %v, want %s", v, id)
	}

	check(2, "alice", "human", "human_trash_restore")
	if v := detailField(t, lines[2], "trashId"); v != id {
		t.Fatalf("restore detail trashId = %v, want %s", v, id)
	}
	if v := detailField(t, lines[2], "overwrite"); v != false {
		t.Fatalf("restore detail overwrite = %v, want false", v)
	}

	// Line 3 is the second delete; line 4 is the admin purge.
	check(4, "admin", "admin", "human_trash_purge")
	if v := detailField(t, lines[4], "trashId"); v != id2 {
		t.Fatalf("purge detail trashId = %v, want %s", v, id2)
	}
	if v := detailField(t, lines[4], "path"); v != "files/a.txt" {
		t.Fatalf("purge detail path = %v", v)
	}
}

// --- Quota --------------------------------------------------------------------

// hfGetQuota runs GET quota and returns the recorder.
func hfGetQuota(t *testing.T, hfh *humanFilesHandler, name string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	hfh.getQuota(rec, hfReq(t, http.MethodGet, name, "", caller, nil))
	return rec
}

func hfPutQuota(t *testing.T, hfh *humanFilesHandler, name string, caller *authpkg.CallerIdentity, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	hfh.putQuota(rec, hfReq(t, http.MethodPut, name, "", caller, strings.NewReader(body)))
	return rec
}

func TestHumanFiles_QuotaUnsetIsUnlimited(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	// No quota configured: a write of any size is allowed.
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "0123456789"); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rec := hfGetQuota(t, hfh, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	// quotaBytes must be present as null (not omitted) to signal unlimited.
	if !strings.Contains(rec.Body.String(), `"usedBytes":10`) || !strings.Contains(rec.Body.String(), `"quotaBytes":null`) {
		t.Fatalf("quota body = %s, want usedBytes:10 and quotaBytes:null", rec.Body.String())
	}
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 10 || q.QuotaBytes != nil {
		t.Fatalf("quota = %+v, want usedBytes 10 / quota nil", q)
	}
}

func TestHumanFiles_QuotaEnforcement(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	getUsed := func() int64 {
		t.Helper()
		rec := hfGetQuota(t, hfh, "alice", hfSelf())
		if rec.Code != http.StatusOK {
			t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
		}
		var q humanQuotaResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
			t.Fatal(err)
		}
		return q.UsedBytes
	}

	// Seed 10 bytes, then the admin sets a 20-byte quota through the API.
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "0123456789"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":20}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}
	rec = hfGetQuota(t, hfh, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 10 || q.QuotaBytes == nil || *q.QuotaBytes != 20 {
		t.Fatalf("quota = %+v, want usedBytes 10 / quota 20", q)
	}

	// 10 + 15 > 20 -> 413 quota_exceeded and the file must not be written.
	rec = hfPutFile(t, hfh, "alice", "b.txt", hfSelf(), strings.Repeat("x", 15))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota put: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	var coded humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &coded); err != nil {
		t.Fatal(err)
	}
	if coded.Code != "quota_exceeded" {
		t.Fatalf("code = %q, want quota_exceeded", coded.Code)
	}
	if got := getUsed(); got != 10 {
		t.Fatalf("usedBytes after rejected write = %d, want 10", got)
	}

	// 10 + 10 == 20 (exactly at the quota) -> 200; then any further byte -> 413.
	if rec := hfPutFile(t, hfh, "alice", "b.txt", hfSelf(), strings.Repeat("x", 10)); rec.Code != http.StatusOK {
		t.Fatalf("at-quota put: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := getUsed(); got != 20 {
		t.Fatalf("usedBytes at quota = %d, want 20", got)
	}
	rec = hfPutFile(t, hfh, "alice", "c.txt", hfSelf(), "y")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("one byte over quota: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &coded); err != nil {
		t.Fatal(err)
	}
	if coded.Code != "quota_exceeded" {
		t.Fatalf("code = %q, want quota_exceeded", coded.Code)
	}
}

func TestHumanFiles_QuotaRestoreEnforcement(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	// Soft-delete a file, then pin the quota exactly at the current usage
	// (files/ is now empty, so the used bytes are the trash entry).
	if rec := hfPutFile(t, hfh, "alice", "f.txt", hfSelf(), "0123456789"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "f.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	var delResp struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatal(err)
	}

	rec = hfGetQuota(t, hfh, "alice", hfSelf())
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes == 0 {
		t.Fatal("trash usage must count toward the quota")
	}
	rec = hfPutQuota(t, hfh, "alice", hfAdmin(), fmt.Sprintf(`{"quotaBytes":%d}`, q.UsedBytes))
	if rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}

	// Restoring the 10 bytes on top of a full quota -> 413, and the trash
	// entry must survive the failed restore.
	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", delResp.TrashID, hfSelf(), ""))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota restore: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	var coded humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &coded); err != nil {
		t.Fatal(err)
	}
	if coded.Code != "quota_exceeded" {
		t.Fatalf("code = %q, want quota_exceeded", coded.Code)
	}
	list := httptest.NewRecorder()
	hfh.listTrash(list, hfTrashReq(t, http.MethodGet, "alice", "", hfSelf(), ""))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), delResp.TrashID) {
		t.Fatalf("trash entry lost after rejected restore: %d %s", list.Code, list.Body.String())
	}
}

func TestHumanFiles_QuotaAuthorization(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	// Setting the quota is admin-only: authorized non-admin callers get
	// 403, non-access callers get the same 404 as a missing human.
	rec := hfPutQuota(t, hfh, "alice", hfSelf(), `{"quotaBytes":100}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self put quota: got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	rec = hfPutQuota(t, hfh, "alice", hfManager(), `{"quotaBytes":100}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("manager put quota: got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	rec = hfPutQuota(t, hfh, "alice", hfBob(), `{"quotaBytes":100}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign put quota: got %d, want 404: %s", rec.Code, rec.Body.String())
	}

	// Reading the quota follows the normal access matrix.
	if rec := hfGetQuota(t, hfh, "alice", hfSelf()); rec.Code != http.StatusOK {
		t.Fatalf("self get quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfGetQuota(t, hfh, "alice", hfAdmin()); rec.Code != http.StatusOK {
		t.Fatalf("admin get quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfGetQuota(t, hfh, "alice", hfBob()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign get quota: got %d, want 404", rec.Code)
	}
	if rec := hfGetQuota(t, hfh, "alice", hfWorker()); rec.Code != http.StatusNotFound {
		t.Fatalf("worker get quota: got %d, want 404", rec.Code)
	}
	if rec := hfGetQuota(t, hfh, "ghost", hfAdmin()); rec.Code != http.StatusNotFound {
		t.Fatalf("ghost get quota: got %d, want 404", rec.Code)
	}
}

func TestHumanFiles_QuotaValidationAndReset(t *testing.T) {
	hfh, _, k8s := newHumanFilesRig(t)

	// Negative quota -> 400; invalid JSON -> 400.
	rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":-5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative quota: got %d, want 400: %s", rec.Code, rec.Body.String())
	}
	rec = hfPutQuota(t, hfh, "alice", hfAdmin(), `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: got %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Set a quota, verify it landed on the CR, then clear it with null.
	rec = hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":20}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}
	var human v1beta1.Human
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: "alice", Namespace: "default"}, &human); err != nil {
		t.Fatal(err)
	}
	if human.Spec.WorkspaceQuotaBytes == nil || *human.Spec.WorkspaceQuotaBytes != 20 {
		t.Fatalf("CR quota = %v, want 20", human.Spec.WorkspaceQuotaBytes)
	}
	rec = hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset quota: %d %s", rec.Code, rec.Body.String())
	}
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: "alice", Namespace: "default"}, &human); err != nil {
		t.Fatal(err)
	}
	if human.Spec.WorkspaceQuotaBytes != nil {
		t.Fatalf("CR quota after reset = %v, want nil", *human.Spec.WorkspaceQuotaBytes)
	}
	if rec := hfGetQuota(t, hfh, "alice", hfSelf()); !strings.Contains(rec.Body.String(), `"quotaBytes":null`) {
		t.Fatalf("quota after reset = %s, want quotaBytes:null", rec.Body.String())
	}
}

func TestHumanFiles_QuotaAuditEvent(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)

	rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":42}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}
	lines := readAuditLines(t, store)
	if len(lines) != 1 {
		t.Fatalf("want 1 audit line, got %d: %s", len(lines), strings.Join(lines, " | "))
	}
	m := decodeAuditLine(t, lines[0])
	if m["who"] != "admin" || m["role"] != "admin" || m["target"] != "alice" || m["action"] != "human_quota_set" {
		t.Fatalf("audit line = who=%v role=%v target=%v action=%v", m["who"], m["role"], m["target"], m["action"])
	}
	if v := detailField(t, lines[0], "quotaBytes"); v != float64(42) {
		t.Fatalf("detail quotaBytes = %v, want 42", v)
	}
}
