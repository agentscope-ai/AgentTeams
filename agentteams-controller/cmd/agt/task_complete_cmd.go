package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func taskCompleteCmd() *cobra.Command {
	var report, team string
	cmd := &cobra.Command{Use: "complete <task-id>", Short: "Complete an accepted main task and return its final report to the original conversation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !taskIdentifier.MatchString(args[0]) || strings.TrimSpace(report) == "" {
			return fmt.Errorf("valid main task id and verified final report required")
		}
		query := url.Values{}
		if team != "" {
			query.Set("team", team)
		}
		response, err := NewAPIClient().doWithHeadersContext(cmd.Context(), http.MethodPost, "/api/v1/tasks/"+url.PathEscape(args[0])+"/complete?"+query.Encode(), map[string]string{"report": report}, nil)
		if err != nil {
			return err
		}
		var result json.RawMessage
		if err := decodeJSONResponse(response, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&report, "report", "", "Verified final user report")
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	_ = cmd.MarkFlagRequired("report")
	return cmd
}
