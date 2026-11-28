package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func taskAmendCmd() *cobra.Command {
	var spec, note, team, replacementNodes string
	cmd := &cobra.Command{Use: "amend <task-id>", Short: "Update requirements of an unfinished execution", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !taskIdentifier.MatchString(args[0]) || strings.TrimSpace(spec) == "" || strings.TrimSpace(note) == "" {
			return fmt.Errorf("valid main task id and requirements and change note required")
		}
		query := url.Values{}
		if team != "" {
			query.Set("team", team)
		}
		body := map[string]any{"spec": spec, "note": note}
		if replacementNodes != "" {
			var nodes []map[string]any
			if json.Unmarshal([]byte(replacementNodes), &nodes) != nil || len(nodes) == 0 {
				return fmt.Errorf("replacement nodes must be a nonempty JSON array")
			}
			body["replacementNodes"] = nodes
		}
		response, err := NewAPIClient().doWithHeadersContext(cmd.Context(), http.MethodPost, "/api/v1/tasks/"+url.PathEscape(args[0])+"/amend?"+query.Encode(), body, nil)
		if err != nil {
			return err
		}
		var result json.RawMessage
		if err := decodeJSONResponse(response, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&spec, "spec", "", "Updated execution requirements")
	cmd.Flags().StringVar(&note, "note", "", "Reason for the change")
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	cmd.Flags().StringVar(&replacementNodes, "replacement-nodes", "", "Optional replacement DAG to reassign unfinished work, preserving history")
	_ = cmd.MarkFlagRequired("spec")
	return cmd
}
