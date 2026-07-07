package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "hiclaw",
		Short: "HiClaw resource management CLI",
		Long: `HiClaw CLI — manages Workers, Teams, Humans, and Managers via the
hiclaw-controller REST API.

Environment variables:
  AGENTTEAMS_CONTROLLER_URL / HICLAW_CONTROLLER_URL
      Controller base URL (default: http://localhost:8090)
  AGENTTEAMS_AUTH_TOKEN / HICLAW_AUTH_TOKEN
      Bearer token for authentication
  AGENTTEAMS_AUTH_TOKEN_FILE / HICLAW_AUTH_TOKEN_FILE
      Path to a file containing the bearer token (K8s projected volume)

Legacy HICLAW_* names are accepted for compatibility with existing installs.`,
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

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
