package hermes

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNewReturnsEmbeddedSurface(t *testing.T) {
	a := New()
	s := a.Surface()
	if s.Runtime != "hermes" {
		t.Fatalf("runtime = %q", s.Runtime)
	}
	if s.SurfaceVersion == "" {
		t.Fatal("empty surfaceVersion")
	}
}

func TestAdapterFieldForKey(t *testing.T) {
	a := New()
	fld, ok := a.FieldForKey("providers.openrouter.api_key")
	if !ok || fld.Key != "providers.*.api_key" {
		t.Fatalf("wildcard: %+v %v", fld, ok)
	}
	fld, ok = a.FieldForKey("delegation.model")
	if !ok || fld.Key != "delegation.model" {
		t.Fatalf("exact: %+v %v", fld, ok)
	}
	if _, ok := a.FieldForKey("no.such.key"); ok {
		t.Fatal("undeclared key must not resolve")
	}
}

func TestConfigRev(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("a: 1\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	rev, err := ConfigRev(path)
	if err != nil {
		t.Fatalf("ConfigRev: %v", err)
	}
	nsStr, sizeStr, ok := strings.Cut(rev, ":")
	if !ok {
		t.Fatalf("rev %q is not mtime_ns:size", rev)
	}
	ns, err := strconv.ParseInt(nsStr, 10, 64)
	if err != nil || ns <= 0 {
		t.Fatalf("mtime_ns %q parsed to %d (%v)", nsStr, ns, err)
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil || size != int64(len(content)) {
		t.Fatalf("size = %s (%v), want %d", sizeStr, err, len(content))
	}
	// A different size must change the rev even if the mtime did not.
	if err := os.WriteFile(path, []byte("a: 123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev2, err := ConfigRev(path)
	if err != nil {
		t.Fatalf("ConfigRev after write: %v", err)
	}
	if rev2 == rev {
		t.Fatalf("rev unchanged after content change: %q", rev)
	}
}

func TestConfigRevMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	_, err := ConfigRev(path)
	if err == nil {
		t.Fatal("missing file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("error %q does not name the missing file", err)
	}
}
