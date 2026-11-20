package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"github.com/spf13/cobra"
)

var taskIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func taskCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Operate on tasks through the public task API"}
	var team string
	get := &cobra.Command{
		Use: "get <task-id>", Short: "Read task state and current submission as JSON", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !taskIdentifier.MatchString(args[0]) {
				return fmt.Errorf("invalid task id")
			}
			endpoint := "/api/v1/tasks/" + url.PathEscape(args[0])
			if team != "" {
				endpoint += "?" + url.Values{"team": {team}}.Encode()
			}
			var response json.RawMessage
			if err := NewAPIClient().DoJSON(http.MethodGet, endpoint, nil, &response); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
		},
	}
	get.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	cmd.AddCommand(get)
	cmd.AddCommand(taskBeginCmd())
	cmd.AddCommand(taskFilesCmd())
	return cmd
}
