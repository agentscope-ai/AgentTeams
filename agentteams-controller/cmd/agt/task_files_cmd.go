package main

import (
	"crypto/sha256"
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

func taskFilesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "files", Short: "Read declared task files without storage paths"}
	for _, action := range []string{"list", "read", "get"} {
		var team, name, version, output string
		var offset int64
		var limit int
		child := &cobra.Command{
			Use: action + " <task-id>", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if !taskIdentifier.MatchString(args[0]) {
					return fmt.Errorf("invalid task id")
				}
				if action != "list" && name == "" {
					return fmt.Errorf("--name must not be empty")
				}
				query := url.Values{}
				for key, value := range map[string]string{"team": team, "name": name, "version": version} {
					if value != "" {
						query.Set(key, value)
					}
				}
				if action == "read" {
					if offset < 0 || limit < 4 || limit > 32768 {
						return fmt.Errorf("offset must be nonnegative and limit 4..32768 bytes")
					}
					if offset > 0 && version == "" {
						return fmt.Errorf("--version is required to continue a text read")
					}
					query.Set("view", "text")
					query.Set("offset", fmt.Sprint(offset))
					query.Set("limit", fmt.Sprint(limit))
				}
				endpoint := "/api/v1/tasks/" + url.PathEscape(args[0]) + "/files?" + query.Encode()
				if action == "get" {
					return downloadTaskFile(cmd, endpoint, args[0], name, version, output)
				}
				var response json.RawMessage
				if err := NewAPIClient().DoJSON(http.MethodGet, endpoint, nil, &response); err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
			},
		}
		child.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
		if action != "list" {
			child.Flags().StringVar(&name, "name", "", "Declared task-local file name")
			_ = child.MarkFlagRequired("name")
			child.Flags().StringVar(&version, "version", "", "Exact file version; refuse a changed file")
		}
		if action == "read" {
			child.Flags().Int64Var(&offset, "offset", 0, "Byte offset returned by the previous read")
			child.Flags().IntVar(&limit, "limit", 32768, "Maximum text bytes (4..32768)")
		}
		if action == "get" {
			child.Flags().StringVar(&output, "output", "", "New local file; defaults to an isolated input directory")
		}
		cmd.AddCommand(child)
	}
	return cmd
}

func downloadTaskFile(cmd *cobra.Command, endpoint, taskID, name, expectedVersion, output string) error {
	response, err := NewAPIClient().Do(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if err := decodeJSONResponse(response, nil); err != nil {
			return err
		}
		return fmt.Errorf("unexpected task download status %d", response.StatusCode)
	}
	const maximum = 64 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return err
	}
	if len(data) > maximum {
		return fmt.Errorf("task file exceeds download limit")
	}
	digest := sha256.Sum256(data)
	checksum := hex.EncodeToString(digest[:])
	if response.Header.Get("X-Content-SHA256") != checksum {
		return fmt.Errorf("task file checksum mismatch; no local file published")
	}
	if response.Header.Get("ETag") == "" {
		return fmt.Errorf("task file version missing; no local file published")
	}
	version := strings.Trim(response.Header.Get("ETag"), `"`)
	if expectedVersion != "" && version != expectedVersion {
		return fmt.Errorf("task file version mismatch; no local file published")
	}
	var temporaryDir string
	if output == "" {
		temporaryDir, err = os.MkdirTemp("", "agentteams-task-input-")
		if err != nil {
			return err
		}
		output = filepath.Join(temporaryDir, filepath.Base(name))
	}
	complete := false
	defer func() {
		if !complete && temporaryDir != "" {
			_ = os.RemoveAll(temporaryDir)
		}
	}()
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(output)
		return err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(output)
		return err
	}
	complete = true
	return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
		TaskID    string `json:"taskId"`
		Name      string `json:"name"`
		Version   string `json:"version"`
		LocalPath string `json:"localPath"`
		Size      int    `json:"size"`
		SHA256    string `json:"sha256"`
	}{taskID, name, version, output, len(data), checksum})
}
