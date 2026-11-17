package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func workerRuntimeCmd(switchRuntime bool) *cobra.Command {
	var name, target, id, output string
	cmd := &cobra.Command{Use: "runtime-status", Short: "Read the current Worker Harness handoff", Args: cobra.NoArgs}
	if switchRuntime {
		cmd.Use, cmd.Short = "switch-runtime", "Switch the Worker Harness at a shared checkpoint"
		cmd.Flags().StringVar(&target, "runtime", "", "Target Harness (required)")
		cmd.MarkFlagRequired("runtime")
		cmd.Flags().StringVar(&id, "request-id", "", "Stable handoff ID for retrying a specific request")
	}
	cmd.Flags().StringVar(&name, "name", "", "Worker name (required)")
	cmd.MarkFlagRequired("name")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := validateWorkerName(name); err != nil {
			return err
		}
		if output != "" && output != "json" {
			return fmt.Errorf("unsupported output format %q", output)
		}
		client := NewAPIClient()
		path := "/api/v1/workers/" + url.PathEscape(name) + "/runtime-handoff"
		resp, err := client.Do(http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		etag := resp.Header.Get("ETag")
		var state service.WorkerRuntimeState
		if err = decodeJSONResponse(resp, &state); err != nil {
			return err
		}
		if switchRuntime {
			requestID := id
			if requestID == "" {
				if op := state.Handoff; op != nil && op.TargetRuntime == target {
					requestID = op.ID
				} else {
					requestID = uuid.NewString()
				}
			}
			if !service.ValidWorkerHandoffRequest(requestID, target) {
				return fmt.Errorf("invalid handoff ID or target Harness %q", target)
			}
			headers := make(http.Header)
			if etag == "" {
				headers.Set("If-None-Match", "*")
			} else {
				headers.Set("If-Match", etag)
			}
			resp, err = client.doWithHeaders(http.MethodPost, path, map[string]string{"request_id": requestID, "target_runtime": target}, headers)
			if err != nil {
				return fmt.Errorf("handoff %s: %w", requestID, err)
			}
			if err = decodeJSONResponse(resp, &state); err != nil {
				return fmt.Errorf("handoff %s: %w", requestID, err)
			}
		}
		if output == "json" {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
		}
		if op := state.Handoff; op != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "worker/%s %s → %s  phase=%s  request=%s\n", name, op.Source.Runtime, op.TargetRuntime, op.Phase, op.ID)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "worker/%s has no recorded Harness handoff\n", name)
		}
		return nil
	}
	return cmd
}
