package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

func skillCmd() *cobra.Command {
	root := &cobra.Command{Use: "skill", Short: "Publish Skills to the asset center"}
	var file string
	publish := &cobra.Command{
		Use:   "publish -f SKILL.zip",
		Short: "Publish a Skill ZIP to the deployment catalog through Controller",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			api := NewAPIClient()
			api.HTTPClient.Timeout = 90 * time.Second
			var result any
			if err := api.DoMultipart("/api/v1/skills", "file", filepath.Base(file), data,
				map[string]string{"scope": "deployment"}, &result); err != nil {
				return err
			}
			output, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(output))
			return nil
		},
	}
	publish.Flags().StringVarP(&file, "file", "f", "", "Skill ZIP containing one named directory with SKILL.md")
	_ = publish.MarkFlagRequired("file")
	root.AddCommand(publish)
	return root
}
