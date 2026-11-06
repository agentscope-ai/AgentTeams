package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
)

func moduleZip(t *testing.T, manifests map[string]map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for path, manifest := range manifests {
		f, err := w.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.NewEncoder(f).Encode(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func moduleIdentityZip(t *testing.T, id, version string) []byte {
	return moduleZip(t, map[string]map[string]string{"plugin.json": {"id": id, "version": version}})
}

func TestInstallOneRejectsWrongArtifactIdentity(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"wrong-id", moduleIdentityZip(t, "unrequested", "0.1.0"), false},
		{"wrong-version", moduleIdentityZip(t, "teamharness", "9.9.9"), false},
		{"invalid-zip", []byte("not a zip"), false},
		{"missing-manifest", moduleZip(t, map[string]map[string]string{}), false},
		{"ambiguous-wrappers", moduleZip(t, map[string]map[string]string{"a/plugin.json": {"id": "teamharness", "version": "0.1.0"}, "b/plugin.json": {"id": "other", "version": "0.1.0"}}), false},
		{"root", moduleIdentityZip(t, "teamharness", "0.1.0"), true},
		{"wrapped", moduleZip(t, map[string]map[string]string{"package/plugin.json": {"id": "teamharness", "version": "0.1.0"}}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum := sha256.Sum256(tc.data)
			m, c := testInstaller(tc.data, hex.EncodeToString(sum[:]))
			next := c.Transport
			uploads := 0
			c.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					uploads++
					return testResp(200, `{"id":"teamharness","version":"0.1.0","loaded":true}`), nil
				}
				return next.RoundTrip(r)
			})
			loaded, _, status, msg := m.installOne(context.Background(), "http://worker.test", "teamharness", moduleInstallRequest{Source: "github", Repo: "o/r", Version: "0.1.0"})
			if tc.ok {
				if status != 200 || !loaded || uploads != 1 {
					t.Fatalf("valid package: status=%d loaded=%v uploads=%d msg=%s", status, loaded, uploads, msg)
				}
			} else if status != 502 || loaded || uploads != 0 {
				t.Fatalf("must refuse before upload: status=%d loaded=%v uploads=%d msg=%s", status, loaded, uploads, msg)
			}
		})
	}
}

func TestInstallOneChecksUploadedIdentity(t *testing.T) {
	for _, body := range []string{`{"loaded":true}`, `{"id":"other","version":"0.1.0","loaded":true}`, `{"id":"teamharness","version":"9.9.9","loaded":true}`} {
		t.Run(body, func(t *testing.T) {
			data := moduleIdentityZip(t, "teamharness", "0.1.0")
			sum := sha256.Sum256(data)
			m, c := testInstaller(data, hex.EncodeToString(sum[:]))
			next := c.Transport
			c.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					return testResp(200, body), nil
				}
				return next.RoundTrip(r)
			})
			loaded, _, status, msg := m.installOne(context.Background(), "http://worker.test", "teamharness", moduleInstallRequest{Source: "github", Repo: "o/r", Version: "0.1.0"})
			if status != 502 || loaded {
				t.Fatalf("wrong upload receipt: status=%d loaded=%v msg=%s", status, loaded, msg)
			}
		})
	}
}
