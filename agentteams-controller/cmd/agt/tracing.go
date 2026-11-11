package main

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

func getTasksCmd() *cobra.Command {
	var team string
	var output string
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "List authorized team tasks, owners, status and dependencies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "json" {
				return fmt.Errorf("task inspection requires -o json")
			}
			query := url.Values{}
			if team != "" {
				query.Set("team", team)
			}
			data, err := NewAPIClient().DoRaw("GET", "/api/v1/tasks?"+query.Encode())
			if err != nil {
				return err
			}
			printRawJSON(data)
			return nil
		},
	}
	cmd.Flags().StringVar(&team, "team", "", "Filter by team")
	cmd.Flags().StringVarP(&output, "output", "o", "json", "Output format (json)")
	return cmd
}

func tracingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tracing", Short: "Read task traces for evidence-based diagnosis"}
	var team string
	var related bool
	task := &cobra.Command{
		Use:   "task <project-id> <task-id>",
		Short: "Read all task turns and observations from Langfuse",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := url.Values{}
			if team != "" {
				query.Set("team", team)
			}
			if related {
				query.Set("includeRelated", "true")
			}
			path := "/api/v1/projects/" + url.PathEscape(args[0]) + "/tasks/" + url.PathEscape(args[1]) + "/traces?" + query.Encode()
			data, err := NewAPIClient().DoRaw("GET", path)
			if err != nil {
				return err
			}
			printRawJSON(data)
			return nil
		},
	}
	task.Flags().StringVar(&team, "team", "", "Owning team (required when project IDs are ambiguous)")
	task.Flags().BoolVar(&related, "related", false, "Include dependency ancestors/descendants and actual causal trace links")
	cmd.AddCommand(task)
	return cmd
}
