package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "hiclaw",
		Short: "AgentTeams resource management CLI",
		Long: `AgentTeams CLI — manages Workers, Teams, Humans, and Managers via the
agentteams-controller REST API.

Environment variables:
  AGENTTEAMS_CONTROLLER_URL / AGENTTEAMS_CONTROLLER_URL
      Controller base URL (default: http://localhost:8090)
  AGENTTEAMS_AUTH_TOKEN / AGENTTEAMS_AUTH_TOKEN
      Bearer token for authentication
  AGENTTEAMS_AUTH_TOKEN_FILE / AGENTTEAMS_AUTH_TOKEN_FILE
      Path to a file containing the bearer token (K8s projected volume)

Legacy AGENTTEAMS_* names are accepted for compatibility with existing installs.`,
	}

	rootCmd.AddCommand(applyCmd())
	rootCmd.AddCommand(createCmd())
	rootCmd.AddCommand(getCmd())
	rootCmd.AddCommand(updateCmd())
	rootCmd.AddCommand(deleteCmd())
	rootCmd.AddCommand(workerCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(llmPreflightCmd())
	rootCmd.AddCommand(rotateCmd())
	rootCmd.AddCommand(managerStateCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
