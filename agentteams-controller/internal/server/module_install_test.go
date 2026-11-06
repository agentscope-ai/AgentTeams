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

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/releases/beta/channels.json"):
			return testResp(200, index), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases/teamharness/0.1.0/"):
			return testResp(200, string(artifact)), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/plugins":
			return testResp(200, `[{"id":"teamharness","version":"0.0.9","loaded":true}]`), nil
		case r.Method == http.MethodPost && r.URL.Path == "/api/plugins/upload":
			return testResp(200, `{"id":"teamharness","version":"0.1.0","loaded":true}`), nil
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
	artifact := moduleIdentityZip(t, "teamharness", "0.1.0")
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
	artifact := moduleIdentityZip(t, "teamharness", "0.1.0")
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
	artifact := moduleIdentityZip(t, "teamharness", "0.1.0")
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
	name, sha, _, _, status, _ := indexArtifact(modules, "teamharness", "", "")
	if status != 200 || name != "a.zip" || sha != "abc" {
		t.Fatalf("name=%q sha=%q status=%d", name, sha, status)
	}
	if _, _, _, _, status, _ = indexArtifact(modules, "missing", "", ""); status != 404 {
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
		"worker":  rh.InstallWorkerPlugin("kubernetes", "", ""),
		"manager": rh.InstallManagerPlugin("kubernetes", ""),
		"team":    rh.InstallTeamModule("kubernetes", "", ""),
	} {
		req := httptestNewRequest(http.MethodPost, "/x", nil, name)
		rec := newRecorder()
		h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status=%d, want 503", name, rec.Code)
		}
	}
}

func TestInstallOneChannelMismatch(t *testing.T) {
	artifact := moduleIdentityZip(t, "teamharness", "0.1.0")
	sum := sha256.Sum256(artifact)
	m, _ := testInstaller(artifact, hex.EncodeToString(sum[:]))
	_, _, status, _ := m.installOne(context.Background(), "http://worker.test", "teamharness", moduleInstallRequest{
		Source: "github", Repo: "o/r", Ref: "main", Channel: "stable", Version: "0.1.0",
	})
	if status != 404 {
		t.Fatalf("status=%d, want 404", status)
	}
}

func TestInstallTeamStopsOnRuntimeMismatch(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	team := &v1beta1.Team{}
	team.Name = "alpha"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "w1", Role: "worker"}, {Name: "w2", Role: "worker"}}
	w1 := &v1beta1.Worker{}
	w1.Name = "w1"
	w1.Namespace = "default"
	w2 := &v1beta1.Worker{}
	w2.Name = "w2"
	w2.Namespace = "default"
	w2.Spec.Runtime = "openclaw"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(w1, w2).Build()

	artifact := moduleIdentityZip(t, "teamharness", "0.1.0")
	sum := sha256.Sum256(artifact)
	m, _ := testInstaller(artifact, hex.EncodeToString(sum[:]))
	m.rh = &ResourceHandler{client: cl, namespace: "default"}
	m.workerBaseURL = func(string, map[string]string) string { return "http://worker.test" }

	updated, failed, _, _, status, _ := m.installTeam(context.Background(), team, "teamharness", moduleInstallRequest{
		Source: "github", Repo: "o/r", Ref: "main", Version: "0.1.0",
	})
	if status != 400 || len(updated) != 1 || updated[0] != "w1" || failed["worker"] != "w2" {
		t.Fatalf("status=%d updated=%v failed=%v", status, updated, failed)
	}
}

func TestInstallOneRejectsDotDotModule(t *testing.T) {
	artifact := moduleIdentityZip(t, "teamharness", "0.1.0")
	sum := sha256.Sum256(artifact)
	m, _ := testInstaller(artifact, hex.EncodeToString(sum[:]))
	_, _, status, _ := m.installOne(context.Background(), "http://worker.test", "..", moduleInstallRequest{
		Source: "github", Repo: "o/r", Ref: "main", Version: "0.1.0",
	})
	if status != 400 {
		t.Fatalf("status=%d, want 400", status)
	}
}

func TestManagerBaseURLUsesResourcePrefix(t *testing.T) {
	m := newModuleInstaller(&ResourceHandler{}, "embedded", "agentteams-worker-", "agentteams-")
	if got := m.managerBaseURL("default"); got != "http://agentteams-manager:18799" {
		t.Fatalf("default manager URL = %s", got)
	}
	if got := m.managerBaseURL("staging"); got != "http://agentteams-manager-staging:18799" {
		t.Fatalf("named manager URL = %s", got)
	}
}

func TestIndexArtifactPluginIDAndKind(t *testing.T) {
	modules := []byte(`{"agentteams-matrix":{"latest":"0.1.0","pluginId":"agentteams-matrix-channel","kind":"bundle","versions":{"0.1.0":{"artifacts":[{"name":"agentteams-matrix-channel-0.1.0-qwenpaw.zip","sha256":"abc"}]}}}}`)
	name, _, pluginID, kind, status, _ := indexArtifact(modules, "agentteams-matrix", "", "")
	if status != 200 || name == "" || pluginID != "agentteams-matrix-channel" || kind != "bundle" {
		t.Fatalf("name=%q pluginID=%q kind=%q status=%d", name, pluginID, kind, status)
	}
	bundleSameID := []byte(`{"m":{"latest":"1.0.0","kind":"bundle","versions":{"1.0.0":{"artifacts":[{"name":"m.zip","sha256":"abc"}]}}}}`)
	if _, _, _, _, status, _ = indexArtifact(bundleSameID, "m", "", ""); status != 400 {
		t.Fatalf("bundle without adapter pluginId: status=%d, want 400", status)
	}
	runner := []byte(`{"r":{"latest":"1.0.0","kind":"runner","versions":{"1.0.0":{"artifacts":[{"name":"r.zip","sha256":"abc"}]}}}}`)
	if _, _, _, _, status, _ = indexArtifact(runner, "r", "", ""); status != 400 {
		t.Fatalf("runner install: status=%d, want 400", status)
	}
	nonZip := []byte(`{"p":{"latest":"1.0.0","versions":{"1.0.0":{"artifacts":[{"name":"p.tar.gz","sha256":"abc"}]}}}}`)
	if _, _, _, _, status, _ = indexArtifact(nonZip, "p", "", ""); status != 400 {
		t.Fatalf("non-zip artifact: status=%d, want 400", status)
	}
}

func TestIndexArtifactPrefersZip(t *testing.T) {
	modules := []byte(`{"p":{"latest":"1.0.0","versions":{"1.0.0":{"artifacts":[{"name":"p.tar.gz","sha256":"aaa"},{"name":"p-qwenpaw-1.0.0.zip","sha256":"bbb"}]}}}}`)
	name, sha, _, _, status, _ := indexArtifact(modules, "p", "", "")
	if status != 200 || name != "p-qwenpaw-1.0.0.zip" || sha != "bbb" {
		t.Fatalf("name=%q sha=%q status=%d", name, sha, status)
	}
}

func TestInstallOneUsesIndexPluginID(t *testing.T) {
	artifact := moduleIdentityZip(t, "agentteams-matrix-channel", "0.1.0")
	sum := sha256.Sum256(artifact)
	index := fmt.Sprintf(`{"schema":1,"channel":"beta","modules":{"agentteams-matrix":{"latest":"0.1.0","pluginId":"agentteams-matrix-channel","kind":"bundle","versions":{"0.1.0":{"artifacts":[{"name":"agentteams-matrix-channel-0.1.0.zip","sha256":"%s"}]}}}}}`, hex.EncodeToString(sum[:]))
	c := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/releases/beta/channels.json"):
			return testResp(200, index), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases/agentteams-matrix/0.1.0/"):
			return testResp(200, string(artifact)), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/plugins":
			return testResp(200, `[{"id":"agentteams-matrix-channel","version":"0.0.9","loaded":true}]`), nil
		case r.Method == http.MethodPost && r.URL.Path == "/api/plugins/upload":
			return testResp(200, `{"id":"agentteams-matrix-channel","version":"0.1.0","loaded":true}`), nil
		default:
			return testResp(404, "not found"), nil
		}
	})}
	m := &moduleInstaller{index: &MillforgeIndexHandler{client: c}, http: c}
	loaded, prev, status, msg := m.installOne(context.Background(), "http://worker.test", "agentteams-matrix", moduleInstallRequest{
		Source: "github", Repo: "o/r", Ref: "main", Version: "0.1.0",
	})
	if status != 200 || !loaded || prev != "0.0.9" {
		t.Fatalf("status=%d loaded=%v prev=%q msg=%s", status, loaded, prev, msg)
	}
}

func TestInstallOneFailsWhenNotLoaded(t *testing.T) {
	artifact := moduleIdentityZip(t, "teamharness", "0.1.0")
	sum := sha256.Sum256(artifact)
	index := fmt.Sprintf(`{"schema":1,"channel":"beta","modules":{"teamharness":{"latest":"0.1.0","versions":{"0.1.0":{"artifacts":[{"name":"teamharness-0.1.0-qwenpaw.zip","sha256":"%s"}]}}}}}`, hex.EncodeToString(sum[:]))
	c := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/releases/beta/channels.json"):
			return testResp(200, index), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases/teamharness/0.1.0/"):
			return testResp(200, string(artifact)), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/plugins":
			return testResp(200, `[{"id":"teamharness","version":"0.0.9"}]`), nil
		case r.Method == http.MethodPost && r.URL.Path == "/api/plugins/upload":
			return testResp(200, `{"id":"teamharness","loaded":false}`), nil
		default:
			return testResp(404, "not found"), nil
		}
	})}
	m := &moduleInstaller{index: &MillforgeIndexHandler{client: c}, http: c}
	_, _, status, msg := m.installOne(context.Background(), "http://worker.test", "teamharness", moduleInstallRequest{
		Source: "github", Repo: "o/r", Ref: "main", Version: "0.1.0",
	})
	if status != 502 || !strings.Contains(msg, "did not load") {
		t.Fatalf("status=%d msg=%q", status, msg)
	}
}
