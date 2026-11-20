package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func taskCreateCmd() *cobra.Command {
	var title, spec, leader string
	cmd := &cobra.Command{Use: "create", Short: "Register the original request and hand it to a Team Leader", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(title) == "" || leader == "" {
			return fmt.Errorf("title and registered Leader required")
		}
		source, user, room := os.Getenv("AGENTTEAMS_ORIGIN_EVENT_ID"), os.Getenv("AGENTTEAMS_REQUESTER_USER_ID"), os.Getenv("AGENTTEAMS_REQUESTER_ROOM_ID")
		if source == "" || !strings.HasPrefix(user, "@") || !strings.HasPrefix(room, "!") {
			return fmt.Errorf("original conversation context unavailable; use the native task tool")
		}
		response, err := NewAPIClient().doWithHeadersContext(cmd.Context(), http.MethodPost, "/api/v1/tasks", map[string]string{"title": title, "spec": spec, "leader": leader}, http.Header{"X-Agentteams-Origin-Event-Id": {source}, "X-Agentteams-Requester-User-Id": {user}, "X-Agentteams-Requester-Room-Id": {room}})
		if err != nil {
			return err
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
	for _, name := range []string{"title", "leader"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}
