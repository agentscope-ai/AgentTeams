package skillscan

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	passJSON  = `{"status":"pass","findings":[]}`
	warnJSON  = `{"status":"warn","findings":[{"rule_id":"style.lint","category":"style","severity":"MEDIUM","file":"SKILL.md","line":3,"title":"style"}]}`
	blockJSON = `{"status":"block","findings":[{"rule_id":"malicious.exec","category":"exec","severity":"CRITICAL","file":"scripts/run.sh","line":1,"title":"exec call"}]}`
)

// --- parseVerdict ---

func TestParseVerdict(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		v, err := parseVerdict(passJSON)
		if err != nil || v.Status != "pass" || len(v.Findings) != 0 {
			t.Fatalf("v=%+v err=%v", v, err)
		}
	})
	t.Run("warn keeps findings", func(t *testing.T) {
		v, err := parseVerdict(warnJSON)
		if err != nil || v.Status != "warn" || len(v.Findings) != 1 {
			t.Fatalf("v=%+v err=%v", v, err)
		}
		if f := v.Findings[0]; f.RuleID != "style.lint" || f.Severity != "MEDIUM" || f.File != "SKILL.md" || f.Line != 3 {
			t.Fatalf("finding=%+v", f)
		}
	})
	t.Run("block", func(t *testing.T) {
		v, err := parseVerdict(blockJSON)
		if err != nil || v.Status != "block" || len(v.Findings) != 1 {
			t.Fatalf("v=%+v err=%v", v, err)
		}
	})
	t.Run("unavailable status is an error", func(t *testing.T) {
		if _, err := parseVerdict(`{"status":"unavailable","detail":"no scanner"}`); err == nil {
			t.Fatal("unavailable: want error")
		}
	})
	t.Run("empty output is an error", func(t *testing.T) {
		if _, err := parseVerdict("   "); err == nil {
			t.Fatal("empty: want error")
		}
	})
	t.Run("unparseable output is an error", func(t *testing.T) {
		if _, err := parseVerdict("Traceback (most recent call last):"); err == nil {
			t.Fatal("garbage: want error")
		}
	})
	t.Run("unknown status is an error", func(t *testing.T) {
		if _, err := parseVerdict(`{"status":"meh"}`); err == nil {
			t.Fatal("unknown status: want error")
		}
	})
	t.Run("last line wins when noise precedes", func(t *testing.T) {
		out := "warning: something\n" + passJSON
		v, err := parseVerdict(out)
		if err != nil || v.Status != "pass" {
			t.Fatalf("v=%+v err=%v", v, err)
		}
	})
}

// --- contentKey ---

func TestContentKeyStableAndSensitive(t *testing.T) {
	a := map[string][]byte{"SKILL.md": []byte("v1"), "scripts/run.sh": []byte("x")}
	b := map[string][]byte{"scripts/run.sh": []byte("x"), "SKILL.md": []byte("v1")} // same, different map order
	c := map[string][]byte{"SKILL.md": []byte("v2"), "scripts/run.sh": []byte("x")} // changed content
	if contentKey("s", a) != contentKey("s", b) {
		t.Fatal("map order changed the key")
	}
	if contentKey("s", a) == contentKey("s", c) {
		t.Fatal("content change did not change the key")
	}
	if contentKey("s", a) == contentKey("other", a) {
		t.Fatal("name change did not change the key")
	}
}

// --- fake k8s client (Manager CR) ---

func managerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// --- fake Docker API ---

// fakeDocker emulates the Docker Engine API with the archive-API contract
// enforced: a PUT archive?path=X returns 404 when X does not exist as a
// directory inside the container (a fresh container has only / and /tmp),
// and tar members are extracted RELATIVE to X (an absolute member name is
// rejected). The probe exec is simulated: when it "finishes" and its
// payload directory holds files, the verdict file it was told to write
// appears in the fake filesystem — a scan whose payload never landed gets
// no verdict and therefore fails closed, like the real probe.
type fakeDocker struct {
	mu                    sync.Mutex
	uploadTars            [][]byte // bodies of the archive PUTs, in order
	verdictBody           string   // content of the verdict file ("404" → missing)
	execCount             int
	probeFirstPollRunning bool // first inspect of the probe exec reports Running
	execExitCode          int

	// fake container filesystem (lazy-initialized: fresh container).
	fs   map[string]string // file path -> content
	dirs map[string]bool
	// last probe exec: [tmpDir, name, verdictPath] from the exec Cmd.
	lastProbe struct {
		tmpDir  string
		verdict string
	}
	probeRan bool
}

func (f *fakeDocker) ensureFS() {
	if f.fs == nil {
		f.fs = map[string]string{}
		f.dirs = map[string]bool{"/": true, "/tmp": true}
	}
}

func (f *fakeDocker) addDir(p string) {
	// Mark p and every ancestor as an existing directory.
	cur := ""
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg == "" {
			continue
		}
		if cur == "" {
			cur = "/" + seg
		} else {
			cur = cur + "/" + seg
		}
		f.dirs[cur] = true
	}
	if p != "/" {
		f.dirs[p] = true
	}
}

func (f *fakeDocker) hasFilesUnder(prefix string) bool {
	for p := range f.fs {
		if strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func (f *fakeDocker) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.ensureFS()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/archive"):
		target := r.URL.Query().Get("path")
		if !f.dirs[target] {
			// Docker archive API: the destination directory must exist.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"path " + target + " does not exist"}`))
			return
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		tr := tar.NewReader(&buf)
		payload := false
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			if strings.HasPrefix(hdr.Name, "/") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			full := target + "/" + hdr.Name
			if hdr.Typeflag == tar.TypeDir {
				f.addDir(full)
				continue
			}
			payload = true
			var content bytes.Buffer
			_, _ = content.ReadFrom(tr)
			f.addDir(full[:len(full)-len(hdr.Name)-1])
			f.fs[full] = content.String()
		}
		if payload {
			// Record payload uploads only: the ensureScratchDir tar (dir
			// entry, no files) is bookkeeping, not a scan upload.
			f.uploadTars = append(f.uploadTars, buf.Bytes())
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/exec"):
		var body struct {
			Cmd []string `json:"Cmd"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Probe cmd: [python, -c, script, tmpDir, name, verdictPath].
		if len(body.Cmd) >= 6 && body.Cmd[1] == "-c" {
			f.lastProbe = struct {
				tmpDir  string
				verdict string
			}{tmpDir: body.Cmd[3], verdict: body.Cmd[5]}
			f.probeRan = false
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"exec-1"}`))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/json"):
		if f.execCount == 0 && f.probeFirstPollRunning {
			f.execCount++
			_, _ = w.Write([]byte(`{"Running":true}`))
			return
		}
		// Finished: run the simulated probe (it writes the verdict only
		// when its payload directory actually contains the skill files).
		if f.lastProbe.verdict != "" && !f.probeRan && f.verdictBody != "404" {
			f.probeRan = true
			if f.hasFilesUnder(f.lastProbe.tmpDir) {
				f.addDir(f.lastProbe.verdict[:len(f.lastProbe.verdict)-len("verdict")-1])
				f.fs[f.lastProbe.verdict] = f.verdictBody
			}
		}
		_, _ = w.Write([]byte(`{"Running":false,"ExitCode":` + itoa(f.execExitCode) + `}`))
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/archive"):
		file := r.URL.Query().Get("path")
		content, ok := f.fs[file]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Return the file as a one-entry tar.
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: file[strings.LastIndex(file, "/")+1:], Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
		_ = tw.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(buf.Bytes())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// --- Client-level tests ---

func newEmbeddedTestClient(t *testing.T, fd *fakeDocker, managers ...*v1beta1.Manager) *Client {
	t.Helper()
	objs := make([]runtime.Object, 0, len(managers))
	for _, m := range managers {
		objs = append(objs, m)
	}
	k8s := fake.NewClientBuilder().WithScheme(managerScheme(t)).WithRuntimeObjects(objs...).Build()
	ts := httptest.NewServer(http.HandlerFunc(fd.handler))
	t.Cleanup(ts.Close)
	return New(Config{
		KubeMode:   "embedded",
		Client:     k8s,
		Namespace:  "default",
		SocketPath: "/var/run/docker.sock",
		CacheTTL:   5 * time.Minute,
		CacheMax:   100,
		HTTPClient: ts.Client(),
		BaseURL:    ts.URL,
	})
}

func qwenpawManager() *v1beta1.Manager {
	return &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec:       v1beta1.ManagerSpec{Runtime: "qwenpaw"},
	}
}

func TestScanSkillEndToEndBlock(t *testing.T) {
	fd := &fakeDocker{verdictBody: blockJSON}
	c := newEmbeddedTestClient(t, fd, qwenpawManager())
	files := map[string][]byte{"SKILL.md": []byte("---\nname: s\n---\n"), "scripts/run.sh": []byte("x")}

	v, err := c.ScanSkill(context.Background(), "s", files)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v.Status != "block" || len(v.Findings) != 1 || v.Findings[0].Severity != "CRITICAL" {
		t.Fatalf("v=%+v", v)
	}
	// The payload was uploaded as a tar containing both files under the
	// uuid dir.
	if len(fd.uploadTars) < 1 {
		t.Fatal("no archive upload recorded")
	}
}

func TestScanSkillEndToEndPassWithPolling(t *testing.T) {
	fd := &fakeDocker{verdictBody: passJSON, probeFirstPollRunning: true}
	c := newEmbeddedTestClient(t, fd, qwenpawManager())

	v, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte("x")})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v.Status != "pass" {
		t.Fatalf("v=%+v", v)
	}
}

func TestScanSkillMissingVerdictFailsClosed(t *testing.T) {
	fd := &fakeDocker{verdictBody: "404"}
	c := newEmbeddedTestClient(t, fd, qwenpawManager())
	_, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte("x")})
	if err == nil {
		t.Fatal("missing verdict: want error (fail closed)")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v, want ErrUnavailable wrap", err)
	}
}

func TestScanSkillExecNonZeroExitFailsClosed(t *testing.T) {
	fd := &fakeDocker{verdictBody: passJSON, execExitCode: 1}
	c := newEmbeddedTestClient(t, fd, qwenpawManager())
	if _, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte("x")}); err == nil {
		t.Fatal("non-zero exit: want error")
	}
}

// --- container selection matrix ---

func TestSelectContainerMatrix(t *testing.T) {
	t.Run("manager qwenpaw selected", func(t *testing.T) {
		fd := &fakeDocker{verdictBody: passJSON}
		c := newEmbeddedTestClient(t, fd, qwenpawManager())
		if _, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte("x")}); err != nil {
			t.Fatalf("scan: %v", err)
		}
	})
	t.Run("manager non-qwenpaw unavailable", func(t *testing.T) {
		m := qwenpawManager()
		m.Spec.Runtime = "openclaw"
		fd := &fakeDocker{verdictBody: passJSON}
		c := newEmbeddedTestClient(t, fd, m)
		if _, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte("x")}); err == nil {
			t.Fatal("non-qwenpaw manager: want unavailable")
		}
	})
	t.Run("no manager unavailable", func(t *testing.T) {
		fd := &fakeDocker{verdictBody: passJSON}
		c := newEmbeddedTestClient(t, fd)
		if _, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte("x")}); err == nil {
			t.Fatal("no manager: want unavailable")
		}
	})
}

// --- cache / TTL / FIFO ---

func TestScanSkillCacheHitNoReExec(t *testing.T) {
	fd := &fakeDocker{verdictBody: passJSON}
	c := newEmbeddedTestClient(t, fd, qwenpawManager())
	files := map[string][]byte{"SKILL.md": []byte("v1")}
	for i := 0; i < 3; i++ {
		if _, err := c.ScanSkill(context.Background(), "s", files); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	// 3 scans, 1 unique payload → exactly one upload (cache hits).
	if n := len(fd.uploadTars); n != 1 {
		t.Fatalf("uploads = %d, want 1 (cache hit)", n)
	}
}

func TestScanSkillCacheTTLExpiry(t *testing.T) {
	fd := &fakeDocker{verdictBody: passJSON}
	c := newEmbeddedTestClient(t, fd, qwenpawManager())
	// Shrink the TTL.
	c.cfg.CacheTTL = time.Millisecond
	files := map[string][]byte{"SKILL.md": []byte("v1")}
	if _, err := c.ScanSkill(context.Background(), "s", files); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := c.ScanSkill(context.Background(), "s", files); err != nil {
		t.Fatal(err)
	}
	if n := len(fd.uploadTars); n != 2 {
		t.Fatalf("uploads = %d, want 2 (TTL expiry)", n)
	}
}

func TestScanSkillFIFOEviction(t *testing.T) {
	fd := &fakeDocker{verdictBody: passJSON}
	c := newEmbeddedTestClient(t, fd, qwenpawManager())
	c.cfg.CacheMax = 2
	for i, v := range []string{"v1", "v2", "v3"} {
		if _, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte(v)}); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	if n := len(fd.uploadTars); n != 3 {
		t.Fatalf("uploads = %d, want 3 (distinct content)", n)
	}
	// Re-scan the evicted payload → must exec again.
	if _, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	if n := len(fd.uploadTars); n != 4 {
		t.Fatalf("uploads after evicted re-scan = %d, want 4", n)
	}
}

// --- k8s mode fails closed ---

func TestClientK8sModeFailsClosed(t *testing.T) {
	c := New(Config{KubeMode: "k8s"})
	_, err := c.ScanSkill(context.Background(), "s", map[string][]byte{"SKILL.md": []byte("v")})
	if err == nil {
		t.Fatal("k8s mode: want fail-closed error")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v, want ErrUnavailable wrap", err)
	}
}

// --- payload tar layout (the probe scans exactly what the tar places) ---

// TestBuildSkillTarLayout: the probe scans exactly what the tar places —
// entries are extracted RELATIVE to the archive-API path (the scratch
// root): a per-scan directory entry first, then every file beneath it.
// Absolute or traversal member names are dropped (the probe then finds an
// empty payload and fails closed) instead of writing outside the scratch
// dir.
func TestBuildSkillTarLayout(t *testing.T) {
	tmpRoot := "/tmp/.skillscan"
	tmpDir := tmpRoot + "/abc123"
	files := map[string][]byte{"SKILL.md": []byte("root"), "scripts/run.sh": []byte("x")}
	tarBytes := buildSkillTar(files, tmpRoot, tmpDir)
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	seen := map[string]string{}
	var first *tar.Header
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if first == nil {
			first = hdr
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(tr)
		seen[hdr.Name] = buf.String()
		if strings.HasPrefix(hdr.Name, "/") {
			t.Fatalf("absolute member name %q (would escape the scratch root)", hdr.Name)
		}
	}
	if first == nil || first.Typeflag != tar.TypeDir || first.Name != "abc123/" {
		t.Fatalf("first entry = %+v, want the relative dir abc123/", first)
	}
	if got := seen["abc123/SKILL.md"]; got != "root" {
		t.Fatalf("SKILL.md entry = %q in %v", got, seen)
	}
	if got := seen["abc123/scripts/run.sh"]; got != "x" {
		t.Fatalf("nested entry lost: %v", seen)
	}
	if len(seen) != 3 {
		t.Fatalf("unexpected entries: %v", seen)
	}

	// Absolute and traversal paths are dropped; only the directory entry
	// remains, so the probe fails closed on the empty payload.
	dropped := buildSkillTar(map[string][]byte{
		"/etc/evil": []byte("x"),
		"a/../b":    []byte("y"),
	}, tmpRoot, tmpDir)
	tr = tar.NewReader(bytes.NewReader(dropped))
	count := 0
	for {
		if _, err := tr.Next(); err != nil {
			break
		}
		count++
	}
	if count != 1 {
		t.Fatalf("members = %d, want only the directory entry (invalid files dropped)", count)
	}
}

// --- probe script contract pins (the gate is the gate) ---

func TestProbeScriptContract(t *testing.T) {
	if !strings.Contains(probeScript, `scan_skill(src, skill_name=name)`) {
		t.Error("probe lost the direct SkillScanner().scan_skill path")
	}
	if !strings.Contains(probeScript, "scan_skill_directory(src, skill_name=name, block=True)") {
		t.Error("probe lost the forced-block fallback for older runtimes")
	}
	if !strings.Contains(probeScript, `"unavailable"`) {
		t.Error("probe must distinguish infrastructure failure from a content verdict")
	}
	if !strings.Contains(probeScript, `sys.argv[3]`) {
		t.Error("probe must write the verdict to the controller-specified path")
	}
}
