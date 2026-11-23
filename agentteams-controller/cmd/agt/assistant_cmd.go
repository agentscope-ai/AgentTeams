package main

import (
	"encoding/json"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

func assistantCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "assistant", Short: "Read role context and send a receipted assistant reminder"}
	var team, manager, user string
	cmd.PersistentFlags().StringVar(&team, "team", "", "Team CR name for a Leader; omit for Manager")
	cmd.PersistentFlags().StringVar(&manager, "manager", "", "Manager CR name")
	cmd.PersistentFlags().StringVar(&user, "user", "", "Registered Human name for a Manager conversation")
	endpoint := func(resource string) string {
		return "/api/v1/assistant/" + resource + "?" + url.Values{"team": {team}, "manager": {manager}, "user": {user}}.Encode()
	}
	cmd.AddCommand(&cobra.Command{Use: "context", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return projectWrite("GET", endpoint("context"), nil)
	}})
	cmd.AddCommand(&cobra.Command{Use: "business", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return projectWrite("GET", endpoint("business"), nil)
	}})
	var before string
	conversation := &cobra.Command{Use: "conversation", Short: "Read a page of this user's shared-protocol conversation", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return projectWrite("GET", endpoint("conversation")+"&before="+url.QueryEscape(before), nil)
	}}
	conversation.Flags().StringVar(&before, "before", "", "Pagination cursor returned as end by the previous page")
	cmd.AddCommand(conversation)
	cmd.AddCommand(&cobra.Command{Use: "receipt <request-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return projectWrite("GET", endpoint("messages/"+url.PathEscape(args[0])), nil)
	}})
	for _, action := range []string{"send", "remember", "remember-business"} {
		var file string
		sub := &cobra.Command{Use: action, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			var body map[string]any
			if err := json.Unmarshal(b, &body); err != nil {
				return err
			}
			method, resource := "POST", "messages"
			if action == "remember" {
				method, resource = "PUT", "context"
			}
			if action == "remember-business" {
				method, resource = "PUT", "business"
			}
			return projectWrite(method, endpoint(resource), body)
		}}
		sub.Flags().StringVar(&file, "file", "", "JSON request file; preserves content without shell escaping")
		_ = sub.MarkFlagRequired("file")
		cmd.AddCommand(sub)
	}
	return cmd
}
