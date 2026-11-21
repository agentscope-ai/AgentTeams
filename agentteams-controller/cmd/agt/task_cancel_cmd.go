package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func taskCancelCmd() *cobra.Command {
	var reason, team string
	cmd := &cobra.Command{Use: "cancel <main-task-id>", Short: "Cancel a main task and unfinished executions while retaining saved results", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !taskIdentifier.MatchString(args[0]) || strings.TrimSpace(reason) == "" {
			return fmt.Errorf("valid main task id and cancellation reason required")
		}
		query := url.Values{}
		if team != "" {
			query.Set("team", team)
		}
		response, err := NewAPIClient().doWithHeadersContext(cmd.Context(), http.MethodPost, "/api/v1/tasks/"+url.PathEscape(args[0])+"/cancel?"+query.Encode(), map[string]string{"reason": reason}, nil)
		if err != nil {
			return err
		}
		var result json.RawMessage
		if err := decodeJSONResponse(response, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&reason, "reason", "", "Reason for cancelling this request; unchanged retries resume pending synchronization")
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
