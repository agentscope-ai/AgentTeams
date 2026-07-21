package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func statusCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster status",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewAPIClient()
			var resp clusterStatusResp
			if err := client.DoJSON("GET", "/api/v1/status", nil, &resp); err != nil {
				return fmt.Errorf("get status: %w", err)
			}
			if output == "json" {
				printJSON(resp)
				return nil
			}
			printDetail([]KeyValue{
				{"Mode", resp.KubeMode},
				{"Total Workers", fmt.Sprintf("%d", resp.TotalWorkers)},
				{"Total Teams", fmt.Sprintf("%d", resp.TotalTeams)},
				{"Total Humans", fmt.Sprintf("%d", resp.TotalHumans)},
			})
			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	return cmd
}

func versionCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show controller version",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewAPIClient()
			var resp versionResp
			if err := client.DoJSON("GET", "/api/v1/version", nil, &resp); err != nil {
				return fmt.Errorf("get version: %w", err)
			}
			if output == "json" {
				printJSON(resp)
				return nil
			}
			printDetail([]KeyValue{
				{"Controller", resp.Controller},
				{"Mode", resp.KubeMode},
			})
			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	return cmd
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

type clusterStatusResp struct {
	KubeMode     string `json:"kubeMode"`
	TotalWorkers int    `json:"totalWorkers"`
	TotalTeams   int    `json:"totalTeams"`
	TotalHumans  int    `json:"totalHumans"`
}

type versionResp struct {
	Controller string `json:"controller"`
	KubeMode   string `json:"kubeMode"`
}
