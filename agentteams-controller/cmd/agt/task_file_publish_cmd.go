package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/spf13/cobra"
)

func taskFilePutCmd() *cobra.Command {
	var source, name, team, kind string
	cmd := &cobra.Command{
		Use: "put <task-id>", Short: "Publish a task file and return its immutable artifact reference", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !taskIdentifier.MatchString(args[0]) || name == "" || name == "." || name == ".." || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
				return fmt.Errorf("invalid task or task-local file name")
			}
			if kind != "intermediate" && kind != "output" {
				return fmt.Errorf("kind must be intermediate or output")
			}
			process := os.Getenv("AGENTTEAMS_PROCESS_ID")
			if process == "" {
				return fmt.Errorf("native task execution identity unavailable")
			}
			file, err := os.Open(source)
			if err != nil {
				return err
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return err
			}
			const maximum = 64 << 20
			if !info.Mode().IsRegular() || info.Size() > maximum {
				return fmt.Errorf("source must be a regular file of at most 64 MiB")
			}
			query := url.Values{"name": {name}, "kind": {kind}}
			if team != "" {
				query.Set("team", team)
			}
			response, err := NewAPIClient().doWithHeaders(http.MethodPost,
				"/api/v1/tasks/"+url.PathEscape(args[0])+"/files?"+query.Encode(),
				io.LimitReader(file, maximum+1), http.Header{"Content-Type": {"application/octet-stream"}, "X-Agentteams-Process-Id": {process}})
			if err != nil {
				return err
			}
			var result json.RawMessage
			if err := decodeJSONResponse(response, &result); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "Local file to publish")
	cmd.Flags().StringVar(&name, "name", "", "Task-local artifact name")
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	cmd.Flags().StringVar(&kind, "kind", "intermediate", "Artifact kind: intermediate or output")
	_ = cmd.MarkFlagRequired("source")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}
