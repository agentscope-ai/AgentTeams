package main

import (
	"github.com/spf13/cobra"
	"net/url"
)

func taskRoomCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "task-room", Short: "List, inspect, archive, restore or delete task rooms"}
	for _, action := range []string{"archive", "unarchive", "delete", "status"} {
		cmd.AddCommand(&cobra.Command{Use: action + " <room-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			method, operation := "POST", action
			if action == "status" {
				method, operation = "GET", "archive"
			}
			return projectWrite(method, "/api/v1/task-rooms/"+url.PathEscape(args[0])+"/"+operation, nil)
		}})
	}
	var team, admin, human string
	list := &cobra.Command{Use: "list", Short: "List accessible archived task rooms", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		query := url.Values{}
		for key, value := range map[string]string{"team": team, "team_admin": admin, "human_member": human} {
			if value != "" {
				query.Set(key, value)
			}
		}
		return projectWrite("GET", "/api/v1/task-rooms/archived?"+query.Encode(), nil)
	}}
	list.Flags().StringVar(&team, "team", "", "owning team")
	list.Flags().StringVar(&admin, "team-admin", "", "Team admin Matrix user ID")
	list.Flags().StringVar(&human, "human-member", "", "human member Matrix user ID")
	cmd.AddCommand(list)
	return cmd
}
