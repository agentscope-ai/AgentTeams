package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage Team CR auto-migration",
	}
	cmd.AddCommand(migrateTeamCmd())
	return cmd
}

func migrateTeamCmd() *cobra.Command {
	var action string

	cmd := &cobra.Command{
		Use:   "team <name>",
		Short: "Control migration for a specific Team",
		Long: `Control the auto-migration lifecycle for a Team CR.

Actions:
  status   Show current migration phase (default)
  start    Enable migration (remove disabled annotation)
  disable  Opt-out this Team from auto-migration
  enable   Re-enable auto-migration for this Team`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			client := NewAPIClient()

			switch action {
			case "status", "":
				return migrateTeamStatus(client, name)
			case "start", "enable":
				return migrateTeamSetAnnotation(client, name, "enabled")
			case "disable":
				return migrateTeamSetAnnotation(client, name, "disabled")
			default:
				return fmt.Errorf("unknown action %q (use: status, start, disable, enable)", action)
			}
		},
	}
	cmd.Flags().StringVar(&action, "action", "status", "Migration action: status|start|disable|enable")
	return cmd
}

func migrateTeamStatus(c *APIClient, name string) error {
	resp, err := c.Do("GET", "/api/v1/teams/"+name, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var team struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &team); err != nil {
		return fmt.Errorf("decode team: %w", err)
	}

	migPhase := team.Metadata.Annotations["hiclaw.io/migration-phase"]
	autoMigrate := team.Metadata.Annotations["hiclaw.io/auto-migrate"]
	migratedAt := team.Metadata.Annotations["hiclaw.io/migrated-at"]

	if migPhase == "" {
		migPhase = "(not started)"
	}
	if autoMigrate == "" {
		autoMigrate = "enabled (default)"
	}

	fmt.Printf("Team:            %s\n", name)
	fmt.Printf("Team Phase:      %s\n", team.Status.Phase)
	fmt.Printf("Auto-Migrate:    %s\n", autoMigrate)
	fmt.Printf("Migration Phase: %s\n", migPhase)
	if migratedAt != "" {
		fmt.Printf("Migrated At:     %s\n", migratedAt)
	}
	return nil
}

func migrateTeamSetAnnotation(c *APIClient, name, value string) error {
	// Use PATCH to update the Team's annotations via the REST API
	patchBody := map[string]interface{}{
		"annotations": map[string]string{
			"hiclaw.io/auto-migrate": value,
		},
	}

	resp, err := c.Do("PATCH", "/api/v1/teams/"+name, patchBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	fmt.Printf("Team %q: auto-migrate set to %q\n", name, value)
	return nil
}
