package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

type taskPreparedFile struct {
	ArtifactID string            `json:"artifactId,omitempty"`
	Name       string            `json:"name"`
	Kind       string            `json:"kind,omitempty"`
	Version    string            `json:"version"`
	Size       int64             `json:"size"`
	SHA256     string            `json:"sha256,omitempty"`
	LocalPath  string            `json:"localPath,omitempty"`
	Preview    *taskInputPreview `json:"preview,omitempty"`
}

type taskInputPreview struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

type taskPreparedInput struct {
	TaskID       string             `json:"taskId"`
	SubmissionID string             `json:"submissionId,omitempty"`
	Summary      string             `json:"summary,omitempty"`
	Purpose      string             `json:"purpose,omitempty"`
	Review       json.RawMessage    `json:"review,omitempty"`
	ReadOnly     bool               `json:"readOnly"`
	Directory    string             `json:"directory,omitempty"`
	Files        []taskPreparedFile `json:"files"`
}

func taskBeginCmd() *cobra.Command {
	var team string
	cmd := &cobra.Command{Use: "begin <task-id>", Short: "Acknowledge work and prepare its accepted inputs", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !taskIdentifier.MatchString(args[0]) {
			return fmt.Errorf("invalid task id")
		}
		process := os.Getenv("AGENTTEAMS_PROCESS_ID")
		shared := os.Getenv("TEAMHARNESS_SHARED_DIR")
		if shared == "" {
			shared = os.Getenv("AGENTTEAMS_SHARED_DIR")
		}
		if process == "" || shared == "" {
			return fmt.Errorf("native task execution identity or shared workspace unavailable")
		}
		root, err := filepath.Abs(shared)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		tasks, err := taskLocalDirectory(root, "tasks")
		if err != nil {
			return err
		}
		directory, err := taskLocalDirectory(tasks, args[0])
		if err != nil {
			return err
		}
		outputs, err := taskLocalDirectory(directory, "workspace")
		if err != nil {
			return err
		}
		scratch, err := taskLocalDirectory(outputs, ".scratch")
		if err != nil {
			return err
		}
		query := url.Values{}
		if team != "" {
			query.Set("team", team)
		}
		response, err := NewAPIClient().doWithHeaders(http.MethodPost, "/api/v1/tasks/"+url.PathEscape(args[0])+"/begin?"+query.Encode(), nil, http.Header{"X-AgentTeams-Process-ID": {process}})
		if err != nil {
			return err
		}
		var context map[string]json.RawMessage
		if err := decodeJSONResponse(response, &context); err != nil {
			return err
		}
		var task struct {
			ID     string `json:"id"`
			Team   string `json:"team"`
			Status string `json:"status"`
		}
		if json.Unmarshal(context["task"], &task) != nil || task.ID != args[0] || task.Status != "in_progress" {
			return fmt.Errorf("invalid acknowledged task context")
		}
		var inputs []taskPreparedInput
		if json.Unmarshal(context["inputs"], &inputs) != nil {
			return fmt.Errorf("invalid task input context")
		}
		inputRoot, err := taskLocalDirectory(directory, "inputs")
		if err != nil {
			return err
		}
		batch, err := os.MkdirTemp(inputRoot, "prepare-")
		if err != nil {
			return err
		}
		prepared := false
		defer func() {
			if !prepared {
				_ = os.RemoveAll(batch)
			}
		}()
		previewBudget := 16384
		for i := range inputs {
			input := &inputs[i]
			if !taskIdentifier.MatchString(input.TaskID) || !input.ReadOnly {
				return fmt.Errorf("invalid prepared input identity")
			}
			input.Directory, err = taskLocalDirectory(batch, input.TaskID)
			if err != nil {
				return err
			}
			for j := range input.Files {
				file := &input.Files[j]
				if !validTaskLocalName(file.Name) || file.Version == "" {
					return fmt.Errorf("invalid task input reference")
				}
				target := filepath.Join(input.Directory, filepath.FromSlash(file.Name))
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					return err
				}
				query := url.Values{"version": {file.Version}}
				if task.Team != "" {
					query.Set("team", task.Team)
				}
				if file.ArtifactID != "" {
					query.Set("artifactId", file.ArtifactID)
				} else {
					query.Set("name", file.Name)
				}
				download, err := fetchTaskFile("/api/v1/tasks/"+url.PathEscape(input.TaskID)+"/files?"+query.Encode(), input.TaskID, file.Name, file.Version, target)
				if err != nil {
					return fmt.Errorf("prepare input %s/%s: %w", input.TaskID, file.Name, err)
				}
				if int64(download.Size) != file.Size || (file.SHA256 != "" && file.SHA256 != download.SHA256) {
					return fmt.Errorf("prepared input content changed")
				}
				if err := os.Chmod(target, 0444); err != nil {
					return err
				}
				file.LocalPath, file.SHA256 = download.LocalPath, download.SHA256
				if previewBudget > 0 && utf8.Valid(download.content) && bytes.IndexByte(download.content, 0) < 0 {
					end := min(len(download.content), 8192, previewBudget)
					for end > 0 && !utf8.Valid(download.content[:end]) {
						end--
					}
					file.Preview = &taskInputPreview{Text: string(download.content[:end]), Truncated: end < len(download.content)}
					previewBudget -= end
				}
			}
		}
		context["inputs"], _ = json.Marshal(inputs)
		context["taskDirectory"], _ = json.Marshal(directory)
		context["outputDirectory"], _ = json.Marshal(outputs)
		context["scratchDirectory"], _ = json.Marshal(scratch)
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(context); err != nil {
			return err
		}
		prepared = true
		return nil
	}}
	cmd.Flags().StringVar(&team, "team", "", "Task's owning Team when its ID is ambiguous")
	return cmd
}

func validTaskLocalName(name string) bool {
	return name != "" && name != "." && name != ".." && path.Clean(name) == name && !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\\x00")
}

func taskLocalDirectory(parent, name string) (string, error) {
	target := filepath.Join(parent, name)
	if err := os.Mkdir(target, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	if real != target {
		return "", fmt.Errorf("task directory is redirected outside its expected location")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("task location is not a directory")
	}
	return real, nil
}
