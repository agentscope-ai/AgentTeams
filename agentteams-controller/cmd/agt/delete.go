package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func deleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a resource",
	}
	cmd.AddCommand(deleteWorkerCmd())
	cmd.AddCommand(deleteTeamCmd())
	cmd.AddCommand(deleteHumanCmd())
	cmd.AddCommand(deleteManagerCmd())
	return cmd
}

func deleteWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "worker <name>",
		Short: "Delete a Worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteResource("worker", args[0])
		},
	}
}

func deleteTeamCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "team <name>",
		Short: "Delete a Team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteResource("team", args[0])
		},
	}
}

func deleteHumanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "human <name>",
		Short: "Delete a Human",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteResource("human", args[0])
		},
	}
}

func deleteManagerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "manager <name>",
		Short: "Delete a Manager",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteResource("manager", args[0])
		},
	}
}

func deleteResource(kind, name string) error {
	client := NewAPIClient()
	if err := client.DoJSON("DELETE", fmt.Sprintf("/api/v1/%ss/%s", kind, name), nil, nil); err != nil {
		return fmt.Errorf("delete %s: %w", kind, err)
	}
	fmt.Printf("%s/%s deleted\n", kind, name)
	return nil
}
