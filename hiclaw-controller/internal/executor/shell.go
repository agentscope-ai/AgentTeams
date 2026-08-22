package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Result holds the parsed output from a shell script execution.
type Result struct {
	Raw          string
	JSON         map[string]interface{}
	MatrixUserID string
	RoomID       string
}

// Shell executes HiClaw bash scripts and parses their output.
type Shell struct {
	ScriptsDir string // base path, e.g. /opt/hiclaw/agent/skills
	Timeout    time.Duration
}

func NewShell(scriptsDir string) *Shell {
	return &Shell{
		ScriptsDir: scriptsDir,
		Timeout:    10 * time.Minute,
	}
}

// Run executes a script with the given arguments and returns the parsed result.
func (s *Shell) Run(ctx context.Context, script string, args ...string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", append([]string{script}, args...)...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("script %s failed: %w\nstderr: %s\nstdout: %s",
			script, err, stderr.String(), stdout.String())
	}

	result := &Result{Raw: stdout.String()}

	// Parse ---RESULT--- JSON block if present
	if idx := strings.Index(result.Raw, "---RESULT---"); idx >= 0 {
		jsonStr := strings.TrimSpace(result.Raw[idx+len("---RESULT---"):])
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil {
			result.JSON = parsed
			if uid, ok := parsed["matrix_user_id"].(string); ok {
				result.MatrixUserID = uid
			}
			if rid, ok := parsed["room_id"].(string); ok {
				result.RoomID = rid
			}
		}
	}

	return result, nil
}

// RunSimple executes a script and returns raw output without JSON parsing.
func (s *Shell) RunSimple(ctx context.Context, script string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", append([]string{script}, args...)...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("script %s failed: %w\nstderr: %s", script, err, stderr.String())
	}

	return stdout.String(), nil
}
