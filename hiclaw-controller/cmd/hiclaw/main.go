package main

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func main() {
	invoked := filepath.Base(os.Args[0])
	if invoked == "" || invoked == "." || invoked == "/" {
		invoked = "agt"
	}

	rootCmd := &cobra.Command{
		Use:   invoked,
		Short: "AgentTeams resource management CLI",
		Long: `AgentTeams CLI — manages Workers, Teams, Humans, and Managers via the
controller REST API. Formerly distributed as ` + "`hiclaw`" + `; both binary
names are supported during the rename transition (see #861).

Environment variables (AGENTTEAMS_ takes precedence; HICLAW_ falls back):
  AGENTTEAMS_CONTROLLER_URL / HICLAW_CONTROLLER_URL
        Controller base URL (default: http://localhost:8090)
  AGENTTEAMS_AUTH_TOKEN / HICLAW_AUTH_TOKEN
        Bearer token for authentication
  AGENTTEAMS_AUTH_TOKEN_FILE / HICLAW_AUTH_TOKEN_FILE
        Path to a file containing the bearer token (K8s projected volume)`,
	}

	rootCmd.AddCommand(applyCmd())
	rootCmd.AddCommand(createCmd())
	rootCmd.AddCommand(getCmd())
	rootCmd.AddCommand(updateCmd())
	rootCmd.AddCommand(deleteCmd())
	rootCmd.AddCommand(workerCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(versionCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
