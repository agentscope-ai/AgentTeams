package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
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
		{"reserved .control rejected", ".control/quota.json", "", false},
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

// TestHumanFiles_QuotaReplacementCharge pins the #1426 fix: a PUT that
// replaces an existing object charges only the delta (newSize-oldSize),
// not the full new size, because used already counts the old object.
func TestHumanFiles_QuotaReplacementCharge(t *testing.T) {
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
	codedCode := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		var coded humanFileCodedError
		if err := json.Unmarshal(rec.Body.Bytes(), &coded); err != nil {
			t.Fatal(err)
		}
		return coded.Code
	}
	getBody := func(path string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", path, hfSelf(), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("get %s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// Pin the quota at 10 bytes before anything is written.
	if rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}

	// New files keep the original semantics: empty area + an 11-byte
	// file over the 10-byte quota -> 413, nothing written.
	rec := hfPutFile(t, hfh, "alice", "g.txt", hfSelf(), strings.Repeat("n", 11))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("new over-quota file: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if code := codedCode(rec); code != "quota_exceeded" {
		t.Fatalf("code = %q, want quota_exceeded", code)
	}
	if got := getUsed(); got != 0 {
		t.Fatalf("used after rejected new file = %d, want 0", got)
	}

	// Maintainer repro: fill the quota exactly (10B -> 200), then
	// overwrite with a 1-byte file. The old check charged 10+1 and
	// rejected the shrink; the replacement delta is 1-10, so it is 200.
	rec = hfPutFile(t, hfh, "alice", "f.txt", hfSelf(), strings.Repeat("a", 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed 10B: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = hfPutFile(t, hfh, "alice", "f.txt", hfSelf(), "b")
	if rec.Code != http.StatusOK {
		t.Fatalf("shrink overwrite at quota: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := getUsed(); got != 1 {
		t.Fatalf("used after shrink = %d, want 1", got)
	}

	// Same-size overwrite at the quota: 10B -> 10B is a zero delta -> 200.
	if rec := hfPutFile(t, hfh, "alice", "f.txt", hfSelf(), strings.Repeat("c", 10)); rec.Code != http.StatusOK {
		t.Fatalf("grow back to 10B: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = hfPutFile(t, hfh, "alice", "f.txt", hfSelf(), strings.Repeat("d", 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("same-size overwrite at quota: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := getUsed(); got != 10 {
		t.Fatalf("used after same-size overwrite = %d, want 10", got)
	}

	// Growth past the quota: 10B -> 11B with used at the quota -> 413,
	// and the rejection touches neither used nor the file.
	rec = hfPutFile(t, hfh, "alice", "f.txt", hfSelf(), strings.Repeat("e", 11))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("grow over quota: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if code := codedCode(rec); code != "quota_exceeded" {
		t.Fatalf("code = %q, want quota_exceeded", code)
	}
	if got := getUsed(); got != 10 {
		t.Fatalf("used after rejected grow = %d, want 10", got)
	}
	if body := getBody("f.txt"); body != strings.Repeat("d", 10) {
		t.Fatalf("rejected grow changed the file: %q", body)
	}

	// Growth that lands exactly on the quota: 6B -> 10B -> 200.
	if rec := hfPutFile(t, hfh, "alice", "f.txt", hfSelf(), strings.Repeat("f", 6)); rec.Code != http.StatusOK {
		t.Fatalf("shrink to 6B: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = hfPutFile(t, hfh, "alice", "f.txt", hfSelf(), strings.Repeat("g", 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("grow to exactly quota: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := getUsed(); got != 10 {
		t.Fatalf("used at quota = %d, want 10", got)
	}
	var etagAtQuota struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &etagAtQuota); err != nil {
		t.Fatal(err)
	}

	// If-Match conflict: an external writer replaces the file (same
	// size, so used stays 10); the stale-ETag overwrite then fails with
	// 412 and leaves used and the file untouched.
	external := strings.Repeat("x", 10)
	if rec := hfPutFile(t, hfh, "alice", "f.txt", hfAdmin(), external); rec.Code != http.StatusOK {
		t.Fatalf("external put: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	req := hfReq(t, http.MethodPut, "alice", "f.txt", hfSelf(), strings.NewReader("h"))
	req.Header.Set("If-Match", etagAtQuota.ETag)
	rec = httptest.NewRecorder()
	hfh.putFile(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: got %d, want 412: %s", rec.Code, rec.Body.String())
	}
	if code := codedCode(rec); code != "etag_mismatch" {
		t.Fatalf("code = %q, want etag_mismatch", code)
	}
	if got := getUsed(); got != 10 {
		t.Fatalf("used after 412 = %d, want 10", got)
	}
	if body := getBody("f.txt"); body != external {
		t.Fatalf("rejected If-Match write changed the file: %q", body)
	}
}

func TestHumanFiles_QuotaRestoreDoesNotGrowUsage(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	// Soft-delete a file, then pin the quota exactly at the current usage.
	// The trash bytes are already counted, so restoring them is net-neutral
	// and must not be rejected at exactly the quota.
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

	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", delResp.TrashID, hfSelf(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore at exact quota: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = hfGetQuota(t, hfh, "alice", hfSelf())
	var after humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.UsedBytes > q.UsedBytes {
		t.Fatalf("restore grew usage: before=%d after=%d", q.UsedBytes, after.UsedBytes)
	}
	rec = httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "f.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("restored file: %d %q", rec.Code, rec.Body.String())
	}
	list := httptest.NewRecorder()
	hfh.listTrash(list, hfTrashReq(t, http.MethodGet, "alice", "", hfSelf(), ""))
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), delResp.TrashID) {
		t.Fatalf("trash entry survived restore: %d %s", list.Code, list.Body.String())
	}
}

func TestHumanFiles_QuotaRestoreWhenAlreadyOver(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

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
	// A quota lowered below the current usage must not block a net-neutral
	// restore; only a further write would be rejected.
	rec = hfPutQuota(t, hfh, "alice", hfAdmin(), fmt.Sprintf(`{"quotaBytes":%d}`, q.UsedBytes-1))
	if rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", delResp.TrashID, hfSelf(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore while already over quota: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = hfGetQuota(t, hfh, "alice", hfSelf())
	var after humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.UsedBytes > q.UsedBytes {
		t.Fatalf("restore grew usage: before=%d after=%d", q.UsedBytes, after.UsedBytes)
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

// A write made while quota is disabled must not leave the ledger at its old
// value when the quota is re-enabled. Without the transition handling, the
// ledger says 6 while live usage is 12+, so a later 4-byte write is admitted.
func TestHumanFiles_QuotaDisabledWriteThenReenable(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	if rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "aaaaaa"); rec.Code != http.StatusOK {
		t.Fatalf("first write: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), strings.Repeat("b", 12)); rec.Code != http.StatusOK {
		t.Fatalf("unlimited overwrite: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("re-enable quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, hfh, "alice", "b.txt", hfSelf(), "cccc"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("post-reenable write: got %d, want 413: %s", rec.Code, rec.Body.String())
	}

	rec := hfGetQuota(t, hfh, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 12 {
		t.Fatalf("usedBytes=%d, want 12 (the rejected 4-byte write must not land)", q.UsedBytes)
	}
}

// TestHumanFiles_QuotaSamePathReplacementReservations covers two replacements
// of the same object: both observed oldSize=10 before either PUT landed. The
// second reservation must build on the first reservation's target size, not
// subtract the same old 10 bytes again.
func TestHumanFiles_QuotaSamePathReplacementReservations(t *testing.T) {
	h1, store, k8s := newHumanFilesRig(t)
	h2 := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store))

	if rec := hfPutQuota(t, h1, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h1, "alice", "first.txt", hfSelf(), strings.Repeat("a", 10)); rec.Code != http.StatusOK {
		t.Fatalf("seed file: %d %s", rec.Code, rec.Body.String())
	}

	req1 := hfReq(t, http.MethodPut, "alice", "first.txt", hfSelf(), nil)
	a1, status, msg := h1.authorizeHumanFile(req1.Context(), "alice", false)
	if a1 == nil {
		t.Fatalf("authorize h1: %d %s", status, msg)
	}
	token1, status, msg := h1.reserveQuota(req1.Context(), a1, "alice", "first.txt", 10, 1)
	if status != 0 || token1 == "" {
		t.Fatalf("first reservation: status=%d token=%q msg=%s", status, token1, msg)
	}

	req2 := hfReq(t, http.MethodPut, "alice", "first.txt", hfSelf(), nil)
	a2, status, msg := h2.authorizeHumanFile(req2.Context(), "alice", false)
	if a2 == nil {
		t.Fatalf("authorize h2: %d %s", status, msg)
	}
	token2, status, msg := h2.reserveQuota(req2.Context(), a2, "alice", "first.txt", 10, 1)
	if status != 0 || token2 == "" {
		t.Fatalf("second reservation: status=%d token=%q msg=%s", status, token2, msg)
	}

	// The second replacement lands first; the first replacement commits after
	// it. The ledger must still count the one-byte object, not zero.
	if err := h2.oss.PutObject(req2.Context(), humanFilesPrefix("alice")+"first.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	h2.commitQuota(req2.Context(), "alice", "first.txt", token2)
	if err := h1.oss.PutObject(req1.Context(), humanFilesPrefix("alice")+"first.txt", []byte("c")); err != nil {
		t.Fatal(err)
	}
	h1.commitQuota(req1.Context(), "alice", "first.txt", token1)

	if rec := hfPutFile(t, h1, "alice", "new.txt", hfSelf(), strings.Repeat("d", 10)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("new 10-byte file after same-path replacements: got %d, want 413: %s", rec.Code, rec.Body.String())
	}

	rec := hfGetQuota(t, h1, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 1 {
		t.Fatalf("usedBytes=%d, want 1", q.UsedBytes)
	}
}

// The later commit may carry a different size than the reservation that was
// superseded. Recompute from live usage on commit so the last writer's bytes
// are not undercounted.
func TestHumanFiles_QuotaSamePathReplacementCommitOrder(t *testing.T) {
	h, store, _ := newHumanFilesRig(t)
	if rec := hfPutQuota(t, h, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h, "alice", "first.txt", hfSelf(), strings.Repeat("a", 10)); rec.Code != http.StatusOK {
		t.Fatalf("seed file: %d %s", rec.Code, rec.Body.String())
	}
	req := hfReq(t, http.MethodPut, "alice", "first.txt", hfSelf(), nil)
	a, status, msg := h.authorizeHumanFile(req.Context(), "alice", false)
	if a == nil {
		t.Fatalf("authorize: %d %s", status, msg)
	}
	token1, status, msg := h.reserveQuota(req.Context(), a, "alice", "first.txt", 10, 5)
	if status != 0 {
		t.Fatalf("first reserve: %d %s", status, msg)
	}
	token2, status, msg := h.reserveQuota(req.Context(), a, "alice", "first.txt", 10, 1)
	if status != 0 {
		t.Fatalf("second reserve: %d %s", status, msg)
	}

	if err := store.PutObject(req.Context(), humanFilesPrefix("alice")+"first.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	h.commitQuota(req.Context(), "alice", "first.txt", token2)
	if err := store.PutObject(req.Context(), humanFilesPrefix("alice")+"first.txt", []byte("ccccc")); err != nil {
		t.Fatal(err)
	}
	h.commitQuota(req.Context(), "alice", "first.txt", token1)

	if rec := hfPutFile(t, h, "alice", "new.txt", hfSelf(), strings.Repeat("d", 6)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("new 6-byte file: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
}

// TestHumanFiles_QuotaSetPreservesInFlightReservation is the quota-set vs
// in-flight-write interleaving: the first handler reserves 6 bytes but has not
// committed its object yet, then a second handler re-sets the same quota. The
// reconciler must not rebuild the ledger from live usage 0 and erase the
// outstanding reservation.
func TestHumanFiles_QuotaSetPreservesInFlightReservation(t *testing.T) {
	h1, store, k8s := newHumanFilesRig(t)
	h2 := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store))

	if rec := hfPutQuota(t, h1, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	req := hfReq(t, http.MethodPut, "alice", "a.txt", hfSelf(), nil)
	access, status, msg := h1.authorizeHumanFile(req.Context(), "alice", false)
	if access == nil {
		t.Fatalf("authorize: %d %s", status, msg)
	}
	token, status, msg := h1.reserveQuota(req.Context(), access, "alice", "a.txt", 0, 6)
	if status != 0 || token == "" {
		t.Fatalf("reserve: status=%d token=%q msg=%s", status, token, msg)
	}

	// Re-setting the same quota must preserve the in-flight reservation.
	if rec := hfPutQuota(t, h2, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("re-set quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h2, "alice", "b.txt", hfSelf(), "bbbbbb"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second write while first reservation is outstanding: got %d, want 413: %s", rec.Code, rec.Body.String())
	}

	// Simulate the first write aborting; release restores the ledger.
	h1.releaseQuota(req.Context(), "alice", "a.txt", token)
}

// TestHumanFiles_QuotaCrossHandlerAtomic is the cross-controller quota gate:
// two independently constructed handlers share the same OSS store and start
// with the same 10-byte quota. Without a shared atomic reservation both can
// pass the check and write 12 bytes; the ledger CAS must let exactly one
// write through.
func TestHumanFiles_QuotaCrossHandlerAtomic(t *testing.T) {
	h1, store, k8s := newHumanFilesRig(t)
	h2 := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store))

	if rec := hfPutQuota(t, h1, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}

	req1 := hfReq(t, http.MethodPut, "alice", "a.txt", hfSelf(), strings.NewReader("aaaaaa"))
	req2 := hfReq(t, http.MethodPut, "alice", "b.txt", hfSelf(), strings.NewReader("bbbbbb"))
	start := make(chan struct{})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		rec := httptest.NewRecorder()
		h1.putFile(rec, req1)
		codes <- rec.Code
	}()
	go func() {
		defer wg.Done()
		<-start
		rec := httptest.NewRecorder()
		h2.putFile(rec, req2)
		codes <- rec.Code
	}()
	close(start)
	wg.Wait()
	close(codes)

	ok, rejected := 0, 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusRequestEntityTooLarge:
			rejected++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok != 1 || rejected != 1 {
		t.Fatalf("cross-handler quota result: ok=%d rejected=%d, want 1/1", ok, rejected)
	}

	rec := hfGetQuota(t, h1, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes > 10 {
		t.Fatalf("usedBytes=%d exceeds quota 10", q.UsedBytes)
	}
}

// staleLedgerOSS returns an old ledger body on the first read while the
// actual object has already advanced to a new version. This is the
// GetObject/StatMeta interleaving: the old bytes must not be CASed using the
// newer ETag.
type staleLedgerOSS struct {
	*ossfake.Memory
	key   string
	stale []byte
	used  bool
}

func (s *staleLedgerOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	if !s.used && key == s.key {
		s.used = true
		return append([]byte(nil), s.stale...), nil
	}
	return s.Memory.GetObject(ctx, key)
}

func TestHumanFiles_QuotaLedgerRejectsStaleSnapshot(t *testing.T) {
	h1, store, k8s := newHumanFilesRig(t)
	if rec := hfPutQuota(t, h1, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h1, "alice", "a.txt", hfSelf(), "aaaaaa"); rec.Code != http.StatusOK {
		t.Fatalf("seed file: %d %s", rec.Code, rec.Body.String())
	}

	wrapped := &staleLedgerOSS{
		Memory: store,
		key:    humanQuotaLedgerKey("alice"),
		stale:  []byte(`{"usedBytes":0}`),
	}
	h2 := NewHumanFilesHandler(k8s, "default", wrapped, audit.NewClient(wrapped))

	rec := httptest.NewRecorder()
	h2.putFile(rec, hfReq(t, http.MethodPut, "alice", "b.txt", hfSelf(), strings.NewReader("bbbbbb")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("stale ledger snapshot: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
}

// TestHumanFiles_UsedBytesCountsNestedDirectoriesWithMCLikeListing covers the
// production listing contract: mc ls returns only direct children, so a
// nested file (or a trash id directory) must be walked rather than silently
// skipped. The in-memory fake returns the full subtree and masks this bug.
func TestHumanFiles_UsedBytesCountsNestedDirectoriesWithMCLikeListing(t *testing.T) {
	_, _, k8s := newHumanFilesRig(t)
	base := ossfake.NewMemory()
	store := &mcLikeOSS{Memory: base}
	hfh := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(base))

	if rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":100}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, hfh, "alice", "docs/notes/a.txt", hfSelf(), "123456"); rec.Code != http.StatusOK {
		t.Fatalf("nested put: %d %s", rec.Code, rec.Body.String())
	}
	rec := hfGetQuota(t, hfh, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 6 {
		t.Fatalf("nested file usedBytes=%d, want 6", q.UsedBytes)
	}

	rec = httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "docs/notes/a.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete nested file: %d %s", rec.Code, rec.Body.String())
	}
	rec = hfGetQuota(t, hfh, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota after delete: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes <= 6 {
		t.Fatalf("trash usage=%d, want content+metadata above the 6-byte payload", q.UsedBytes)
	}
}

// TestHumanFiles_QuotaLargerOlderOutstandingReplacementStaysAccounted is the
// maintainer's 10/1/1/9 interleaving: with a 10-byte quota and a 1-byte
// first.txt, handler A reserves the 1->10 replacement without writing;
// handler B then reserves 1->1, lands, and commits. The single-slot
// reservation map used to let B's commit delete A's in-flight 10-byte charge,
// so the 9-byte extra.txt write passed the quota check and the final usage
// overshot. The per-path outstanding queue must keep A's reservation charged
// until A's own commit removes it.
func TestHumanFiles_QuotaLargerOlderOutstandingReplacementStaysAccounted(t *testing.T) {
	h1, store, k8s := newHumanFilesRig(t)
	h2 := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store))
	ctx := context.Background()

	if rec := hfPutQuota(t, h1, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h1, "alice", "first.txt", hfSelf(), "a"); rec.Code != http.StatusOK {
		t.Fatalf("seed 1B: %d %s", rec.Code, rec.Body.String())
	}

	// Handler A reserves 1 -> 10 and stops before the object write.
	req1 := hfReq(t, http.MethodPut, "alice", "first.txt", hfSelf(), nil)
	a1, status, msg := h1.authorizeHumanFile(req1.Context(), "alice", false)
	if a1 == nil {
		t.Fatalf("authorize h1: %d %s", status, msg)
	}
	token1, status, msg := h1.reserveQuota(req1.Context(), a1, "alice", "first.txt", 1, 10)
	if status != 0 || token1 == "" {
		t.Fatalf("A reservation: status=%d token=%q msg=%s", status, token1, msg)
	}

	// Handler B reserves 1 -> 1, lands, and commits.
	req2 := hfReq(t, http.MethodPut, "alice", "first.txt", hfSelf(), nil)
	a2, status, msg := h2.authorizeHumanFile(req2.Context(), "alice", false)
	if a2 == nil {
		t.Fatalf("authorize h2: %d %s", status, msg)
	}
	token2, status, msg := h2.reserveQuota(req2.Context(), a2, "alice", "first.txt", 1, 1)
	if status != 0 || token2 == "" {
		t.Fatalf("B reservation: status=%d token=%q msg=%s", status, token2, msg)
	}
	if err := store.PutObject(ctx, humanFilesPrefix("alice")+"first.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	h2.commitQuota(ctx, "alice", "first.txt", token2)

	// A's in-flight 10-byte replacement must still be charged: 10 + 9 > 10.
	rec := hfPutFile(t, h2, "alice", "extra.txt", hfSelf(), strings.Repeat("e", 9))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("extra 9B while A's 10B replacement is outstanding: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	var coded humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &coded); err != nil {
		t.Fatal(err)
	}
	if coded.Code != "quota_exceeded" {
		t.Fatalf("code = %q, want quota_exceeded", coded.Code)
	}
	ledger, _, err := h1.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledger.UsedBytes != 10 {
		t.Fatalf("ledger usedBytes = %d, want 10 (A's in-flight delta must survive B's commit)", ledger.UsedBytes)
	}

	// Handler A now lands and commits its 10-byte replacement.
	if err := store.PutObject(ctx, humanFilesPrefix("alice")+"first.txt", []byte(strings.Repeat("a", 10))); err != nil {
		t.Fatal(err)
	}
	h1.commitQuota(ctx, "alice", "first.txt", token1)

	// Terminal state: exactly the quota's worth of live bytes, no extra.txt,
	// and the path entry deleted once its last reservation settled.
	// Reuse the outer recorder: `if rec := ...` would shadow it and the
	// following unmarshal would decode the earlier 413 body instead.
	rec = hfGetQuota(t, h1, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 10 {
		t.Fatalf("terminal live usage = %d, want 10", q.UsedBytes)
	}
	rec = httptest.NewRecorder()
	h1.getFile(rec, hfReq(t, http.MethodGet, "alice", "extra.txt", hfSelf(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("extra.txt GET: got %d, want 404", rec.Code)
	}
	ledger, _, err = h1.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read terminal ledger: %v", err)
	}
	if ledger.UsedBytes != 10 {
		t.Fatalf("terminal ledger usedBytes = %d, want 10", ledger.UsedBytes)
	}
	if _, ok := ledger.Paths["first.txt"]; ok {
		t.Fatalf("settled path entry survived: %+v", ledger.Paths["first.txt"])
	}

	// Guard against regressing the other direction (rejecting everything):
	// a shrinking overwrite at the quota is still a zero-delta write.
	if rec := hfPutFile(t, h1, "alice", "first.txt", hfSelf(), "abcde"); rec.Code != http.StatusOK {
		t.Fatalf("shrink overwrite at quota: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = hfGetQuota(t, h1, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 5 {
		t.Fatalf("usage after shrink = %d, want 5", q.UsedBytes)
	}
}

// TestHumanFiles_QuotaReverseInterleavedCommitStaysAccounted is the mirror of
// the maintainer repro with the assertion pinned at the moment it matters:
// B (the later, smaller reservation) lands and commits FIRST, and the
// over-quota write must be rejected immediately, while A's larger earlier
// reservation is still outstanding. A's later commit then settles everything
// at the quota.
func TestHumanFiles_QuotaReverseInterleavedCommitStaysAccounted(t *testing.T) {
	h1, store, k8s := newHumanFilesRig(t)
	h2 := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store))
	ctx := context.Background()

	if rec := hfPutQuota(t, h1, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h1, "alice", "first.txt", hfSelf(), "a"); rec.Code != http.StatusOK {
		t.Fatalf("seed 1B: %d %s", rec.Code, rec.Body.String())
	}

	req1 := hfReq(t, http.MethodPut, "alice", "first.txt", hfSelf(), nil)
	a1, status, msg := h1.authorizeHumanFile(req1.Context(), "alice", false)
	if a1 == nil {
		t.Fatalf("authorize h1: %d %s", status, msg)
	}
	token1, status, msg := h1.reserveQuota(req1.Context(), a1, "alice", "first.txt", 1, 10)
	if status != 0 || token1 == "" {
		t.Fatalf("A reservation: status=%d token=%q msg=%s", status, token1, msg)
	}

	req2 := hfReq(t, http.MethodPut, "alice", "first.txt", hfSelf(), nil)
	a2, status, msg := h2.authorizeHumanFile(req2.Context(), "alice", false)
	if a2 == nil {
		t.Fatalf("authorize h2: %d %s", status, msg)
	}
	token2, status, msg := h2.reserveQuota(req2.Context(), a2, "alice", "first.txt", 1, 1)
	if status != 0 || token2 == "" {
		t.Fatalf("B reservation: status=%d token=%q msg=%s", status, token2, msg)
	}
	if err := store.PutObject(ctx, humanFilesPrefix("alice")+"first.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	h2.commitQuota(ctx, "alice", "first.txt", token2)

	// Immediately after B's commit, A's 10-byte head of the chain is still
	// outstanding: any further byte is over quota.
	if rec := hfPutFile(t, h2, "alice", "extra2.txt", hfSelf(), strings.Repeat("e", 9)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("extra2 9B right after B's commit: got %d, want 413: %s", rec.Code, rec.Body.String())
	}

	if err := store.PutObject(ctx, humanFilesPrefix("alice")+"first.txt", []byte(strings.Repeat("a", 10))); err != nil {
		t.Fatal(err)
	}
	h1.commitQuota(ctx, "alice", "first.txt", token1)

	rec := hfGetQuota(t, h1, "alice", hfSelf())
	if rec.Code != http.StatusOK {
		t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
	}
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 10 {
		t.Fatalf("terminal live usage = %d, want 10", q.UsedBytes)
	}
	ledger, _, err := h1.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledger.UsedBytes != 10 {
		t.Fatalf("terminal ledger usedBytes = %d, want 10", ledger.UsedBytes)
	}
}

// TestHumanFiles_QuotaLostTokenFallback pins the crash-recovery contract: a
// reservation whose commit/release never arrives keeps the ledger consistent
// (I1/I5) — sync recomputes the invariant, a release for an unknown token is
// a no-op, and the ledger stays valid JSON with no negative usage.
func TestHumanFiles_QuotaLostTokenFallback(t *testing.T) {
	h, _, _ := newHumanFilesRig(t)
	ctx := context.Background()

	if rec := hfPutQuota(t, h, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	req := hfReq(t, http.MethodPut, "alice", "a.txt", hfSelf(), nil)
	a, status, msg := h.authorizeHumanFile(req.Context(), "alice", false)
	if a == nil {
		t.Fatalf("authorize: %d %s", status, msg)
	}
	token, status, msg := h.reserveQuota(req.Context(), a, "alice", "a.txt", 0, 6)
	if status != 0 || token == "" {
		t.Fatalf("reserve: status=%d token=%q msg=%s", status, token, msg)
	}

	// The object write never happens and the token is lost. Syncing must
	// leave I1 intact: used = liveTotal + (Counted - live(a.txt)).
	if err := h.syncQuotaUsage(ctx, "alice"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	ledger, _, err := h.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	liveTotal, err := h.usedBytes(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := ledger.Paths["a.txt"]
	if !ok {
		t.Fatalf("outstanding entry missing after sync: %+v", ledger.Paths)
	}
	liveA, err := h.liveFileSize(ctx, "alice", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := liveTotal + entry.Counted - liveA
	if ledger.UsedBytes != want {
		t.Fatalf("I1 broken after sync: usedBytes = %d, want liveTotal+(Counted-live) = %d", ledger.UsedBytes, want)
	}
	if ledger.UsedBytes != 6 {
		t.Fatalf("usedBytes = %d, want 6 (6-byte reservation, nothing landed)", ledger.UsedBytes)
	}

	// A release for a path that was never reserved: no panic, no negative
	// usage, and the ledger is still valid JSON.
	h.releaseQuota(ctx, "alice", "ghost.txt", "missing-token")
	ledger, _, err = h.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("ledger no longer valid JSON: %v", err)
	}
	if ledger.UsedBytes < 0 {
		t.Fatalf("usedBytes went negative: %d", ledger.UsedBytes)
	}
	if ledger.UsedBytes != 6 {
		t.Fatalf("unknown-token release changed usage: %d, want 6", ledger.UsedBytes)
	}
}

// TestHumanFiles_QuotaLedgerLegacyFormatMigration covers the one-time,
// lenient, fail-closed upgrade of the legacy path->single-reservation ledger:
// the in-flight reservation survives the migration charged at its reserved
// target size, Reservations is cleared, and I1 holds for the recomputed
// usage — so the migrated reservation still gates writes.
func TestHumanFiles_QuotaLedgerLegacyFormatMigration(t *testing.T) {
	h, store, _ := newHumanFilesRig(t)
	ctx := context.Background()

	// A legacy ledger body: an in-flight 10-byte reservation for first.txt
	// whose object write has not landed.
	legacy := humanQuotaLedger{
		UsedBytes: 10,
		Reservations: map[string]quotaReservation{
			"first.txt": {Token: "legacy-token", BaseOldSize: 0, NewSize: 10},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(ctx, humanQuotaLedgerKey("alice"), data); err != nil {
		t.Fatal(err)
	}

	// Any ledger read upgrades the format in place.
	ledger, _, err := h.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read (migration) ledger: %v", err)
	}
	if len(ledger.Reservations) != 0 {
		t.Fatalf("legacy Reservations not cleared: %+v", ledger.Reservations)
	}
	entry, ok := ledger.Paths["first.txt"]
	if !ok {
		t.Fatalf("migrated path entry missing: %+v", ledger.Paths)
	}
	if len(entry.Outstanding) != 1 || entry.Outstanding[0].Token != "legacy-token" || entry.Outstanding[0].NewSize != 10 {
		t.Fatalf("migrated outstanding = %+v, want the legacy reservation", entry.Outstanding)
	}
	// I1: used = liveTotal (0) + (Counted (10) - live (0)).
	if entry.Counted != 10 {
		t.Fatalf("migrated Counted = %d, want 10", entry.Counted)
	}
	if ledger.UsedBytes != 10 {
		t.Fatalf("migrated usedBytes = %d, want 10", ledger.UsedBytes)
	}

	// The migrated in-flight reservation still gates writes: with 10 of the
	// 10-byte quota charged to the outstanding write, one more byte is over.
	if rec := hfPutQuota(t, h, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h, "alice", "extra.txt", hfSelf(), "x"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("write over migrated in-flight reservation: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
}

// faultyStatOSS returns a non-NotExist error from StatMeta for `remaining`
// calls on one key, then delegates — a transient stat fault injected on an
// in-flight path (the stat-side mirror of the staleLedgerOSS read fault).
type faultyStatOSS struct {
	*ossfake.Memory
	key       string
	remaining int
}

func (f *faultyStatOSS) StatMeta(ctx context.Context, key string) (oss.ObjectMeta, error) {
	if f.remaining > 0 && key == f.key {
		f.remaining--
		return oss.ObjectMeta{}, errors.New("transient stat fault")
	}
	return f.Memory.StatMeta(ctx, key)
}

// TestHumanFiles_QuotaStatFaultDoesNotDropInFlightCharge is the maintainer's
// fourth-round repro: with a 10-byte quota and an in-flight 6-byte
// reservation that has not landed, re-setting the quota triggers
// reconcile -> sync while the stat of the reserved path faults once. The
// old code skipped the path yet still wrote the total, dropping the charge
// to 0 — a following 6-byte write then passed and the final usage overshot
// to 12. Sync must fail closed: the operation errors, the ledger is left
// byte-for-byte untouched, and the preserved charge still gates writes.
func TestHumanFiles_QuotaStatFaultDoesNotDropInFlightCharge(t *testing.T) {
	h1, store, k8s := newHumanFilesRig(t)
	ctx := context.Background()

	if rec := hfPutQuota(t, h1, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	req := hfReq(t, http.MethodPut, "alice", "first.txt", hfSelf(), nil)
	access, status, msg := h1.authorizeHumanFile(req.Context(), "alice", false)
	if access == nil {
		t.Fatalf("authorize: %d %s", status, msg)
	}
	token, status, msg := h1.reserveQuota(req.Context(), access, "alice", "first.txt", 0, 6)
	if status != 0 || token == "" {
		t.Fatalf("reserve: status=%d token=%q msg=%s", status, token, msg)
	}
	before, beforeETag, err := h1.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read ledger before fault: %v", err)
	}

	faulty := &faultyStatOSS{
		Memory:    store,
		key:       humanFilesPrefix("alice") + "first.txt",
		remaining: 1,
	}
	h2 := NewHumanFilesHandler(k8s, "default", faulty, audit.NewClient(faulty))

	// Re-setting the quota reconciles the ledger; the stat fault must fail
	// the whole operation (500) instead of writing a total that dropped the
	// in-flight charge.
	rec := hfPutQuota(t, h2, "alice", hfAdmin(), `{"quotaBytes":10}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("re-set quota under stat fault: got %d, want 500: %s", rec.Code, rec.Body.String())
	}
	faulty.remaining = 0
	after, afterETag, err := h1.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read ledger after fault: %v", err)
	}
	if afterETag != beforeETag || after.UsedBytes != before.UsedBytes {
		t.Fatalf("faulted sync modified the ledger: etag %q->%q, usedBytes %d->%d",
			beforeETag, afterETag, before.UsedBytes, after.UsedBytes)
	}
	if after.UsedBytes != 6 {
		t.Fatalf("ledger usedBytes = %d, want 6 (in-flight charge must survive the stat fault)", after.UsedBytes)
	}
	entry, ok := after.Paths["first.txt"]
	if !ok || entry.Counted != 6 || len(entry.Outstanding) != 1 {
		t.Fatalf("ledger paths = %+v, want first.txt Counted 6 with the outstanding reservation", after.Paths)
	}

	// The preserved charge still gates writes: 6 in-flight + 6 new > 10.
	if rec := hfPutFile(t, h1, "alice", "second.txt", hfSelf(), "123456"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("6B write against preserved in-flight charge: got %d, want 413: %s", rec.Code, rec.Body.String())
	}

	// The original write lands and commits; usage settles at exactly 6.
	if err := store.PutObject(ctx, humanFilesPrefix("alice")+"first.txt", []byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	h1.commitQuota(ctx, "alice", "first.txt", token)
	ledger, _, err := h1.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read terminal ledger: %v", err)
	}
	if ledger.UsedBytes != 6 {
		t.Fatalf("terminal usedBytes = %d, want 6", ledger.UsedBytes)
	}
	if _, ok := ledger.Paths["first.txt"]; ok {
		t.Fatalf("settled entry survived commit: %+v", ledger.Paths["first.txt"])
	}
}

// --- Round 5 P1 regressions (maintainer-named) -------------------------------

// concurrentPutOSS serves a stale body on the first read of `key` while
// advancing the underlying object to `injected` in the same call — the
// concurrent writer landing between the handler's GetObject and StatMeta.
type concurrentPutOSS struct {
	*ossfake.Memory
	key      string
	stale    []byte
	injected []byte
	used     bool
}

func (c *concurrentPutOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	if !c.used && key == c.key {
		c.used = true
		if err := c.Memory.PutObject(ctx, c.key, c.injected); err != nil {
			return nil, err
		}
		return append([]byte(nil), c.stale...), nil
	}
	return c.Memory.GetObject(ctx, key)
}

// multipartETagOSS reports one key's ETag in multipart form (md5-N), which
// cannot be verified against the body locally and forces the double-read
// consistency fallback in readFileSnapshot.
type multipartETagOSS struct {
	*ossfake.Memory
	key string
}

func (m *multipartETagOSS) StatMeta(ctx context.Context, key string) (oss.ObjectMeta, error) {
	meta, err := m.Memory.StatMeta(ctx, key)
	if err != nil {
		return meta, err
	}
	if key == m.key {
		if data, derr := m.Memory.GetObject(ctx, key); derr == nil {
			meta.ETag = md5Hex(data) + "-3"
		}
	}
	return meta, nil
}

// hfTrashIDs returns the valid trash entry ids currently present for a
// human (ids carry a time prefix, so listing order is chronological).
func hfTrashIDs(t *testing.T, store *ossfake.Memory, username string) map[string]struct{} {
	t.Helper()
	infos, err := store.ListObjectsDetailed(context.Background(), humanTrashPrefix(username))
	if err != nil {
		t.Fatalf("list trash: %v", err)
	}
	ids := map[string]struct{}{}
	for _, info := range infos {
		id := strings.SplitN(info.Name, "/", 2)[0]
		if trashIDRegex.MatchString(id) {
			ids[id] = struct{}{}
		}
	}
	return ids
}

// TestReviewerFileGETETagMustDescribeReturnedBytes is the maintainer's
// round-5 P1-1 repro: a GET that straddled a concurrent PUT used to serve
// "A's bytes + B's ETag", whose If-Match write then silently clobbered the
// never-seen B. The served ETag must describe exactly the served bytes,
// and that same token must authorize an If-Match write.
func TestReviewerFileGETETagMustDescribeReturnedBytes(t *testing.T) {
	base := ossfake.NewMemory()
	ctx := context.Background()
	key := humanFilesPrefix("alice") + "f.txt"
	if err := base.PutObject(ctx, key, []byte("AAAAA")); err != nil {
		t.Fatal(err)
	}
	wrapped := &concurrentPutOSS{
		Memory:   base,
		key:      key,
		stale:    []byte("AAAAA"),
		injected: []byte("BBBBB"),
	}
	_, _, k8s := newHumanFilesRig(t)
	hfh := NewHumanFilesHandler(k8s, "default", wrapped, audit.NewClient(wrapped))

	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "f.txt", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	etag := rec.Result().Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET did not set an ETag header")
	}
	if got := md5Hex(body); !strings.EqualFold(strings.Trim(etag, `"`), got) {
		t.Fatalf("served ETag %q does not describe the served bytes (md5=%q): an If-Match write with this token would clobber a version the client never saw", etag, got)
	}

	// The served token must actually authorize an If-Match write.
	put := hfReq(t, http.MethodPut, "alice", "f.txt", hfSelf(), strings.NewReader("CCCCC"))
	put.Header.Set("If-Match", etag)
	rec2 := httptest.NewRecorder()
	hfh.putFile(rec2, put)
	if rec2.Code != http.StatusOK {
		t.Fatalf("If-Match PUT with the served ETag: got %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
}

// TestReviewerSoftDeleteMustPreserveConcurrentNewVersion is the maintainer's
// round-5 P1-2 repro: a soft delete whose archive straddled a concurrent
// PUT used to trash A and delete B — the newer version vanished from both
// live and trash. Whatever the race resolves to, the newer version must
// survive: either the delete 409s and B stays live, or the 200 archive
// holds B (never A), the trash gains no orphan entry, and B is never gone
// from both places.
func TestReviewerSoftDeleteMustPreserveConcurrentNewVersion(t *testing.T) {
	base := ossfake.NewMemory()
	ctx := context.Background()
	key := humanFilesPrefix("alice") + "f.txt"
	if err := base.PutObject(ctx, key, []byte("AAAAA")); err != nil {
		t.Fatal(err)
	}
	wrapped := &concurrentPutOSS{
		Memory:   base,
		key:      key,
		stale:    []byte("AAAAA"),
		injected: []byte("BBBBB"),
	}
	_, _, k8s := newHumanFilesRig(t)
	hfh := NewHumanFilesHandler(k8s, "default", wrapped, audit.NewClient(wrapped))

	before := hfTrashIDs(t, base, "alice")
	rec := httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "f.txt", hfSelf(), nil))

	switch rec.Code {
	case http.StatusConflict:
		var ce humanFileCodedError
		if err := json.Unmarshal(rec.Body.Bytes(), &ce); err != nil {
			t.Fatalf("conflict body: %v: %s", err, rec.Body.String())
		}
		if ce.Code != "version_conflict" {
			t.Fatalf("conflict code = %q, want version_conflict", ce.Code)
		}
		data, err := base.GetObject(ctx, key)
		if err != nil || string(data) != "BBBBB" {
			t.Fatalf("409 but the live file is not the new version: %q err=%v, want BBBBB", data, err)
		}
		if after := hfTrashIDs(t, base, "alice"); len(after) != len(before) {
			t.Fatalf("409 left a trash entry behind: before=%v after=%v", before, after)
		}
	case http.StatusOK:
		after := hfTrashIDs(t, base, "alice")
		if len(after) != len(before)+1 {
			t.Fatalf("trash ids after delete = %v, want exactly one new entry (before %v)", after, before)
		}
		var trashID string
		for id := range after {
			if _, existed := before[id]; !existed {
				trashID = id
			}
		}
		content, err := base.GetObject(ctx, humanTrashPrefix("alice")+trashID+"/content")
		if err != nil || string(content) != "BBBBB" {
			t.Fatalf("archived content = %q err=%v, want the new version BBBBB (never AAAAA)", content, err)
		}
		if _, err := base.GetObject(ctx, humanTrashPrefix("alice")+trashID+"/meta.json"); err != nil {
			t.Fatalf("trash entry %s has no meta.json", trashID)
		}
		if _, err := base.GetObject(ctx, key); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("delete reported success but the live file is still readable (err=%v)", err)
		}
	default:
		t.Fatalf("delete: got %d, want 409 (version_conflict) or 200: %s", rec.Code, rec.Body.String())
	}

	// Red line (both branches): the new version must be in live or in the
	// trash — it may never be gone from both.
	kept := false
	if live, err := base.GetObject(ctx, key); err == nil && string(live) == "BBBBB" {
		kept = true
	} else {
		for id := range hfTrashIDs(t, base, "alice") {
			if c, err := base.GetObject(ctx, humanTrashPrefix("alice")+id+"/content"); err == nil && string(c) == "BBBBB" {
				kept = true
				break
			}
		}
	}
	if !kept {
		t.Fatal("red line: the new version BBBBB is gone from both live and trash")
	}
	// No orphan trash entries: every id carries both content and meta.json.
	for id := range hfTrashIDs(t, base, "alice") {
		if _, err := base.GetObject(ctx, humanTrashPrefix("alice")+id+"/content"); err != nil {
			t.Fatalf("orphan trash entry %s: missing content", id)
		}
		if _, err := base.GetObject(ctx, humanTrashPrefix("alice")+id+"/meta.json"); err != nil {
			t.Fatalf("orphan trash entry %s: missing meta.json", id)
		}
	}
}

// TestHumanFiles_PathLockSerializesMutations verifies the per-path lease:
// while one holder keeps the lease for a rel, a second handler's PUT on
// that rel is rejected with 409 file_busy and writes nothing; after the
// lease is released the same PUT succeeds.
func TestHumanFiles_PathLockSerializesMutations(t *testing.T) {
	h1, store, k8s := newHumanFilesRig(t)
	ctx := context.Background()
	key := humanFilesPrefix("alice") + "f.txt"
	lockKey := humanPathLockKey("alice", "f.txt")

	lock, err := h1.acquireHumanPathLock(ctx, "alice", "f.txt", humanPathLockTTL)
	if err != nil {
		t.Fatalf("manual acquire: %v", err)
	}
	if _, err := store.GetObject(ctx, lockKey); err != nil {
		t.Fatalf("lock object not stored: %v", err)
	}

	h2 := NewHumanFilesHandler(k8s, "default", store, audit.NewClient(store))
	rec := hfPutFile(t, h2, "alice", "f.txt", hfSelf(), "BBBBB")
	if rec.Code != http.StatusConflict {
		t.Fatalf("put under held lease: got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var ce humanFileCodedError
	if err := json.Unmarshal(rec.Body.Bytes(), &ce); err != nil {
		t.Fatal(err)
	}
	if ce.Code != "file_busy" {
		t.Fatalf("error code = %q, want file_busy", ce.Code)
	}
	if _, err := store.GetObject(ctx, key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the blocked put still wrote the file (err=%v)", err)
	}

	lock.Release(ctx)
	if _, err := store.GetObject(ctx, lockKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release left the lock object behind (err=%v)", err)
	}
	if rec := hfPutFile(t, h2, "alice", "f.txt", hfSelf(), "BBBBB"); rec.Code != http.StatusOK {
		t.Fatalf("put after release: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if data, err := store.GetObject(ctx, key); err != nil || string(data) != "BBBBB" {
		t.Fatalf("file after release = %q err=%v, want BBBBB", data, err)
	}
}

// TestHumanFiles_PathLockExpiredLeaseCanBeStolen verifies the steal path:
// a lease whose expiresAt is in the past (a crashed holder) is taken over
// by a CAS write — quickly, with a new owner and a fresh expiry — and the
// stolen lease releases cleanly.
func TestHumanFiles_PathLockExpiredLeaseCanBeStolen(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	ctx := context.Background()
	lockKey := humanPathLockKey("alice", "f.txt")

	past := time.Now().UTC().Add(-10 * time.Minute)
	old := humanPathLockPayload{
		Owner:      "00000000-0000-4000-8000-000000000000",
		Path:       "f.txt",
		AcquiredAt: past.Add(-time.Minute).Format(time.RFC3339),
		ExpiresAt:  past.Format(time.RFC3339),
	}
	body, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(ctx, lockKey, body); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	lock, err := hfh.acquireHumanPathLock(ctx, "alice", "f.txt", humanPathLockTTL)
	if err != nil {
		t.Fatalf("acquire over an expired lease: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("stealing an expired lease took %v, want < 3s", d)
	}
	cur, err := store.GetObject(ctx, lockKey)
	if err != nil {
		t.Fatalf("lock object after steal: %v", err)
	}
	var p humanPathLockPayload
	if err := json.Unmarshal(cur, &p); err != nil {
		t.Fatal(err)
	}
	if p.Owner == "" || p.Owner == old.Owner {
		t.Fatalf("steal did not change the owner: %q", p.Owner)
	}
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		t.Fatalf("steal lease expiresAt parse: %v", err)
	}
	now := time.Now().UTC()
	if exp.Before(now.Add(4*time.Minute)) || exp.After(now.Add(6*time.Minute)) {
		t.Fatalf("stolen lease expires %v, want ≈ now+5min", exp)
	}

	lock.Release(ctx)
	if _, err := store.GetObject(ctx, lockKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release after steal left the lock object (err=%v)", err)
	}
}

// TestHumanFiles_GetSnapshotHandlesMultipartETag verifies the fallback for
// ETags that cannot be verified locally (multipart md5-N): the double-read
// consistency check accepts a stable object and the GET still reports the
// multipart ETag verbatim.
func TestHumanFiles_GetSnapshotHandlesMultipartETag(t *testing.T) {
	base := ossfake.NewMemory()
	ctx := context.Background()
	key := humanFilesPrefix("alice") + "big.bin"
	content := []byte("multipart payload")
	if err := base.PutObject(ctx, key, content); err != nil {
		t.Fatal(err)
	}
	wrapped := &multipartETagOSS{Memory: base, key: key}
	_, _, k8s := newHumanFilesRig(t)
	hfh := NewHumanFilesHandler(k8s, "default", wrapped, audit.NewClient(wrapped))

	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "big.bin", hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("body = %q, want %q", rec.Body.Bytes(), content)
	}
	if got, want := rec.Result().Header.Get("ETag"), md5Hex(content)+"-3"; got != want {
		t.Fatalf("ETag = %q, want the multipart form %q", got, want)
	}
}
