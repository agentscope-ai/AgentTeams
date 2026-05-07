package main

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func createCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a resource",
	}
	cmd.AddCommand(createWorkerCmd())
	cmd.AddCommand(createTeamCmd())
	cmd.AddCommand(createHumanCmd())
	cmd.AddCommand(createManagerCmd())
	return cmd
}

// ---------------------------------------------------------------------------
// create worker
// ---------------------------------------------------------------------------

func createWorkerCmd() *cobra.Command {
	var (
		name             string
		model            string
		runtime          string
		image            string
		identity         string
		soul             string
		soulFile         string
		skills           string
		packageURI       string
		expose           string
		team             string
		role             string
		containerManaged bool
		outputFmt        string
		waitTimeout      time.Duration
		noWait           bool
	)

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Create a Worker",
		Long: `Create a new Worker resource via the controller REST API.

  hiclaw create worker --name alice --model qwen3.6-plus
  hiclaw create worker --name alice --soul-file /path/to/SOUL.md --skills github-operations
  hiclaw create worker --name charlie --runtime copaw --expose 8080,3000
  hiclaw create worker --name remote-worker --runtime copaw --container-managed=false
  To configure mcpServers, use a YAML manifest and pass it with 'hiclaw apply -f worker.yaml'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if err := validateWorkerName(name); err != nil {
				return err
			}
			if model == "" {
				model = defaultWorkerModel()
			}
			if soulFile != "" {
				data, err := os.ReadFile(soulFile)
				if err != nil {
					return fmt.Errorf("read --soul-file %q: %w", soulFile, err)
				}
				soul = string(data)
			}
			if packageURI != "" {
				var err error
				packageURI, err = expandPackageURI(packageURI)
				if err != nil {
					return err
				}
			}

			req := map[string]interface{}{
				"name":  name,
				"model": model,
			}
			setIfNotEmpty(req, "runtime", runtime)
			setIfNotEmpty(req, "image", image)
			setIfNotEmpty(req, "identity", identity)
			setIfNotEmpty(req, "soul", soul)
			setIfNotEmpty(req, "package", packageURI)
			setIfNotEmpty(req, "team", team)
			setIfNotEmpty(req, "role", role)
			if !containerManaged {
				req["containerManaged"] = false
			}
			if skills != "" {
				req["skills"] = splitCSV(skills)
			}
			if expose != "" {
				req["expose"] = parseExposePorts(expose)
			}

			client := NewAPIClient()
			var createResp map[string]interface{}
			if err := client.DoJSON("POST", "/api/v1/workers", req, &createResp); err != nil {
				return fmt.Errorf("create worker: %w", err)
			}

			if noWait {
				if outputFmt == "json" {
					printJSON(createResp)
				} else {
					fmt.Printf("worker/%s create accepted (poll `hiclaw get workers -o json` for phase=Running)\n", name)
				}
				return nil
			}

			finalStatus, err := waitForWorkerReady(client, name, waitTimeout)
			if err != nil {
				return err
			}

			if outputFmt == "json" {
				printJSON(finalStatus)
			} else {
				fmt.Printf("worker/%s ready\n", name)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (required)")
	cmd.Flags().StringVar(&model, "model", "", "LLM model ID (default: $HICLAW_DEFAULT_MODEL, else qwen3.6-plus)")
	cmd.Flags().StringVar(&runtime, "runtime", "", "Agent runtime (openclaw|copaw|hermes)")
	cmd.Flags().StringVar(&image, "image", "", "Container image override")
	cmd.Flags().StringVar(&identity, "identity", "", "Worker identity description")
	cmd.Flags().StringVar(&soul, "soul", "", "Worker SOUL.md content (inline)")
	cmd.Flags().StringVar(&soulFile, "soul-file", "", "Path to SOUL.md file (overrides --soul)")
	cmd.Flags().StringVar(&skills, "skills", "", "Comma-separated built-in skills")
	cmd.Flags().StringVar(&packageURI, "package", "", "Package URI (nacos://, http://, oss://) or shorthand")
	cmd.Flags().StringVar(&expose, "expose", "", "Comma-separated ports to expose (e.g. 8080,3000)")
	cmd.Flags().StringVar(&team, "team", "", "Team name (assigns worker to a team)")
	cmd.Flags().StringVar(&role, "role", "", "Role within team (team_leader|worker)")
	cmd.Flags().BoolVar(&containerManaged, "container-managed", true, "Whether controller manages container lifecycle (default true; set false for remote/pip workers)")
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "", "Output format (json)")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 3*time.Minute, "Maximum time to wait for the Worker to report Ready")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return immediately after the controller accepts the create request, without polling for Ready")
	return cmd
}

func waitForWorkerReady(client *APIClient, name string, timeout time.Duration) (*workerResp, error) {
	deadline := time.Now().Add(timeout)
	last := &workerResp{Name: name, Phase: "Pending"}

	for {
		var resp workerResp
		err := client.DoJSON("GET", "/api/v1/workers/"+name+"/status", nil, &resp)
		if err == nil {
			last = &resp
			switch resp.Phase {
			case "Ready":
				return &resp, nil
			case "Failed":
				return nil, fmt.Errorf("worker/%s failed during startup: %s", name, renderWorkerStatusSummary(&resp))
			}
		} else {
			var apiErr *APIError
			if !isRetryableWorkerStatusError(err, &apiErr) {
				return nil, fmt.Errorf("wait for worker/%s ready: %w", name, err)
			}
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("worker/%s did not become ready within %s (last status: %s)", name, timeout, renderWorkerStatusSummary(last))
		}

		time.Sleep(2 * time.Second)
	}
}

func isRetryableWorkerStatusError(err error, apiErr **APIError) bool {
	if err == nil {
		return false
	}
	typed, ok := err.(*APIError)
	if !ok {
		return false
	}
	if apiErr != nil {
		*apiErr = typed
	}
	return typed.StatusCode == 404 || typed.StatusCode >= 500
}

func renderWorkerStatusSummary(resp *workerResp) string {
	if resp == nil {
		return "unknown"
	}

	parts := []string{}
	if phase := strings.TrimSpace(resp.Phase); phase != "" {
		parts = append(parts, "phase="+phase)
	}
	if state := strings.TrimSpace(resp.ContainerState); state != "" {
		parts = append(parts, "state="+state)
	}
	if msg := strings.TrimSpace(resp.Message); msg != "" {
		parts = append(parts, "message="+msg)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// create team
// ---------------------------------------------------------------------------

func createTeamCmd() *cobra.Command {
	var (
		name                 string
		leaderName           string
		leaderModel          string
		leaderHeartbeatEvery string
		workerIdleTimeout    string
		workers              string
		description          string
	)

	cmd := &cobra.Command{
		Use:   "team",
		Short: "Create a Team",
		Long: `Create a new Team resource with a leader and optional workers.

  hiclaw create team --name alpha --leader-name alpha-lead
  hiclaw create team --name alpha --leader-name alpha-lead --workers alice,bob
  hiclaw create team --name alpha --leader-name alpha-lead --leader-model claude-sonnet-4-6 --description "Frontend team"
  hiclaw create team --name alpha --leader-name alpha-lead --leader-heartbeat-every 30m --worker-idle-timeout 12h`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if leaderName == "" {
				return fmt.Errorf("--leader-name is required")
			}

			leader := map[string]interface{}{
				"name": leaderName,
			}
			if leaderModel != "" {
				leader["model"] = leaderModel
			}
			if leaderHeartbeatEvery != "" {
				leader["heartbeat"] = map[string]interface{}{
					"enabled": true,
					"every":   leaderHeartbeatEvery,
				}
			}
			setIfNotEmpty(leader, "workerIdleTimeout", workerIdleTimeout)

			workerList := []interface{}{}
			if workers != "" {
				for _, w := range splitCSV(workers) {
					workerList = append(workerList, map[string]interface{}{"name": w})
				}
			}

			req := map[string]interface{}{
				"name":    name,
				"leader":  leader,
				"workers": workerList,
			}
			setIfNotEmpty(req, "description", description)

			client := NewAPIClient()
			var resp map[string]interface{}
			if err := client.DoJSON("POST", "/api/v1/teams", req, &resp); err != nil {
				return fmt.Errorf("create team: %w", err)
			}
			fmt.Printf("team/%s created\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Team name (required)")
	cmd.Flags().StringVar(&leaderName, "leader-name", "", "Leader worker name (required)")
	cmd.Flags().StringVar(&leaderModel, "leader-model", "", "Leader LLM model")
	cmd.Flags().StringVar(&leaderHeartbeatEvery, "leader-heartbeat-every", "", "Leader heartbeat interval (e.g. 30m)")
	cmd.Flags().StringVar(&workerIdleTimeout, "worker-idle-timeout", "", "Idle timeout before the leader may sleep workers (e.g. 12h)")
	cmd.Flags().StringVar(&workers, "workers", "", "Comma-separated worker names")
	cmd.Flags().StringVar(&description, "description", "", "Team description")
	return cmd
}

// ---------------------------------------------------------------------------
// create human
// ---------------------------------------------------------------------------

func createHumanCmd() *cobra.Command {
	var (
		name              string
		displayName       string
		email             string
		permissionLevel   int
		accessibleTeams   string
		accessibleWorkers string
		note              string
	)

	cmd := &cobra.Command{
		Use:   "human",
		Short: "Create a Human user",
		Long: `Create a new Human resource (Matrix account + room access).

  hiclaw create human --name bob --display-name "Bob Chen"
  hiclaw create human --name alice --display-name "Alice" --email alice@example.com --permission-level 50`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if displayName == "" {
				return fmt.Errorf("--display-name is required")
			}

			req := map[string]interface{}{
				"name":            name,
				"displayName":     displayName,
				"permissionLevel": permissionLevel,
			}
			setIfNotEmpty(req, "email", email)
			setIfNotEmpty(req, "note", note)
			if accessibleTeams != "" {
				req["accessibleTeams"] = splitCSV(accessibleTeams)
			}
			if accessibleWorkers != "" {
				req["accessibleWorkers"] = splitCSV(accessibleWorkers)
			}

			client := NewAPIClient()
			var resp map[string]interface{}
			if err := client.DoJSON("POST", "/api/v1/humans", req, &resp); err != nil {
				return fmt.Errorf("create human: %w", err)
			}
			fmt.Printf("human/%s created\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Human username (required)")
	cmd.Flags().StringVar(&displayName, "display-name", "", "Display name (required)")
	cmd.Flags().StringVar(&email, "email", "", "Email address")
	cmd.Flags().IntVar(&permissionLevel, "permission-level", 0, "Permission level (0-100)")
	cmd.Flags().StringVar(&accessibleTeams, "accessible-teams", "", "Comma-separated team names")
	cmd.Flags().StringVar(&accessibleWorkers, "accessible-workers", "", "Comma-separated worker names")
	cmd.Flags().StringVar(&note, "note", "", "Note for the Human user")
	return cmd
}

// ---------------------------------------------------------------------------
// create manager
// ---------------------------------------------------------------------------

func createManagerCmd() *cobra.Command {
	var (
		name    string
		model   string
		runtime string
		image   string
		soul    string
	)

	cmd := &cobra.Command{
		Use:   "manager",
		Short: "Create a Manager agent",
		Long: `Create a new Manager resource.

  hiclaw create manager --name default --model qwen3.6-plus
  hiclaw create manager --name default --model claude-sonnet-4-6 --runtime copaw`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if model == "" {
				return fmt.Errorf("--model is required")
			}

			req := map[string]interface{}{
				"name":  name,
				"model": model,
			}
			setIfNotEmpty(req, "runtime", runtime)
			setIfNotEmpty(req, "image", image)
			setIfNotEmpty(req, "soul", soul)

			client := NewAPIClient()
			var resp map[string]interface{}
			if err := client.DoJSON("POST", "/api/v1/managers", req, &resp); err != nil {
				return fmt.Errorf("create manager: %w", err)
			}
			fmt.Printf("manager/%s created\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Manager name (required)")
	cmd.Flags().StringVar(&model, "model", "", "LLM model ID (required)")
	cmd.Flags().StringVar(&runtime, "runtime", "", "Agent runtime (openclaw|copaw|hermes)")
	cmd.Flags().StringVar(&image, "image", "", "Container image override")
	cmd.Flags().StringVar(&soul, "soul", "", "Manager SOUL.md content")
	return cmd
}

// ---------------------------------------------------------------------------
// Helpers (migrated from old main.go)
// ---------------------------------------------------------------------------

var workerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// defaultWorkerModel returns the model ID to use when a CLI flag does not
// specify --model. It prefers the install-time configured model
// (HICLAW_DEFAULT_MODEL, propagated by the controller into both the manager
// and worker containers via WorkerEnvBuilder); only when the env var is unset
// does it fall back to the "qwen3.6-plus" default. Without this
// fallback every `hiclaw create worker` / `hiclaw apply worker` invoked by the
// Manager Agent would silently override the admin's install-time model choice.
func defaultWorkerModel() string {
	if m := strings.TrimSpace(os.Getenv("HICLAW_DEFAULT_MODEL")); m != "" {
		return m
	}
	return "qwen3.6-plus"
}

func validateWorkerName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("invalid worker name: name is required")
	}
	if !workerNamePattern.MatchString(name) {
		return fmt.Errorf("invalid worker name %q: must start with a lowercase letter or digit and contain only lowercase letters, digits, and hyphens", name)
	}
	return nil
}

func expandPackageURI(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return raw, nil
	}

	base := strings.TrimSpace(os.Getenv("HICLAW_NACOS_REGISTRY_URI"))
	if base == "" {
		base = "nacos://market.hiclaw.io:80/public"
	}
	if !strings.HasPrefix(base, "nacos://") {
		return "", fmt.Errorf("invalid HICLAW_NACOS_REGISTRY_URI %q: must start with nacos://", base)
	}
	base = strings.TrimRight(base, "/")
	if base == "nacos:" || base == "nacos:/" || base == "nacos://" {
		return "", fmt.Errorf("invalid HICLAW_NACOS_REGISTRY_URI %q: missing host/namespace", base)
	}

	parts := strings.Split(raw, "/")
	encoded := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return "", fmt.Errorf("invalid package shorthand %q: empty path segment", raw)
		}
		encoded = append(encoded, url.PathEscape(part))
	}

	return base + "/" + strings.Join(encoded, "/"), nil
}

func splitCSV(s string) []string {
	var result []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func parseExposePorts(s string) []map[string]interface{} {
	var ports []map[string]interface{}
	for _, p := range splitCSV(s) {
		port := map[string]interface{}{"port": p}
		ports = append(ports, port)
	}
	return ports
}

func setIfNotEmpty(m map[string]interface{}, key, value string) {
	if value != "" {
		m[key] = value
	}
}
