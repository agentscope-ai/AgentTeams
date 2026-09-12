package skillscan

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// scanTarget is the resolved scan location: one container + the temp paths
// used for this scan (the uuid ties the payload dir and the verdict file).
type scanTarget struct {
	container string
	tmpDir    string // <tmpRoot>/<uuid> — the skill payload inside the container
	verdict   string // <tmpRoot>/<uuid>.verdict — the probe's JSON output
}

// dockerBackend scans via the Docker Engine API over the mounted socket —
// the same transport pattern as internal/backend (raw HTTP, no CLI: the
// controller image is alpine and has no docker binary). Content transfer is
// the archive API (one tar upload, one tar download), execution is exec
// create + detached start + inspect polling.
type dockerBackend struct {
	cfg     Config
	cli     *http.Client
	baseURL string
	k8s     client.Client // Manager CR lookup (container + runtime)
}

// newDockerBackend builds the embedded-mode backend. cli may be a test
// client (httptest TCP); in production it dials the unix socket.
func newDockerBackend(cfg Config, cli *http.Client, baseURL string) *dockerBackend {
	return &dockerBackend{cfg: cfg, cli: cli, baseURL: baseURL, k8s: cfg.Client}
}

// Run executes one scan: upload the payload, run the probe, read the
// verdict, clean up. It returns the probe's JSON line (the caller parses
// it) or an error (the caller fails closed).
func (b *dockerBackend) Run(ctx context.Context, name string, files map[string][]byte) (string, error) {
	target, err := b.selectScanTarget(ctx)
	if err != nil {
		return "", err
	}
	logger := log.FromContext(ctx)

	if err := b.uploadArchive(ctx, target.container, tmpRoot, buildSkillTar(files, target.tmpDir)); err != nil {
		return "", fmt.Errorf("upload scan payload: %w", err)
	}
	// Best-effort cleanup on every exit path.
	defer b.cleanup(ctx, target, logger)

	cmd := []string{b.cfg.PythonBin, "-c", probeScript, target.tmpDir, name, target.verdict}
	if err := b.execDetached(ctx, target.container, cmd); err != nil {
		return "", fmt.Errorf("probe exec: %w", err)
	}
	verdict, err := b.downloadFile(ctx, target.container, target.verdict)
	if err != nil {
		// A missing verdict file means the probe died before writing —
		// an infrastructure failure, never a pass.
		return "", fmt.Errorf("read probe verdict: %w", err)
	}
	return verdict, nil
}

// selectScanTarget resolves the scan container from the Manager CRs:
// v1 scans only in the default-manager container when its runtime is
// qwenpaw (production managers run qwenpaw). Any other situation (no
// Manager CR, non-qwenpaw runtime, API failure) is "unavailable" — the
// caller fails closed. A worker-container fallback is a follow-up.
func (b *dockerBackend) selectScanTarget(ctx context.Context) (*scanTarget, error) {
	if b.k8s == nil {
		return nil, errors.New("no k8s client: cannot resolve the scan container")
	}
	var managers v1beta1.ManagerList
	if err := b.k8s.List(ctx, &managers, client.InNamespace(b.cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("list managers: %w", err)
	}
	var manager *v1beta1.Manager
	for i := range managers.Items {
		if managers.Items[i].Name == "default" {
			manager = &managers.Items[i]
			break
		}
	}
	if manager == nil && len(managers.Items) > 0 {
		manager = &managers.Items[0]
	}
	if manager == nil {
		return nil, errors.New("no Manager CR found")
	}
	if manager.Spec.Runtime != "qwenpaw" {
		return nil, fmt.Errorf("manager runtime %q cannot run the qwenpaw skill scanner", manager.Spec.Runtime)
	}

	uid, err := shortUUID()
	if err != nil {
		return nil, err
	}
	prefix := b.cfg.ResourcePrefix.Or(authpkg.DefaultResourcePrefix)
	return &scanTarget{
		container: prefix.ManagerPodName(manager.Name),
		tmpDir:    tmpRoot + "/" + uid,
		verdict:   tmpRoot + "/" + uid + ".verdict",
	}, nil
}

// buildSkillTar renders the payload as a tar whose entries live under
// <tmpDir>/ — one upload materializes the whole skill directory.
func buildSkillTar(files map[string][]byte, tmpDir string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for path, data := range files {
		hdr := &tar.Header{
			Name: tmpDir + "/" + path,
			Mode: 0o644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			// Cannot happen with a valid Header; the upload will fail.
			continue
		}
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	return buf.Bytes()
}

// uploadArchive PUTs a tar into the container (Docker archive API). path is
// the directory the tar entries are extracted relative to.
func (b *dockerBackend) uploadArchive(ctx context.Context, container, path string, tarBytes []byte) error {
	u := fmt.Sprintf("%s/containers/%s/archive?path=%s", b.baseURL, url.PathEscape(container), url.QueryEscape(path))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(tarBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := b.cli.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("archive upload failed (status %d): %s", resp.StatusCode, truncate(string(body)))
	}
	return nil
}

// downloadFile GETs a single file from the container (the Docker archive
// API returns it as a one-entry tar).
func (b *dockerBackend) downloadFile(ctx context.Context, container, path string) (string, error) {
	u := fmt.Sprintf("%s/containers/%s/archive?path=%s", b.baseURL, url.PathEscape(container), url.QueryEscape(path))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := b.cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errors.New("verdict file missing (the probe produced no output)")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("archive download failed (status %d): %s", resp.StatusCode, truncate(string(body)))
	}
	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read verdict tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", err
			}
			return string(data), nil
		}
	}
	return "", errors.New("verdict tar had no regular file entry")
}

// execDetached creates an exec, starts it detached (no attach stream —
// the probe writes its result to a file), and polls inspect until the
// process exits. A non-zero exit is an error.
func (b *dockerBackend) execDetached(ctx context.Context, container string, cmd []string) error {
	// 1. Create.
	payload, _ := json.Marshal(struct {
		Cmd []string `json:"Cmd"`
	}{Cmd: cmd})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.baseURL+"/containers/"+url.PathEscape(container)+"/exec", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.cli.Do(req)
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("exec create failed (status %d): %s", resp.StatusCode, truncate(string(body)))
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return fmt.Errorf("parse exec create response: %s", truncate(string(body)))
	}

	// 2. Start detached.
	startReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.baseURL+"/exec/"+url.PathEscape(created.ID)+"/start",
		strings.NewReader(`{"Detach":true,"Tty":false}`))
	if err != nil {
		return err
	}
	startReq.Header.Set("Content-Type", "application/json")
	startResp, err := b.cli.Do(startReq)
	if err != nil {
		return fmt.Errorf("exec start: %w", err)
	}
	startBody, _ := io.ReadAll(startResp.Body)
	startResp.Body.Close()
	if startResp.StatusCode != http.StatusOK && startResp.StatusCode != http.StatusNoContent && startResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("exec start failed (status %d): %s", startResp.StatusCode, truncate(string(startBody)))
	}

	// 3. Poll inspect until the exec exits.
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("probe exec timed out: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
		info, err := b.execInspect(ctx, created.ID)
		if err != nil {
			return err
		}
		if !info.Running {
			if info.ExitCode != 0 {
				return fmt.Errorf("probe exited with code %d", info.ExitCode)
			}
			return nil
		}
	}
}

func (b *dockerBackend) execInspect(ctx context.Context, id string) (struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}, error) {
	var out struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/exec/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return out, err
	}
	resp, err := b.cli.Do(req)
	if err != nil {
		return out, fmt.Errorf("exec inspect: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("exec inspect failed (status %d): %s", resp.StatusCode, truncate(string(body)))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("parse exec inspect: %w", err)
	}
	return out, nil
}

// cleanup removes the payload dir and the verdict file (best effort — a
// cleanup failure must not fail a scan that already produced a verdict).
func (b *dockerBackend) cleanup(ctx context.Context, t *scanTarget, logger interface {
	Info(msg string, keys ...any)
}) {
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := []string{"rm", "-rf", t.tmpDir, t.verdict}
	if err := b.execDetached(cctx, t.container, cmd); err != nil && logger != nil {
		logger.Info("skillscan cleanup failed (ignored)", "container", t.container, "err", err.Error())
	}
}

// shortUUID returns 16 hex chars — enough uniqueness for a temp path whose
// lifetime is one scan.
func shortUUID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// truncate shortens error bodies for log safety.
func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// newSocketClient builds the production Docker API client (unix socket).
func newSocketClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
}
