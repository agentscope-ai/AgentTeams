package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func workerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Worker lifecycle operations",
	}
	cmd.AddCommand(workerWakeCmd())
	cmd.AddCommand(workerSleepCmd())
	cmd.AddCommand(workerEnsureReadyCmd())
	cmd.AddCommand(workerStatusCmd())
	cmd.AddCommand(workerReportReadyCmd())
	cmd.AddCommand(workerHeartbeatCmd())
	return cmd
}

// ---------------------------------------------------------------------------
// worker wake
// ---------------------------------------------------------------------------

func workerWakeCmd() *cobra.Command {
	var (
		name string
		team string
	)

	cmd := &cobra.Command{
		Use:   "wake",
		Short: "Wake a sleeping Worker",
		Long: `Start a stopped/sleeping Worker container.

  hiclaw worker wake --name alice
  hiclaw worker wake --name alpha-dev --team alpha-team`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			_ = team
			client := NewAPIClient()
			var resp lifecycleResp
			if err := client.DoJSON("POST", "/api/v1/workers/"+name+"/wake", nil, &resp); err != nil {
				return fmt.Errorf("wake worker: %w", err)
			}
			fmt.Printf("worker/%s phase=%s\n", resp.Name, resp.Phase)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (required)")
	cmd.Flags().StringVar(&team, "team", "", "Team name context (optional)")
	return cmd
}

// ---------------------------------------------------------------------------
// worker sleep
// ---------------------------------------------------------------------------

func workerSleepCmd() *cobra.Command {
	var (
		name string
		team string
	)

	cmd := &cobra.Command{
		Use:   "sleep",
		Short: "Put a Worker to sleep",
		Long: `Stop a running Worker container (preserves state).

  hiclaw worker sleep --name alice
  hiclaw worker sleep --name alpha-dev --team alpha-team`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			_ = team
			client := NewAPIClient()
			var resp lifecycleResp
			if err := client.DoJSON("POST", "/api/v1/workers/"+name+"/sleep", nil, &resp); err != nil {
				return fmt.Errorf("sleep worker: %w", err)
			}
			fmt.Printf("worker/%s phase=%s\n", resp.Name, resp.Phase)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (required)")
	cmd.Flags().StringVar(&team, "team", "", "Team name context (optional)")
	return cmd
}

// ---------------------------------------------------------------------------
// worker ensure-ready
// ---------------------------------------------------------------------------

func workerEnsureReadyCmd() *cobra.Command {
	var (
		name string
		team string
	)

	cmd := &cobra.Command{
		Use:   "ensure-ready",
		Short: "Ensure a Worker is running and ready",
		Long: `Start the Worker if sleeping, then report current phase.

  hiclaw worker ensure-ready --name alice
  hiclaw worker ensure-ready --name alpha-dev --team alpha-team`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			_ = team
			client := NewAPIClient()
			var resp lifecycleResp
			if err := client.DoJSON("POST", "/api/v1/workers/"+name+"/ensure-ready", nil, &resp); err != nil {
				return fmt.Errorf("ensure-ready: %w", err)
			}
			fmt.Printf("worker/%s phase=%s\n", resp.Name, resp.Phase)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (required)")
	cmd.Flags().StringVar(&team, "team", "", "Team name context (optional)")
	return cmd
}

// ---------------------------------------------------------------------------
// worker status
// ---------------------------------------------------------------------------

func workerStatusCmd() *cobra.Command {
	var (
		name   string
		team   string
		output string
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show Worker runtime status",
		Long: `Show runtime status for a single Worker or all Workers in a team.

  hiclaw worker status --name alice
  hiclaw worker status --team alpha-team`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" && team == "" {
				return fmt.Errorf("--name or --team is required")
			}

			client := NewAPIClient()

			if name != "" {
				var resp workerResp
				if err := client.DoJSON("GET", "/api/v1/workers/"+name+"/status", nil, &resp); err != nil {
					return fmt.Errorf("worker status: %w", err)
				}
				if output == "json" {
					printJSON(resp)
					return nil
				}
				printDetail(workerDetail(resp))
				return nil
			}

			// --team: list all workers in team, show runtime summary table
			var resp workerListResp
			if err := client.DoJSON("GET", "/api/v1/workers?team="+team, nil, &resp); err != nil {
				return fmt.Errorf("list team workers: %w", err)
			}
			if output == "json" {
				printJSON(resp)
				return nil
			}
			if resp.Total == 0 {
				fmt.Printf("No workers found in team %s.\n", team)
				return nil
			}
			headers := []string{"NAME", "PHASE", "STATE", "MODEL", "RUNTIME"}
			var rows [][]string
			for _, w := range resp.Workers {
				var detail workerResp
				if err := client.DoJSON("GET", "/api/v1/workers/"+w.Name+"/status", nil, &detail); err != nil {
					return fmt.Errorf("worker %s status: %w", w.Name, err)
				}
				rows = append(rows, []string{
					detail.Name,
					or(detail.Phase, "Pending"),
					or(detail.ContainerState, "unknown"),
					detail.Model,
					or(detail.Runtime, "openclaw"),
				})
			}
			printTable(headers, rows)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name")
	cmd.Flags().StringVar(&team, "team", "", "Team name (show all workers in team)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	return cmd
}

// ---------------------------------------------------------------------------
// worker report-ready
// ---------------------------------------------------------------------------

func workerReportReadyCmd() *cobra.Command {
	var (
		name         string
		lastActiveAt string
	)

	cmd := &cobra.Command{
		Use:   "report-ready",
		Short: "Report worker readiness to controller",
		Long: `Report this worker as ready to the controller (one-shot).

  # One-shot ready report
  hiclaw worker report-ready

  # Ready report with last-active timestamp
  hiclaw worker report-ready --last-active-at 2026-05-13T00:00:00Z

Worker name is read from --name, AGENTTEAMS_WORKER_CR_NAME, or AGENTTEAMS_WORKER_NAME env var.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				name = os.Getenv("AGENTTEAMS_WORKER_CR_NAME")
			}
			if name == "" {
				name = os.Getenv("AGENTTEAMS_WORKER_NAME")
			}
			if name == "" {
				return fmt.Errorf("--name, AGENTTEAMS_WORKER_CR_NAME, or AGENTTEAMS_WORKER_NAME is required")
			}

			client := NewAPIClient()
			path := "/api/v1/workers/" + name + "/ready"

			var body interface{}
			if lastActiveAt != "" {
				body = struct {
					LastActiveAt string `json:"lastActiveAt,omitempty"`
				}{LastActiveAt: lastActiveAt}
			}

			var lastErr error
			for attempt := 1; attempt <= 5; attempt++ {
				if err := client.DoJSON("POST", path, body, nil); err != nil {
					lastErr = err
					fmt.Fprintf(os.Stderr, "report-ready attempt %d/5 failed: %v\n", attempt, err)
					time.Sleep(time.Duration(attempt) * 2 * time.Second)
					client = NewAPIClient()
					continue
				}
				fmt.Fprintf(os.Stderr, "worker/%s reported ready\n", name)
				return nil
			}
			return fmt.Errorf("report-ready failed after 5 attempts: %w", lastErr)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (default: AGENTTEAMS_WORKER_CR_NAME or AGENTTEAMS_WORKER_NAME env)")
	cmd.Flags().StringVar(&lastActiveAt, "last-active-at", "", "Last business activity timestamp (RFC3339)")
	return cmd
}

// ---------------------------------------------------------------------------
// worker heartbeat
// ---------------------------------------------------------------------------

func workerHeartbeatCmd() *cobra.Command {
	var (
		name         string
		lastActiveAt string
	)

	cmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "Send a heartbeat to the controller",
		Long: `Report a periodic heartbeat to the controller (one-shot).

  # Simple heartbeat
  hiclaw worker heartbeat

  # Heartbeat with last-active timestamp
  hiclaw worker heartbeat --last-active-at 2026-05-13T00:00:00Z

Worker name is read from --name, AGENTTEAMS_WORKER_CR_NAME, or AGENTTEAMS_WORKER_NAME env var.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				name = os.Getenv("AGENTTEAMS_WORKER_CR_NAME")
			}
			if name == "" {
				name = os.Getenv("AGENTTEAMS_WORKER_NAME")
			}
			if name == "" {
				return fmt.Errorf("--name, AGENTTEAMS_WORKER_CR_NAME, or AGENTTEAMS_WORKER_NAME is required")
			}

			client := NewAPIClient()
			path := "/api/v1/workers/" + name + "/heartbeat"

			var body interface{}
			if lastActiveAt != "" {
				body = struct {
					LastActiveAt string `json:"lastActiveAt,omitempty"`
				}{LastActiveAt: lastActiveAt}
			}

			var lastErr error
			for attempt := 1; attempt <= 5; attempt++ {
				if err := client.DoJSON("POST", path, body, nil); err != nil {
					lastErr = err
					fmt.Fprintf(os.Stderr, "heartbeat attempt %d/5 failed: %v\n", attempt, err)
					time.Sleep(time.Duration(attempt) * 2 * time.Second)
					client = NewAPIClient()
					continue
				}
				fmt.Fprintf(os.Stderr, "worker/%s heartbeat sent\n", name)
				return nil
			}
			return fmt.Errorf("heartbeat failed after 5 attempts: %w", lastErr)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (default: AGENTTEAMS_WORKER_CR_NAME or AGENTTEAMS_WORKER_NAME env)")
	cmd.Flags().StringVar(&lastActiveAt, "last-active-at", "", "Last business activity timestamp (RFC3339)")
	return cmd
}

// ---------------------------------------------------------------------------
// Response type
// ---------------------------------------------------------------------------

type lifecycleResp struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}
