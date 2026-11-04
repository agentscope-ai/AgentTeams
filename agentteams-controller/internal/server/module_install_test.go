package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testResp(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func testInstaller(artifact []byte, sha string) (*moduleInstaller, *http.Client) {
	index := fmt.Sprintf(`{"schema":1,"channel":"beta","modules":{"teamharness":{"latest":"0.1.0","versions":{"0.1.0":{"artifacts":[{"name":"teamharness-0.1.0-qwenpaw.zip","sha256":"%s"}]}}}}}`, sha)
	c := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/releases/channels.json"):
			return testResp(200, index), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases/teamharness/0.1.0/"):
			return testResp(200, string(artifact)), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/plugins":
			return testResp(200, `[{"id":"teamharness","version":"0.0.9","loaded":true}]`), nil
		case r.Method == http.MethodPost && r.URL.Path == "/api/plugins/upload":
			return testResp(200, `{"installed":true,"loaded":true}`), nil
		default:
			return testResp(404, "not found"), nil
		}
	})}
	m := &moduleInstaller{
		index:          &MillforgeIndexHandler{client: c},
		http:           c,
		workerBaseURL:  func(string, map[string]string) string { return "http://worker.test" },
		managerBaseURL: func(string) string { return "http://manager.test" },
	}
	return m, c
}

func TestInstallOneSuccess(t *testing.T) {
	artifact := []byte("zip-bytes")
	sum := sha256.Sum256(artifact)
	m, _ := testInstaller(artifact, hex.EncodeToString(sum[:]))
	loaded, prev, status, msg := m.installOne(context.Background(), "http://worker.test", "teamharness", moduleInstallRequest{
		Source: "github", Repo: "o/r", Ref: "main", Version: "0.1.0",
		Artifact: "teamharness-0.1.0-qwenpaw.zip",
	})
	if status != 200 || !loaded || prev != "0.0.9" {
		t.Fatalf("status=%d loaded=%v prev=%q msg=%s", status, loaded, prev, msg)
	}
}

func TestInstallOneSHAConflict(t *testing.T) {
	artifact := []byte("zip-bytes")
	sum := sha256.Sum256(artifact)
	m, _ := testInstaller(artifact, hex.EncodeToString(sum[:]))
	_, _, status, _ := m.installOne(context.Background(), "http://worker.test", "teamharness", moduleInstallRequest{
		Source: "github", Repo: "o/r", Ref: "main", Version: "0.1.0",
		SHA256: strings.Repeat("0", 64),
	})
	if status != 409 {
		t.Fatalf("status=%d, want 409", status)
	}
}

func TestInstallOneArtifactMismatch(t *testing.T) {
	artifact := []byte("zip-bytes")
	wrong := sha256.Sum256([]byte("other"))
	m, _ := testInstaller(artifact, hex.EncodeToString(wrong[:]))
	_, _, status, _ := m.installOne(context.Background(), "http://worker.test", "teamharness", moduleInstallRequest{
		Source: "github", Repo: "o/r", Ref: "main", Version: "0.1.0",
	})
	if status != 502 {
		t.Fatalf("status=%d, want 502", status)
	}
}

func TestIndexArtifact(t *testing.T) {
	modules := []byte(`{"teamharness":{"latest":"0.1.0","versions":{"0.1.0":{"artifacts":[{"name":"a.zip","sha256":"abc"}]}}}}`)
	name, sha, status, _ := indexArtifact(modules, "teamharness", "", "")
	if status != 200 || name != "a.zip" || sha != "abc" {
		t.Fatalf("name=%q sha=%q status=%d", name, sha, status)
	}
	if _, _, status, _ = indexArtifact(modules, "missing", "", ""); status != 404 {
		t.Fatalf("missing module status=%d", status)
	}
}

func TestArtifactURL(t *testing.T) {
	if got := artifactURL("github", "o/r", "main", "m", "1.0.0", "a.zip"); got != "https://raw.githubusercontent.com/o/r/main/releases/m/1.0.0/a.zip" {
		t.Fatalf("github url=%s", got)
	}
	if got := artifactURL("gitcode", "o/r", "main", "m", "1.0.0", "a.zip"); got != "https://raw.gitcode.com/o/r/raw/main/releases/m/1.0.0/a.zip" {
		t.Fatalf("gitcode url=%s", got)
	}
}

func httptestNewRequest(method, target string, body io.Reader, name string) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.SetPathValue("name", name)
	req.SetPathValue("module", "teamharness")
	return req
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func TestInstallRoutesKubeMode(t *testing.T) {
	rh := &ResourceHandler{}
	for name, h := range map[string]http.HandlerFunc{
		"worker":  rh.InstallWorkerPlugin("kubernetes", ""),
		"manager": rh.InstallManagerPlugin("kubernetes", ""),
		"team":    rh.InstallTeamModule("kubernetes", ""),
	} {
		req := httptestNewRequest(http.MethodPost, "/x", nil, name)
		rec := newRecorder()
		h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status=%d, want 503", name, rec.Code)
		}
	}
}
