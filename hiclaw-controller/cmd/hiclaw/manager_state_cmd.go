package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hiclaw/hiclaw-controller/internal/managerstate"
)

func managerStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manager-state",
		Short: "Atomic OpenClaw Manager state.json operations",
		Long: `Manage the OpenClaw Manager task board at ~/state.json.

This command mirrors manager/agent/skills/task-management/scripts/manage-state.sh
so Manager skills can keep calling the shell wrapper while the implementation
moves into the hiclaw CLI.

  hiclaw manager-state --action init
  hiclaw manager-state --action add-finite --task-id T --title TITLE --assigned-to W --room-id R
  hiclaw manager-state --action list`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed, err := managerstate.ParseArgs(args)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				return err
			}
			out, err := managerstate.Run(&managerstate.Store{}, parsed)
			if err != nil {
				if strings.HasPrefix(err.Error(), "ERROR:") || strings.HasPrefix(err.Error(), "usage:") {
					fmt.Fprintln(os.Stderr, err.Error())
					return err
				}
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
	return cmd
}
