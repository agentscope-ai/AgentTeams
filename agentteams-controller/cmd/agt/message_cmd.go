package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// messageCmd gives Harnesses without native cross-room tools the same Matrix
// handoff path, using the current agent's credentials and sender identity.
func messageCmd() *cobra.Command {
	var room, text string
	cmd := &cobra.Command{
		Use: "message", Short: "Send a Matrix message as your current agent",
		RunE: func(cmd *cobra.Command, _ []string) error {
			base := strings.TrimRight(os.Getenv("AGENTTEAMS_MATRIX_URL"), "/")
			token := os.Getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN")
			if os.Getenv("AGENTTEAMS_AGENT_ROLE") == "manager" {
				token = os.Getenv("AGENTTEAMS_MANAGER_MATRIX_TOKEN")
			}
			if base == "" || token == "" {
				return fmt.Errorf("Matrix URL and current agent token are required")
			}
			mentions := regexp.MustCompile(`@[A-Za-z0-9._=+/\-]+:[A-Za-z0-9.\-]+(?::[0-9]+)?`).FindAllString(text, -1)
			body := map[string]interface{}{"msgtype": "m.text", "body": text, "m.mentions": map[string]interface{}{"user_ids": append([]string{}, mentions...)}}
			client := NewAPIClient()
			client.BaseURL, client.Token = base, token
			var result map[string]interface{}
			path := "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/send/m.room.message/agt-" + fmt.Sprint(time.Now().UnixNano())
			if err := client.DoJSON("PUT", path, body, &result); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		},
	}
	cmd.Flags().StringVar(&room, "room", "", "Destination Matrix room ID")
	cmd.Flags().StringVar(&text, "text", "", "Message text, including full Matrix mentions")
	_ = cmd.MarkFlagRequired("room")
	_ = cmd.MarkFlagRequired("text")
	return cmd
}
