package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func taskSubmitCmd() *cobra.Command {
	var team, summary, report, status string
	var files []string
	cmd := &cobra.Command{Use: "submit <task-id>", Short: "Publish outputs and submit one candidate with an automatic notification receipt", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !taskIdentifier.MatchString(args[0]) {
			return fmt.Errorf("invalid task id")
		}
		process := os.Getenv("AGENTTEAMS_PROCESS_ID")
		if process == "" {
			return fmt.Errorf("native task execution identity unavailable")
		}
		if strings.TrimSpace(summary) == "" {
			return fmt.Errorf("submission summary required")
		}
		switch status {
		case "SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "INTERRUPTED":
		default:
			return fmt.Errorf("invalid result status")
		}
		shared := os.Getenv("TEAMHARNESS_SHARED_DIR")
		if shared == "" {
			shared = os.Getenv("AGENTTEAMS_SHARED_DIR")
		}
		if shared == "" {
			return fmt.Errorf("shared task workspace unavailable")
		}
		root, err := filepath.Abs(shared)
		if err != nil {
			return err
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		outputs := filepath.Join(root, "tasks", args[0], "workspace")
		realOutputs, err := filepath.EvalSymlinks(outputs)
		if err != nil {
			return err
		}
		if realOutputs != outputs {
			return fmt.Errorf("task output directory is redirected")
		}
		type output struct {
			name string
			data []byte
		}
		snapshots := []output{}
		seen := map[string]bool{}
		for _, name := range files {
			if !validTaskLocalName(name) || seen[name] {
				return fmt.Errorf("invalid or duplicate output name: %s", name)
			}
			seen[name] = true
			target, err := filepath.EvalSymlinks(filepath.Join(outputs, filepath.FromSlash(name)))
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(outputs, target)
			if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("output escapes task directory: %s", name)
			}
			file, err := os.Open(target)
			if err != nil {
				return err
			}
			info, err := file.Stat()
			if err != nil {
				file.Close()
				return err
			}
			const maximum = 64 << 20
			if !info.Mode().IsRegular() || info.Size() > maximum {
				file.Close()
				return fmt.Errorf("output must be a regular file of at most 64 MiB: %s", name)
			}
			data, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
			file.Close()
			if readErr != nil {
				return readErr
			}
			if len(data) > maximum {
				return fmt.Errorf("output too large: %s", name)
			}
			snapshots = append(snapshots, output{name, data})
		}
		client := NewAPIClient()
		artifacts := []string{}
		for _, file := range snapshots {
			query := url.Values{"name": {file.name}, "kind": {"output"}}
			if team != "" {
				query.Set("team", team)
			}
			var operation [16]byte
			if _, err := rand.Read(operation[:]); err != nil {
				return err
			}
			headers := http.Header{"Content-Type": {"application/octet-stream"}, "Idempotency-Key": {hex.EncodeToString(operation[:])}, "X-Agentteams-Process-Id": {process}}
			var response json.RawMessage
			if err := publishTaskFile(cmd.Context(), client, "/api/v1/tasks/"+url.PathEscape(args[0])+"/files?"+query.Encode(), file.data, headers, &response); err != nil {
				return err
			}
			var ref struct {
				ArtifactID string `json:"artifactId"`
				Name       string `json:"name"`
			}
			if json.Unmarshal(response, &ref) != nil || ref.ArtifactID == "" || ref.Name != file.name {
				return fmt.Errorf("invalid output publication receipt")
			}
			artifacts = append(artifacts, ref.ArtifactID)
		}
		body, err := json.Marshal(map[string]any{"summary": summary, "report": report, "status": status, "artifacts": artifacts})
		if err != nil {
			return err
		}
		query := url.Values{}
		if team != "" {
			query.Set("team", team)
		}
		response, err := client.doWithHeadersContext(cmd.Context(), http.MethodPost, "/api/v1/tasks/"+url.PathEscape(args[0])+"/submit?"+query.Encode(), json.RawMessage(body), http.Header{"X-Agentteams-Process-Id": {process}})
		if err != nil {
			return err
		}
		var result json.RawMessage
		if err := decodeJSONResponse(response, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	cmd.Flags().StringVar(&summary, "summary", "", "Candidate result summary")
	cmd.Flags().StringVar(&report, "report", "", "Optional detailed result text")
	cmd.Flags().StringVar(&status, "status", "SUCCESS", "Candidate result status")
	cmd.Flags().StringArrayVar(&files, "file", nil, "Output name relative to this task's output directory; repeat for more files")
	_ = cmd.MarkFlagRequired("summary")
	return cmd
}
