package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

func taskPlanCmd() *cobra.Command {
	var filename, inline, team string
	cmd := &cobra.Command{Use: "plan <main-task-id>", Short: "Save your execution DAG and let the task system prepare rooms and dispatch ready nodes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !taskIdentifier.MatchString(args[0]) || (filename == "") == (inline == "") {
			return fmt.Errorf("valid main task id and exactly one of --file or --json required")
		}
		body := []byte(inline)
		if filename != "" {
			file, err := os.Open(filename)
			if err != nil {
				return err
			}
			defer file.Close()
			body, err = io.ReadAll(io.LimitReader(file, (1<<20)+1))
			if err != nil {
				return err
			}
		}
		if len(body) > 1<<20 || !json.Valid(body) {
			return fmt.Errorf("plan must be valid JSON no larger than 1 MiB")
		}
		query := url.Values{}
		if team != "" {
			query.Set("team", team)
		}
		response, err := NewAPIClient().doWithHeadersContext(cmd.Context(), http.MethodPost, "/api/v1/tasks/"+url.PathEscape(args[0])+"/plan?"+query.Encode(), json.RawMessage(body), nil)
		if err != nil {
			return err
		}
		var result json.RawMessage
		if err := decodeJSONResponse(response, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&filename, "file", "", "Local JSON plan file with nodes, local IDs and dependencies")
	cmd.Flags().StringVar(&inline, "json", "", "Inline JSON plan using the same nodes schema")
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	return cmd
}
