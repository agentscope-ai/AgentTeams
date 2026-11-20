package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func taskReviewCmd(accepted bool) *cobra.Command {
	action := "reject"
	if accepted {
		action = "accept"
	}
	var submission, note, team string
	cmd := &cobra.Command{Use: action + " <task-id>", Short: "Record your business review of an exact submitted candidate", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !taskIdentifier.MatchString(args[0]) || strings.TrimSpace(submission) == "" || strings.TrimSpace(note) == "" {
			return fmt.Errorf("valid task id, exact submission identity and business review note required")
		}
		query := url.Values{}
		if team != "" {
			query.Set("team", team)
		}
		body := map[string]string{"submissionId": submission, "note": note}
		response, err := NewAPIClient().doWithHeadersContext(cmd.Context(), http.MethodPost,
			"/api/v1/tasks/"+url.PathEscape(args[0])+"/"+action+"?"+query.Encode(), body, nil)
		if err != nil {
			return err
		}
		var result json.RawMessage
		if err := decodeJSONResponse(response, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&submission, "submission", "", "Exact candidate submission ID from the task-system notification")
	cmd.Flags().StringVar(&note, "note", "", "Your business validation evidence or rejection reason")
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	_ = cmd.MarkFlagRequired("submission")
	_ = cmd.MarkFlagRequired("note")
	return cmd
}
