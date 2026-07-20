package main

import (
	"archive/zip"
	"bytes"
	"testing"
)

func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	w := zip.NewWriter(buf)
	for name, content := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestExtractWorkerFieldsFromZip(t *testing.T) {
	cases := []struct {
		name        string
		manifest    string
		wantModel   string
		wantRuntime string
	}{
		{
			name:        "empty zip, no manifest",
			manifest:    "",
			wantModel:   "",
			wantRuntime: "",
		},
		{
			name:        "manifest without worker block uses top-level fields",
			manifest:    `{"model":"top","runtime":"copaw"}`,
			wantModel:   "top",
			wantRuntime: "copaw",
		},
		{
			name:        "worker block overrides top-level (matches doc schema)",
			manifest:    `{"model":"top","runtime":"openclaw","worker":{"model":"nested","runtime":"copaw"}}`,
			wantModel:   "nested",
			wantRuntime: "copaw",
		},
		{
			name:        "worker block partially overrides leaves other top-level intact",
			manifest:    `{"runtime":"openclaw","worker":{"model":"only-model"}}`,
			wantModel:   "only-model",
			wantRuntime: "openclaw",
		},
		{
			name:        "missing fields stay empty so caller defaults can apply",
			manifest:    `{"worker":{"suggested_name":"alice"}}`,
			wantModel:   "",
			wantRuntime: "",
		},
		{
			name:        "invalid JSON returns empty (caller falls back)",
			manifest:    `{"worker":}`,
			wantModel:   "",
			wantRuntime: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			if tc.manifest != "" {
				files["manifest.json"] = tc.manifest
			}
			data := buildZip(t, files)
			gotModel, gotRuntime := extractWorkerFieldsFromZip(data)
			if gotModel != tc.wantModel {
				t.Errorf("model: got %q, want %q", gotModel, tc.wantModel)
			}
			if gotRuntime != tc.wantRuntime {
				t.Errorf("runtime: got %q, want %q", gotRuntime, tc.wantRuntime)
			}
		})
	}
}

func TestExtractWorkerFieldsFromZip_NotAZip(t *testing.T) {
	gotModel, gotRuntime := extractWorkerFieldsFromZip([]byte("not a zip"))
	if gotModel != "" || gotRuntime != "" {
		t.Errorf("expected empty fields for non-zip input, got model=%q runtime=%q", gotModel, gotRuntime)
	}
}
