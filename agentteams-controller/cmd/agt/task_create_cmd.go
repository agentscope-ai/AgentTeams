package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func taskCreateCmd() *cobra.Command {
	var title, spec, leader, inputID string
	var relatedInputs []string
	cmd := &cobra.Command{Use: "create", Short: "Register the original request and hand it to a Team Leader", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(title) == "" || leader == "" {
			return fmt.Errorf("title and registered Leader required")
		}
		if inputID != "" && os.Getenv("AGENTTEAMS_INPUT_ID") != inputID {
			return fmt.Errorf("selected input does not match the native source binding")
		}
		source, user, room := os.Getenv("AGENTTEAMS_ORIGIN_EVENT_ID"), os.Getenv("AGENTTEAMS_REQUESTER_USER_ID"), os.Getenv("AGENTTEAMS_REQUESTER_ROOM_ID")
		if source == "" || !strings.HasPrefix(user, "@") || !strings.HasPrefix(room, "!") {
			return fmt.Errorf("original conversation context unavailable; use the native task tool")
		}
		payload := map[string]any{"title": title, "spec": spec, "leader": leader}
		if len(relatedInputs) > 0 {
			var bound map[string]string
			if json.Unmarshal([]byte(os.Getenv("AGENTTEAMS_RELATED_INPUTS")), &bound) != nil {
				return fmt.Errorf("related inputs do not match the native source binding")
			}
			sources := make([]string, 0, len(relatedInputs))
			seen := map[string]bool{}
			for _, ref := range relatedInputs {
				event := bound[ref]
				if !strings.HasPrefix(event, "$") || seen[event] {
					return fmt.Errorf("related inputs do not match the native source binding")
				}
				seen[event] = true
				sources = append(sources, event)
			}
			payload["relatedSources"] = sources
		}
		response, err := NewAPIClient().doWithHeadersContext(cmd.Context(), http.MethodPost, "/api/v1/tasks", payload, http.Header{"X-Agentteams-Input-Frontier": {os.Getenv("AGENTTEAMS_INPUT_FRONTIER")}, "X-Agentteams-Origin-Event-Id": {source}, "X-Agentteams-Requester-User-Id": {user}, "X-Agentteams-Requester-Room-Id": {room}})
		if err != nil {
			return err
		}
		if response.StatusCode == http.StatusConflict {
			raw, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				return readErr
			}
			var stale struct {
				OK     bool   `json:"ok"`
				Code   string `json:"error"`
				NoTask bool   `json:"noTaskRegistered"`
			}
			if json.Unmarshal(raw, &stale) == nil && !stale.OK && stale.Code == "task_input_stale" && stale.NoTask {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(json.RawMessage(raw))
			}
			response.Body = io.NopCloser(bytes.NewReader(raw))
		}
		var result json.RawMessage
		if err := decodeJSONResponse(response, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&title, "title", "", "Original request title")
	cmd.Flags().StringVar(&spec, "spec", "", "Optional supplementary context; the task system preserves the original request")
	cmd.Flags().StringVar(&leader, "leader", "", "Registered Team Leader")
	cmd.Flags().StringVar(&inputID, "input-id", "", "Original input reference supplied and bound by your framework")
	cmd.Flags().StringArrayVar(&relatedInputs, "related-input-id", nil, "Related correction input reference supplied and bound by your framework (repeatable)")
	for _, name := range []string{"title", "leader"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}
