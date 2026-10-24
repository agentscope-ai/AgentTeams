package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

// projectCmd groups the W-PR-2 human-intervention and lifecycle commands.
// They wrap the Controller write endpoints so an L2 human (or admin/team
// leader) can pause / resume / replan / create / cancel / complete projects
// without raw curl. The CLI forwards whatever bearer token is configured
// (AGENTTEAMS_AUTH_TOKEN or AGENTTEAMS_AUTH_TOKEN_FILE), so a Matrix access
// token works for L2 humans just like `agt get projects`.
func projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Intervene in or manage a project (pause/resume/replan/create/cancel/complete)",
	}
	cmd.AddCommand(projectPauseCmd())
	cmd.AddCommand(projectResumeCmd())
	cmd.AddCommand(projectReplanCmd())
	cmd.AddCommand(projectCreateCmd())
	cmd.AddCommand(projectCancelCmd())
	cmd.AddCommand(projectCompleteCmd())
	return cmd
}

func projectPauseCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "pause <project-id>",
		Short: "Pause a project (stops new task dispatch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if reason != "" {
				body["reason"] = reason
			}
			return projectWrite("POST", "/api/v1/projects/"+args[0]+"/pause", body)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "optional pause reason (recorded in pause_reason)")
	return cmd
}

func projectResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <project-id>",
		Short: "Resume a paused project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return projectWrite("POST", "/api/v1/projects/"+args[0]+"/resume", nil)
		},
	}
}

func projectReplanCmd() *cobra.Command {
	var tasksFile string
	cmd := &cobra.Command{
		Use:   "replan <project-id>",
		Short: "Replace a project's DAG plan (validated: duplicate ids / unknown deps / cycles rejected)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var tasks []map[string]any
			if tasksFile != "" {
				data, err := os.ReadFile(tasksFile)
				if err != nil {
					return fmt.Errorf("read tasks file: %w", err)
				}
				if err := json.Unmarshal(data, &tasks); err != nil {
					return fmt.Errorf("parse tasks file (expected JSON array): %w", err)
				}
			}
			return projectWrite("POST", "/api/v1/projects/"+args[0]+"/replan", map[string]any{"tasks": tasks})
		},
	}
	cmd.Flags().StringVar(&tasksFile, "tasks", "", "JSON array file of tasks: [{\"taskId\",\"title\",\"assignedTo\",\"dependsOn\",\"status\"}]")
	return cmd
}

func projectCreateCmd() *cobra.Command {
	var (
		title        string
		source       string
		requester    string
		teamID       string
		projectID    string
		sourceRoomID string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			if title == "" {
				return fmt.Errorf("--title is required")
			}
			body := map[string]any{
				"title": title,
			}
			if source != "" {
				body["source"] = source
			}
			if requester != "" {
				body["requester"] = requester
			}
			if teamID != "" {
				body["team_id"] = teamID
			}
			if projectID != "" {
				body["project_id"] = projectID
			}
			if sourceRoomID != "" {
				body["source_room_id"] = sourceRoomID
			}
			return projectWrite("POST", "/api/v1/projects", body)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "project title (required)")
	cmd.Flags().StringVar(&source, "source", "", "source label (matrix/dingtalk/wechat/...)")
	cmd.Flags().StringVar(&requester, "requester", "", "human-readable requester id")
	cmd.Flags().StringVar(&teamID, "team", "", "owning team id (required for L2 humans / team leaders)")
	cmd.Flags().StringVar(&projectID, "id", "", "optional custom project id (plain token)")
	cmd.Flags().StringVar(&sourceRoomID, "room", "", "source Matrix room id for intervention notifications")
	return cmd
}

func projectCancelCmd() *cobra.Command {
	var reason string
	var replacement string
	var submissionID string
	var team string
	cmd := &cobra.Command{
		Use:   "cancel <project-id> <task-id>",
		Short: "Cancel a single task (reason required)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			body := map[string]any{"reason": reason}
			if replacement != "" {
				body["replacementTaskId"] = replacement
			}
			if submissionID != "" {
				body["submissionId"] = submissionID
			}
			path := "/api/v1/projects/" + args[0] + "/tasks/" + args[1] + "/cancel"
			if team != "" {
				query := url.Values{}
				query.Set("team", team)
				path += "?" + query.Encode()
			}
			return projectWrite("POST", path, body)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "cancellation reason (required)")
	cmd.Flags().StringVar(&replacement, "replacement", "", "optional replacement task id")
	cmd.Flags().StringVar(&submissionID, "submission-id", "", "current task submission identity (required when TaskMeta has submission_id)")
	cmd.Flags().StringVar(&team, "team", "", "owning team for an ambiguous project id")
	return cmd
}

func projectCompleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "complete <project-id>",
		Short: "Accept/complete a project (all tasks must be terminal)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return projectWrite("POST", "/api/v1/projects/"+args[0]+"/complete", nil)
		},
	}
}

// projectWrite performs a write request and prints the JSON response (or a
// concise one-line confirmation for the create endpoint).
func projectWrite(method, path string, body map[string]any) error {
	client := NewAPIClient()
	var resp map[string]any
	if err := client.DoJSON(method, path, body, &resp); err != nil {
		return err
	}
	if resp == nil {
		fmt.Println("ok")
		return nil
	}
	// Create returns a small {"project_id", ...} summary; write endpoints
	// return the full workflow. Either way, pretty-print the JSON.
	out, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
