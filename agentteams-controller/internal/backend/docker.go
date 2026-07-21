package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DockerConfig holds Docker backend configuration.
type DockerConfig struct {
	SocketPath           string
	WorkerImage          string // default worker image (AGENTTEAMS_WORKER_IMAGE)
	CopawWorkerImage     string // default copaw worker image (AGENTTEAMS_COPAW_WORKER_IMAGE)
	HermesWorkerImage    string // default hermes worker image (AGENTTEAMS_HERMES_WORKER_IMAGE)
	OpenHumanWorkerImage string // default openhuman worker image (AGENTTEAMS_OPENHUMAN_WORKER_IMAGE)
	QwenPawWorkerImage   string // default qwenpaw worker image (AGENTTEAMS_QWENPAW_WORKER_IMAGE)
	DefaultNetwork       string // default Docker network (default "hiclaw-net")
}

// DockerBackend manages worker containers via the Docker Engine API over a Unix socket.
type DockerBackend struct {
	config          DockerConfig
	client          *http.Client
	containerPrefix string
}

// NewDockerBackend creates a DockerBackend that talks to the given Docker socket.
func NewDockerBackend(config DockerConfig, containerPrefix string) *DockerBackend {
	transport := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", config.SocketPath)
		},
	}
	return &DockerBackend{
		config:          config,
		client:          &http.Client{Transport: transport},
		containerPrefix: containerPrefix,
	}
}

// WithPrefix returns a shallow copy of the backend with a different container name prefix.
// The returned backend shares the same HTTP client (safe — client is read-only).
// Use WithPrefix("") to disable prefix for containers that already have full names
// (e.g. Manager containers named "agentteams-manager" rather than "agentteams-worker-X").
func (d *DockerBackend) WithPrefix(prefix string) *DockerBackend {
	cp := *d
	cp.containerPrefix = prefix
	return &cp
}

func (d *DockerBackend) Name() string                   { return "docker" }
func (d *DockerBackend) DeploymentMode() string         { return DeployLocal }
func (d *DockerBackend) NeedsCredentialInjection() bool { return false }

func (d *DockerBackend) Available(ctx context.Context) bool {
	// Check socket file exists
	if _, err := os.Stat(d.config.SocketPath); err != nil {
		return false
	}
	// Ping the Docker daemon
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pingCtx, http.MethodGet, "http://localhost/_ping", nil)
	if err != nil {
		return false
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (d *DockerBackend) Create(ctx context.Context, req CreateRequest) (*WorkerResult, error) {
	var containerName string
	if req.ContainerName != "" {
		containerName = req.ContainerName
	} else {
		prefix := d.containerPrefix
		if req.NamePrefix != "" {
			prefix = req.NamePrefix
		}
		containerName = prefix + req.Name
	}

	// Resolve effective runtime once: explicit > caller fallback > openclaw.
	// We do this before image fallback so all runtime-dependent decisions
	// (image, working dir, labels) see a consistent normalized value. The
	// CRD intentionally does not pin a default — see ResolveRuntime godoc.
	// Caller (worker / manager reconciler) is responsible for picking the
	// right env var for RuntimeFallback (AGENTTEAMS_DEFAULT_WORKER_RUNTIME for
	// workers, AGENTTEAMS_MANAGER_RUNTIME for managers).
	req.Runtime = ResolveRuntime(req.Runtime, req.RuntimeFallback)

	// Default image fallback
	image := req.Image
	if image == "" {
		switch {
		case req.Runtime == RuntimeCopaw && d.config.CopawWorkerImage != "":
			image = d.config.CopawWorkerImage
		case req.Runtime == RuntimeHermes && d.config.HermesWorkerImage != "":
			image = d.config.HermesWorkerImage
		case req.Runtime == RuntimeOpenHuman && d.config.OpenHumanWorkerImage != "":
			image = d.config.OpenHumanWorkerImage
		case req.Runtime == RuntimeQwenPaw && d.config.QwenPawWorkerImage != "":
			image = d.config.QwenPawWorkerImage
		default:
			image = d.config.WorkerImage
		}
	}
	req.Image = image

	// Default network fallback
	if req.Network == "" && d.config.DefaultNetwork != "" {
		req.Network = d.config.DefaultNetwork
	}

	// Inject SA token for worker-to-controller authentication (embedded mode).
	if req.AuthToken != "" {
		if req.Env == nil {
			req.Env = make(map[string]string)
		}
		req.Env["AGENTTEAMS_AUTH_TOKEN"] = req.AuthToken
	}
	if req.ControllerURL != "" {
		req.Env["AGENTTEAMS_CONTROLLER_URL"] = req.ControllerURL
	}

	// Infer WorkingDir from HOME env if not set
	if req.WorkingDir == "" {
		if home, ok := req.Env["HOME"]; ok {
			req.WorkingDir = home
		}
	}

	// Ensure image is available locally, pull if needed
	if err := d.ensureImage(ctx, req.Image); err != nil {
		return nil, err
	}

	// Detect console port from env (for CoPaw workers)
	consolePort := ""
	if req.Env != nil {
		consolePort = firstNonEmptyTrimmed(req.Env["AGENTTEAMS_CONSOLE_PORT"])
	}

	// Pick a random host port for console binding
	hostPort := 0
	if consolePort != "" {
		hostPort = 10000 + rand.Intn(10001)
	}

	const maxPortRetries = 10
	for attempt := 0; ; attempt++ {
		payload := d.buildCreatePayload(req, consolePort, hostPort)
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal create payload: %w", err)
		}

		containerID, err := d.doCreate(ctx, containerName, body)
		if err != nil {
			return nil, err
		}

		// Start the container
		startErr := d.startContainer(ctx, containerID)
		if startErr == nil {
			result := &WorkerResult{
				Name:           req.Name,
				Backend:        "docker",
				DeploymentMode: DeployLocal,
				Status:         StatusRunning,
				ContainerID:    containerID,
				RawStatus:      "running",
			}
			if consolePort != "" && hostPort > 0 {
				result.ConsoleHostPort = strconv.Itoa(hostPort)
				log.Printf("[Docker] Console: container port %s -> host port %d", consolePort, hostPort)
			}
			return result, nil
		}

		// Check if start failed due to port conflict — retry with different port
		errMsg := startErr.Error()
		if consolePort != "" && attempt < maxPortRetries &&
			(strings.Contains(errMsg, "already allocated") ||
				strings.Contains(errMsg, "address already in use") ||
				strings.Contains(errMsg, "port is already")) {
			log.Printf("[Docker] Host port %d in use, retrying with %d...", hostPort, hostPort+1)
			hostPort++
			// Clean up the container we just created
			d.Delete(ctx, req.Name)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		return nil, fmt.Errorf("start after create: %w", startErr)
	}
}

// doCreate sends the container create request to Docker, handling conflict by
// deleting the existing container and retrying once.
func (d *DockerBackend) doCreate(ctx context.Context, containerName string, body []byte) (string, error) {
	for retry := 0; retry < 2; retry++ {
		u := fmt.Sprintf("http://localhost/containers/create?name=%s", url.QueryEscape(containerName))
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
		if err != nil {
			return "", fmt.Errorf("build create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := d.client.Do(httpReq)
		if err != nil {
			return "", fmt.Errorf("docker create: %w", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusConflict && retry == 0 {
			// Remove existing container and retry once
			log.Printf("[Docker] Container %s already exists, removing before recreate", containerName)
			// Extract worker name from container name
			name := strings.TrimPrefix(containerName, d.containerPrefix)
			if err := d.Delete(ctx, name); err != nil {
				return "", fmt.Errorf("delete existing container: %w", err)
			}
			time.Sleep(1 * time.Second)
			continue
		}
		if resp.StatusCode == http.StatusConflict {
			return "", fmt.Errorf("%w: container %q", ErrConflict, containerName)
		}
		if resp.StatusCode != http.StatusCreated {
			return "", fmt.Errorf("docker create failed (status %d): %s", resp.StatusCode, string(respBody))
		}

		var createResp struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal(respBody, &createResp); err != nil {
			return "", fmt.Errorf("parse create response: %w", err)
		}
		return createResp.ID, nil
	}
	return "", fmt.Errorf("docker create: exhausted retries")
}

func (d *DockerBackend) Delete(ctx context.Context, name string) error {
	containerName := d.containerPrefix + name
	u := fmt.Sprintf("http://localhost/containers/%s?force=true", url.PathEscape(containerName))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker delete: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker delete failed (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func (d *DockerBackend) Start(ctx context.Context, name string) error {
	containerName := d.containerPrefix + name
	if err := d.startContainer(ctx, containerName); err != nil {
		if strings.Contains(err.Error(), "status 404") {
			return fmt.Errorf("%w: worker %q", ErrNotFound, name)
		}
		return err
	}
	return nil
}

func (d *DockerBackend) Stop(ctx context.Context, name string) error {
	containerName := d.containerPrefix + name
	u := fmt.Sprintf("http://localhost/containers/%s/stop?t=10", url.PathEscape(containerName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker stop: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: worker %q", ErrNotFound, name)
	}
	if resp.StatusCode == http.StatusNotModified {
		return nil // already stopped
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker stop failed (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func (d *DockerBackend) Status(ctx context.Context, name string) (*WorkerResult, error) {
	containerName := d.containerPrefix + name
	u := fmt.Sprintf("http://localhost/containers/%s/json", url.PathEscape(containerName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &WorkerResult{
			Name:           name,
			Backend:        "docker",
			DeploymentMode: DeployLocal,
			Status:         StatusNotFound,
		}, nil
	}

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker inspect failed (status %d): %s", resp.StatusCode, string(body))
	}

	var inspectResp struct {
		ID    string `json:"Id"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal(body, &inspectResp); err != nil {
		return nil, fmt.Errorf("parse inspect response: %w", err)
	}

	return &WorkerResult{
		Name:           name,
		Backend:        "docker",
		DeploymentMode: DeployLocal,
		Status:         normalizeDockerStatus(inspectResp.State.Status),
		ContainerID:    inspectResp.ID,
		RawStatus:      inspectResp.State.Status,
	}, nil
}

// --- internal helpers ---

// ensureImage checks if an image exists locally and pulls it if not.
func (d *DockerBackend) ensureImage(ctx context.Context, image string) error {
	// Check if image exists locally
	// Note: Docker Engine API expects unescaped image names in the path
	// (e.g. /images/agentteams/worker-agent:latest/json), not PathEscaped.
	u := "http://localhost/images/" + image + "/json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build image inspect request: %w", err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker image inspect: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil // image exists
	}

	// Pull the image
	log.Printf("[Docker] Image not found locally, pulling: %s", image)
	pullURL := fmt.Sprintf("http://localhost/images/create?fromImage=%s", url.QueryEscape(image))
	pullReq, err := http.NewRequestWithContext(ctx, http.MethodPost, pullURL, nil)
	if err != nil {
		return fmt.Errorf("build image pull request: %w", err)
	}
	pullResp, err := d.client.Do(pullReq)
	if err != nil {
		return fmt.Errorf("docker image pull: %w", err)
	}
	// Read full body to wait for pull completion (Docker streams progress JSON)
	io.Copy(io.Discard, pullResp.Body)
	pullResp.Body.Close()

	// Verify image is now available
	verifyReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build image verify request: %w", err)
	}
	verifyResp, err := d.client.Do(verifyReq)
	if err != nil {
		return fmt.Errorf("docker image verify: %w", err)
	}
	verifyResp.Body.Close()

	if verifyResp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to pull image %s", image)
	}
	log.Printf("[Docker] Image pulled successfully: %s", image)
	return nil
}

func (d *DockerBackend) startContainer(ctx context.Context, nameOrID string) error {
	u := fmt.Sprintf("http://localhost/containers/%s/start", url.PathEscape(nameOrID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker start: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil // already running
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("docker start failed (status 404): container not found")
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker start failed (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// dockerCreatePayload is the Docker Engine API container create body.
type dockerCreatePayload struct {
	Image            string                  `json:"Image"`
	Env              []string                `json:"Env,omitempty"`
	WorkingDir       string                  `json:"WorkingDir,omitempty"`
	ExposedPorts     map[string]struct{}     `json:"ExposedPorts,omitempty"`
	HostConfig       *dockerHostConfig       `json:"HostConfig,omitempty"`
	NetworkingConfig *dockerNetworkingConfig `json:"NetworkingConfig,omitempty"`
}

type dockerHostConfig struct {
	NetworkMode   string                         `json:"NetworkMode,omitempty"`
	ExtraHosts    []string                       `json:"ExtraHosts,omitempty"`
	Binds         []string                       `json:"Binds,omitempty"`
	PortBindings  map[string][]dockerPortBinding `json:"PortBindings,omitempty"`
	RestartPolicy *dockerRestartPolicy           `json:"RestartPolicy,omitempty"`
	SecurityOpt   []string                       `json:"SecurityOpt,omitempty"`
}

type dockerRestartPolicy struct {
	Name string `json:"Name"`
}

type dockerNetworkingConfig struct {
	EndpointsConfig map[string]*dockerEndpointSettings `json:"EndpointsConfig,omitempty"`
}

type dockerEndpointSettings struct {
	Aliases []string `json:"Aliases,omitempty"`
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort"`
}

func (d *DockerBackend) buildCreatePayload(req CreateRequest, consolePort string, hostPort int) dockerCreatePayload {
	// Sort env keys for deterministic output
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	envList := make([]string, 0, len(req.Env))
	for _, k := range keys {
		envList = append(envList, k+"="+req.Env[k])
	}

	p := dockerCreatePayload{
		Image:      req.Image,
		Env:        envList,
		WorkingDir: req.WorkingDir,
	}

	hc := &dockerHostConfig{
		NetworkMode: req.Network,
		ExtraHosts:  req.ExtraHosts,
	}

	// Bind mounts
	for _, v := range req.Volumes {
		bind := v.HostPath + ":" + v.ContainerPath
		if v.ReadOnly {
			bind += ":ro"
		}
		hc.Binds = append(hc.Binds, bind)
	}

	// Restart policy
	if req.RestartPolicy != "" {
		hc.RestartPolicy = &dockerRestartPolicy{Name: req.RestartPolicy}
	}

	// Console port binding (CoPaw workers)
	if consolePort != "" && hostPort > 0 {
		portKey := consolePort + "/tcp"
		p.ExposedPorts = map[string]struct{}{portKey: {}}
		hc.PortBindings = map[string][]dockerPortBinding{
			portKey: {{HostPort: strconv.Itoa(hostPort)}},
		}
	}

	// Additional port mappings
	for _, pm := range req.Ports {
		proto := pm.Protocol
		if proto == "" {
			proto = "tcp"
		}
		portKey := pm.ContainerPort + "/" + proto
		if p.ExposedPorts == nil {
			p.ExposedPorts = make(map[string]struct{})
		}
		p.ExposedPorts[portKey] = struct{}{}
		if hc.PortBindings == nil {
			hc.PortBindings = make(map[string][]dockerPortBinding)
		}
		hc.PortBindings[portKey] = append(hc.PortBindings[portKey], dockerPortBinding{
			HostIP:   pm.HostIP,
			HostPort: pm.HostPort,
		})
	}

	if hc.NetworkMode != "" || len(hc.ExtraHosts) > 0 || len(hc.PortBindings) > 0 ||
		len(hc.Binds) > 0 || hc.RestartPolicy != nil {
		p.HostConfig = hc
	}

	// Network aliases
	if len(req.NetworkAliases) > 0 && req.Network != "" {
		p.NetworkingConfig = &dockerNetworkingConfig{
			EndpointsConfig: map[string]*dockerEndpointSettings{
				req.Network: {Aliases: req.NetworkAliases},
			},
		}
	}

	return p
}

func normalizeDockerStatus(status string) WorkerStatus {
	switch strings.ToLower(status) {
	case "running":
		return StatusRunning
	case "exited", "dead":
		return StatusStopped
	case "created", "restarting":
		return StatusStarting
	default:
		return StatusUnknown
	}
}
