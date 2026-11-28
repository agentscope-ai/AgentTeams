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
	var artifacts []string
	var noArtifacts bool
	cmd := &cobra.Command{Use: "complete <task-id>", Short: "Complete an accepted main task and return its final report to the original conversation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !taskIdentifier.MatchString(args[0]) || strings.TrimSpace(report) == "" {
			return fmt.Errorf("valid main task id and verified final report required")
		}
		query := url.Values{}
		if team != "" {
			query.Set("team", team)
		}
		body := map[string]any{"report": report}
		if noArtifacts {
			body["artifacts"] = []string{}
		} else if len(artifacts) > 0 {
			body["artifacts"] = artifacts
		}
		response, err := NewAPIClient().doWithHeadersContext(cmd.Context(), http.MethodPost, "/api/v1/tasks/"+url.PathEscape(args[0])+"/complete?"+query.Encode(), body, nil)
		if err != nil {
			return err
		}
		var result json.RawMessage
		if err := decodeJSONResponse(response, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringArrayVar(&artifacts, "artifact", nil, "Accepted artifact ID to deliver; repeat to select final outputs (default: all accepted outputs)")
	cmd.Flags().BoolVar(&noArtifacts, "no-artifacts", false, "Return the final report without attaching accepted files")
	cmd.MarkFlagsMutuallyExclusive("artifact", "no-artifacts")
	cmd.Flags().StringVar(&report, "report", "", "Verified final user report")
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	_ = cmd.MarkFlagRequired("report")
	return cmd
}
