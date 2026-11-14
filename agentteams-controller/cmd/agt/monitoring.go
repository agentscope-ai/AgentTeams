package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/spf13/cobra"
)

func monitoringCmd() *cobra.Command {
	var page, limit int
	var search, output string
	cmd := &cobra.Command{
		Use: "monitoring", Short: "Inspect authorized component and Agent container metrics",
		Long: "Read a page of container metrics using your Controller identity (administrator or Manager). CPU 100% means one CPU core. Missing metrics remain unavailable; this is a current sample, not historical monitoring.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if page < 1 || page > 1000000 || limit < 1 || limit > 50 {
				return fmt.Errorf("page must be 1..1000000 and limit must be 1..50")
			}
			if output != "table" && output != "json" {
				return fmt.Errorf("output must be table or json")
			}
			q := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
			if search != "" {
				q.Set("search", search)
			}
			data, err := NewAPIClient().DoRaw(http.MethodGet, "/api/v1/monitoring?"+q.Encode())
			if err != nil {
				return err
			}
			if output == "json" {
				return monitoringJSON(cmd, data)
			}
			var result struct {
				Items []struct {
					Kind, Name, Availability, Reason string
					Metrics                          *backend.ContainerMetrics
				}
				Total, Page, TotalPages int
				SampledAt               string
			}
			if err := json.Unmarshal(data, &result); err != nil {
				return fmt.Errorf("decode monitoring response: %w", err)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "KIND\tNAME\tSTATE\tCPU\tMEMORY\tLIMIT\tRESTARTS\tOOM\tSOURCE")
			for _, row := range result.Items {
				state, cpu, memory, quota, restarts, oom := "—", "—", "—", "—", "—", "—"
				source := row.Availability
				if row.Reason != "" {
					source += ": " + row.Reason
				}
				if m := row.Metrics; m != nil {
					state = m.State
					restarts = strconv.Itoa(m.RestartCount)
					oom = strconv.FormatBool(m.OOMKilled)
					if m.CPUPercent != nil {
						cpu = fmt.Sprintf("%.2f%%", *m.CPUPercent)
					}
					memory = monitoringMemory(m.MemoryUsageBytes)
					quota = "unlimited"
					if m.MemoryLimitBytes != nil {
						quota = monitoringMemory(m.MemoryLimitBytes)
					}
					if m.UsageError != "" {
						source += ": " + m.UsageError
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", traceCell(row.Kind), traceCell(row.Name), traceCell(state), cpu, memory, quota, restarts, oom, traceCell(source))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Showing %d of %d resources; page %d/%d. Sampled at %s. Use --page for more resources or -o json for activity and lifecycle details.\n", len(result.Items), result.Total, result.Page, result.TotalPages, traceCell(result.SampledAt))
			return err
		},
	}
	cmd.Flags().IntVar(&page, "page", 1, "Result page")
	cmd.Flags().IntVar(&limit, "limit", 12, "Resources per page (1..50)")
	cmd.Flags().StringVar(&search, "search", "", "Filter by resource name, kind, runtime or phase")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "Output format: table or json")
	return cmd
}

func logsCmd() *cobra.Command {
	var tail int
	var since, output string
	cmd := &cobra.Command{
		Use: "logs <kind> <name>", Short: "Read authorized component or Agent container output",
		Long: "Read recent stdout/stderr through the Controller using your own administrator/Manager identity. Kind is worker, manager or component; use monitoring to discover names. Reads are bounded and common credential values are masked by the Controller. This command does not follow streams or execute container commands.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "worker" && args[0] != "manager" && args[0] != "component" {
				return fmt.Errorf("kind must be worker, manager or component")
			}
			if strings.TrimSpace(args[1]) == "" || args[1] == "." || args[1] == ".." || strings.ContainsAny(args[1], "/\\?#\x00\r\n") {
				return fmt.Errorf("invalid resource name")
			}
			if tail < 1 || tail > 1000 {
				return fmt.Errorf("tail must be 1..1000")
			}
			if output != "text" && output != "json" {
				return fmt.Errorf("output must be text or json")
			}
			q := url.Values{"tail": {strconv.Itoa(tail)}}
			if since != "" {
				t, err := time.Parse(time.RFC3339Nano, since)
				if err != nil {
					duration, parseErr := time.ParseDuration(since)
					if parseErr != nil || duration <= 0 {
						return fmt.Errorf("since must be an RFC3339 timestamp with timezone or a positive duration, e.g. 15m")
					}
					t = time.Now().Add(-duration)
				}
				q.Set("since", t.UTC().Format(time.RFC3339Nano))
			}
			path := "/api/v1/monitoring/" + args[0] + "/" + url.PathEscape(args[1]) + "/logs?" + q.Encode()
			data, err := NewAPIClient().DoRaw(http.MethodGet, path)
			if err != nil {
				return err
			}
			if output == "json" {
				return monitoringJSON(cmd, data)
			}
			var result struct {
				Text, SampledAt string
				Tail            int
				Truncated       bool
			}
			if err := json.Unmarshal(data, &result); err != nil {
				return fmt.Errorf("decode logs response: %w", err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Recent container output (tail %d); sampled at %s.\n", result.Tail, traceCell(result.SampledAt))
			if result.Truncated {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning: logs truncated by the Controller; narrow --since or --tail. This is not a complete log history.")
			}
			// Log content is untrusted. Keep line breaks, but never execute ANSI
			// or cursor-control characters in a human terminal.
			text := strings.Map(func(r rune) rune {
				if unicode.IsControl(r) && r != '\n' && r != '\t' {
					return -1
				}
				return r
			}, result.Text)
			_, err = fmt.Fprint(cmd.OutOrStdout(), text)
			return err
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 300, "Maximum recent log lines (1..1000)")
	cmd.Flags().StringVar(&since, "since", "", "Start time: RFC3339 with timezone or positive duration, e.g. 15m")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "Output format: text or json")
	return cmd
}

func monitoringMemory(value *uint64) string {
	if value == nil {
		return "—"
	}
	if *value >= 1<<30 {
		return fmt.Sprintf("%.2f GiB", float64(*value)/(1<<30))
	}
	return fmt.Sprintf("%.1f MiB", float64(*value)/(1<<20))
}

func monitoringJSON(cmd *cobra.Command, data []byte) error {
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, data, "", "  "); err != nil {
		return fmt.Errorf("decode monitoring response: %w", err)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), formatted.String())
	return err
}
