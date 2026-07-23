package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func updateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update a resource",
	}
	cmd.AddCommand(updateWorkerCmd())
	cmd.AddCommand(updateTeamCmd())
	cmd.AddCommand(updateManagerCmd())
	return cmd
}

// ---------------------------------------------------------------------------
// update worker
// ---------------------------------------------------------------------------

func updateWorkerCmd() *cobra.Command {
	var (
		name       string
		model      string
		runtime    string
		image      string
		identity   string
		soul       string
		skills     string
		packageURI string
		expose     string
		state      string
	)

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Update a Worker",
		Long: `Update an existing Worker resource. Only specified fields are changed.

  agt update worker --name alice --model claude-sonnet-4-6
  agt update worker --name alice --image agentteams/agentteams-worker:v1.2.0
  agt update worker --name alice --skills github-operations,code-review
  To update CPU/memory resources, use a YAML manifest and pass it with 'agt apply -f worker.yaml'.
  To update mcpServers, use a YAML manifest and pass it with 'agt apply -f worker.yaml'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}

			if packageURI != "" {
				var err error
				packageURI, err = expandPackageURI(packageURI)
				if err != nil {
					return err
				}
			}

			req := map[string]interface{}{}
			setIfNotEmpty(req, "model", model)
			setIfNotEmpty(req, "runtime", runtime)
			setIfNotEmpty(req, "image", image)
			setIfNotEmpty(req, "identity", identity)
			setIfNotEmpty(req, "soul", soul)
			setIfNotEmpty(req, "package", packageURI)
			setIfNotEmpty(req, "state", state)
			if cmd.Flags().Changed("skills") {
				req["skills"] = splitCSV(skills)
			}
			if expose != "" {
				req["expose"] = parseExposePorts(expose)
			}

			if len(req) == 0 {
				return fmt.Errorf("at least one field must be specified for update")
			}

			client := NewAPIClient()
			var resp map[string]interface{}
			if err := client.DoJSON("PUT", "/api/v1/workers/"+name, req, &resp); err != nil {
				return fmt.Errorf("update worker: %w", err)
			}
			fmt.Printf("worker/%s configured\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (required)")
	cmd.Flags().StringVar(&model, "model", "", "LLM model ID")
	cmd.Flags().StringVar(&runtime, "runtime", "", "Agent runtime (openclaw|copaw|hermes|openhuman)")
	cmd.Flags().StringVar(&image, "image", "", "Container image override")
	cmd.Flags().StringVar(&identity, "identity", "", "Worker identity description")
	cmd.Flags().StringVar(&soul, "soul", "", "Worker SOUL.md content")
	cmd.Flags().StringVar(&skills, "skills", "", "Comma-separated built-in skills")
	cmd.Flags().StringVar(&packageURI, "package", "", "Package URI")
	cmd.Flags().StringVar(&expose, "expose", "", "Comma-separated ports to expose")
	cmd.Flags().StringVar(&state, "state", "", "Desired lifecycle state (Running|Sleeping|Stopped)")
	return cmd
}

// ---------------------------------------------------------------------------
// update team
// ---------------------------------------------------------------------------

func updateTeamCmd() *cobra.Command {
	var (
		name                 string
		teamName             string
		description          string
		leaderName           string
		leaderHeartbeatEvery string
		workers              string
		peerMentions         bool
	)

	cmd := &cobra.Command{
		Use:   "team",
		Short: "Update a Team",
		Long: `Update an existing Team resource. Only specified fields are changed.

  agt update team --name alpha --description "Updated description"
  agt update team --name alpha --leader-name alpha-lead --workers alice,bob
  agt update team --name alpha --leader-heartbeat-every 30m

Create or update each Worker separately to configure its runtime fields.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}

			req := map[string]interface{}{}
			setIfNotEmpty(req, "teamName", teamName)
			setIfNotEmpty(req, "description", description)
			setIfNotEmpty(req, "heartbeatEvery", leaderHeartbeatEvery)
			if cmd.Flags().Changed("leader-name") || cmd.Flags().Changed("workers") {
				if leaderName == "" {
					return fmt.Errorf("--leader-name is required when updating Team membership")
				}
				workerMembers := []interface{}{
					map[string]interface{}{"name": leaderName, "role": "team_leader"},
				}
				for _, worker := range splitCSV(workers) {
					workerMembers = append(workerMembers, map[string]interface{}{"name": worker, "role": "worker"})
				}
				req["workerMembers"] = workerMembers
			}
			if cmd.Flags().Changed("peer-mentions") {
				req["peerMentions"] = peerMentions
			}

			if len(req) == 0 {
				return fmt.Errorf("at least one field must be specified for update")
			}

			client := NewAPIClient()
			var resp map[string]interface{}
			if err := client.DoJSON("PUT", "/api/v1/teams/"+name, req, &resp); err != nil {
				return fmt.Errorf("update team: %w", err)
			}
			fmt.Printf("team/%s configured\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Team name (required)")
	cmd.Flags().StringVar(&teamName, "team-name", "", "Runtime/storage team name")
	cmd.Flags().StringVar(&description, "description", "", "Team description")
	cmd.Flags().StringVar(&leaderName, "leader-name", "", "Existing Worker resource used as Team Leader")
	cmd.Flags().StringVar(&leaderHeartbeatEvery, "leader-heartbeat-every", "", "Leader heartbeat interval (e.g. 30m)")
	cmd.Flags().StringVar(&workers, "workers", "", "Comma-separated existing Worker resource names")
	cmd.Flags().BoolVar(&peerMentions, "peer-mentions", true, "Allow Team Workers to mention peers")
	return cmd
}

// ---------------------------------------------------------------------------
// update manager
// ---------------------------------------------------------------------------

func updateManagerCmd() *cobra.Command {
	var (
		name    string
		model   string
		runtime string
		image   string
		soul    string
	)

	cmd := &cobra.Command{
		Use:   "manager",
		Short: "Update a Manager",
		Long: `Update an existing Manager resource. Only specified fields are changed.

  agt update manager --name default --model claude-sonnet-4-6
  agt update manager --name default --image agentteams/agentteams-manager:v1.2.0
  To update CPU/memory resources, use a YAML manifest and pass it with 'agt apply -f manager.yaml'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}

			req := map[string]interface{}{}
			setIfNotEmpty(req, "model", model)
			setIfNotEmpty(req, "runtime", runtime)
			setIfNotEmpty(req, "image", image)
			setIfNotEmpty(req, "soul", soul)

			if len(req) == 0 {
				return fmt.Errorf("at least one field must be specified for update")
			}

			client := NewAPIClient()
			var resp map[string]interface{}
			if err := client.DoJSON("PUT", "/api/v1/managers/"+name, req, &resp); err != nil {
				return fmt.Errorf("update manager: %w", err)
			}
			fmt.Printf("manager/%s configured\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Manager name (required)")
	cmd.Flags().StringVar(&model, "model", "", "LLM model ID")
	cmd.Flags().StringVar(&runtime, "runtime", "", "Agent runtime (openclaw|copaw|hermes|openhuman)")
	cmd.Flags().StringVar(&image, "image", "", "Container image override")
	cmd.Flags().StringVar(&soul, "soul", "", "Manager SOUL.md content")
	return cmd
}
