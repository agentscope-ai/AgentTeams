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
  HICLAW_CONTROLLER_URL   Controller base URL (default: http://localhost:8090)
  HICLAW_AUTH_TOKEN        Bearer token for authentication
  HICLAW_AUTH_TOKEN_FILE   Path to a file containing the bearer token (K8s projected volume)`,
	}

	rootCmd.AddCommand(applyCmd())
	rootCmd.AddCommand(createCmd())
	rootCmd.AddCommand(getCmd())
	rootCmd.AddCommand(updateCmd())
	rootCmd.AddCommand(deleteCmd())
	rootCmd.AddCommand(workerCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(rotateCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
