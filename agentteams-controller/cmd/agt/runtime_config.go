package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/spf13/cobra"
	sigyaml "sigs.k8s.io/yaml"
)

func loadWorkerRuntimeConfig(path string) (*v1beta1.WorkerRuntimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --runtime-config-file %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("--runtime-config-file %q is empty", path)
	}
	jsonData, err := sigyaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("malformed --runtime-config-file %q: %w", path, err)
	}
	if bytes.Equal(bytes.TrimSpace(jsonData), []byte("null")) {
		return nil, fmt.Errorf("--runtime-config-file %q is empty", path)
	}

	var config v1beta1.WorkerRuntimeConfig
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("malformed --runtime-config-file %q: %w", path, err)
	}
	if err := ensureSingleJSONDocument(decoder); err != nil {
		return nil, fmt.Errorf("malformed --runtime-config-file %q: %w", path, err)
	}
	return &config, nil
}

func ensureSingleJSONDocument(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple documents are not supported")
	}
	return err
}

func workerRuntimeConfigFromFlags(cmd *cobra.Command, runtime, configFile string, deepAgentsSandbox bool, coordinators string) (*v1beta1.WorkerRuntimeConfig, error) {
	convenienceSet := cmd.Flags().Changed("deepagents-sandbox") || cmd.Flags().Changed("deepagents-coordinators")
	if configFile != "" {
		if convenienceSet {
			return nil, fmt.Errorf("--runtime-config-file cannot be combined with --deepagents-sandbox or --deepagents-coordinators")
		}
		return loadWorkerRuntimeConfig(configFile)
	}
	if !convenienceSet {
		return nil, nil
	}
	if !deepAgentsSandbox {
		return nil, fmt.Errorf("--deepagents-coordinators requires --deepagents-sandbox")
	}
	if runtime != "deepagents" {
		return nil, fmt.Errorf("--deepagents-sandbox requires --runtime deepagents")
	}

	approvers := splitCSV(coordinators)
	if len(approvers) == 0 {
		adminUser := strings.TrimSpace(os.Getenv("AGENTTEAMS_ADMIN_USER"))
		matrixDomain := strings.TrimSpace(os.Getenv("AGENTTEAMS_MATRIX_DOMAIN"))
		if adminUser == "" || matrixDomain == "" {
			return nil, fmt.Errorf("--deepagents-sandbox requires --deepagents-coordinators or both AGENTTEAMS_ADMIN_USER and AGENTTEAMS_MATRIX_DOMAIN")
		}
		approvers = []string{"@" + adminUser + ":" + matrixDomain}
	}

	return &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
		Approvals: v1beta1.DeepAgentsApprovalConfig{
			FileWrites:   "required",
			MCPDefault:   "required",
			Coordinators: approvers,
		},
		Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
	}}, nil
}
