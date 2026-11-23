package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

func gatewayCmd() *cobra.Command {
	root := &cobra.Command{Use: "gateway", Short: "Manage gateway model and MCP assets before assigning them to Agents"}
	for _, kind := range []string{"models", "mcps"} {
		kind := kind
		group := &cobra.Command{Use: kind, Short: "Manage " + kind + " through Controller"}
		actions := []string{"capabilities", "list", "get", "save", "bind", "verify"}
		if kind == "mcps" {
			actions = []string{"capabilities", "list", "get", "save", "bind", "unbind", "authorize", "verify", "call", "delete", "import"}
		}
		for _, action := range actions {
			action := action
			var file, worker, provider, model, tool, arguments string
			var update bool
			command := &cobra.Command{Use: action + " [name]", Short: action + " gateway " + kind, Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
				body := map[string]any{"action": action}
				if len(args) > 0 {
					body["name"] = args[0]
				}
				if worker != "" {
					if err := validateWorkerName(worker); err != nil {
						return err
					}
					body["worker"] = worker
				}
				if provider != "" {
					body["provider"] = provider
				}
				if model != "" {
					body["model"] = model
				}
				if tool != "" {
					body["tool"] = tool
				}
				if update {
					body["update"] = true
				}
				if arguments != "" {
					var value map[string]any
					if json.Unmarshal([]byte(arguments), &value) != nil || value == nil {
						return fmt.Errorf("--arguments must be a JSON object")
					}
					body["arguments"] = value
				}
				if file != "" {
					data, err := os.ReadFile(file)
					if err != nil {
						return err
					}
					if action == "import" {
						body["content"] = string(data)
					} else {
						var value any
						if err := yaml.Unmarshal(data, &value); err != nil {
							return fmt.Errorf("invalid YAML/JSON input: %w", err)
						}
						field := "input"
						if action == "authorize" {
							field = "workers"
						}
						body[field] = value
					}
				}
				if (action == "save" || action == "import" || action == "authorize") && file == "" {
					return fmt.Errorf("%s requires -f FILE", action)
				}
				api := NewAPIClient()
				api.HTTPClient.Timeout = 90 * time.Second
				var result any
				path := "/api/v1/gateway/assets/" + kind
				if action == "verify" && worker != "" {
					path = "/api/v1/workers/" + worker + "/gateway-probe"
					body = map[string]any{"kind": strings.TrimSuffix(kind, "s")}
					if len(args) > 0 {
						body["server"] = args[0]
					}
				}
				if err := api.DoJSON("POST", path, body, &result); err != nil {
					return err
				}
				data, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
				if result, ok := result.(map[string]any); ok && result["success"] == false {
					return fmt.Errorf("gateway operation did not succeed; inspect the result and retry, do not bypass permissions via Agent CR")
				}
				return nil
			}}
			command.Flags().StringVarP(&file, "file", "f", "", "YAML/JSON asset input; Worker array for authorize; OpenAPI file for import")
			command.Flags().StringVar(&worker, "worker", "", "Target Worker CR name (verify uses its saved gateway identity)")
			command.Flags().StringVar(&provider, "provider", "", "Model provider to bind")
			command.Flags().StringVar(&model, "model", "", "Model alias to bind or probe")
			command.Flags().StringVar(&tool, "tool", "", "MCP tool to call explicitly")
			command.Flags().StringVar(&arguments, "arguments", "", "MCP tool arguments as JSON")
			command.Flags().BoolVar(&update, "update", false, "Update an existing model provider and synchronize its managed route")
			group.AddCommand(command)
		}
		root.AddCommand(group)
	}
	return root
}
