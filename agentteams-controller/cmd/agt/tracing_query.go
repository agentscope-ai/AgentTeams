package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func traceListCmd(use, positional string) *cobra.Command {
	var agent, worker, team, session, task, project, room, runtimeName, since, until, output string
	var limit, page int
	cmd := &cobra.Command{Use: use, Short: "List authorized execution traces", Args: cobra.NoArgs}
	if positional != "" {
		cmd.Args = cobra.ExactArgs(1)
	}
	if positional == "worker" {
		cmd.Aliases = []string{"agent"}
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if positional == "worker" {
			worker = args[0]
		}
		if positional == "session" {
			session = args[0]
		}
		if agent != "" && worker != "" && agent != worker {
			return fmt.Errorf("--agent and --worker must identify the same Agent")
		}
		if agent != "" {
			worker = agent
		}
		if limit < 1 || limit > 100 || page < 1 || page > 99999 {
			return fmt.Errorf("limit must be 1..100 and page 1..99999")
		}
		if output != "table" && output != "json" {
			return fmt.Errorf("output must be table or json")
		}
		from, err := traceQueryTime(since, true)
		if err != nil {
			return err
		}
		to, err := traceQueryTime(until, false)
		if err != nil {
			return err
		}
		if !from.IsZero() && !to.IsZero() && !from.Before(to) {
			return fmt.Errorf("--since must precede --until")
		}
		q := url.Values{"limit": {fmt.Sprint(limit)}, "page": {fmt.Sprint(page)}}
		for k, v := range map[string]string{"worker": worker, "team": team, "session": session, "task": task, "project": project, "room": room, "runtime": runtimeName} {
			if v != "" {
				q.Set(k, v)
			}
		}
		if !from.IsZero() {
			q.Set("since", from.UTC().Format(time.RFC3339Nano))
		}
		if !to.IsZero() {
			q.Set("until", to.UTC().Format(time.RFC3339Nano))
		}
		data, err := NewAPIClient().DoRaw("GET", "/api/v1/traces?"+q.Encode())
		if err != nil {
			return err
		}
		if output == "json" {
			return traceJSON(cmd, data)
		}
		var result struct {
			Data []struct {
				ID, Name, UserID, SessionID, Timestamp string
				Latency                                *float64
			}
			Meta struct{ Page, TotalPages, TotalItems int }
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("decode traces: %w", err)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "TRACE ID\tAGENT\tSESSION\tTIME\tLATENCY (s)\tNAME")
		for _, row := range result.Data {
			latency := "-"
			if row.Latency != nil {
				latency = fmt.Sprintf("%.3f", *row.Latency)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", traceCell(row.ID), traceCell(row.UserID), traceCell(row.SessionID), traceCell(row.Timestamp), latency, traceCell(row.Name))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Showing %d of %d traces; page %d/%d (use --page for other pages).\n", len(result.Data), result.Meta.TotalItems, result.Meta.Page, result.Meta.TotalPages)
		return err
	}
	f := cmd.Flags()
	f.StringVar(&agent, "agent", "", "Agent/Worker resource name (including team Leaders)")
	f.StringVar(&worker, "worker", "", "Alias of --agent")
	f.StringVar(&team, "team", "", "Filter by team")
	f.StringVar(&session, "session", "", "Filter by session; no default time cutoff")
	f.StringVar(&task, "task", "", "Filter by task ID")
	f.StringVar(&project, "project", "", "Filter by project ID")
	f.StringVar(&room, "room", "", "Filter by Matrix room ID")
	f.StringVar(&runtimeName, "runtime", "", "Filter by runtime")
	f.StringVar(&since, "since", "", "Start time (inclusive): RFC3339 with timezone or positive duration, e.g. 1h")
	f.StringVar(&until, "until", "", "End time (exclusive): RFC3339 with timezone")
	f.IntVar(&limit, "limit", 20, "Traces per page (1..100)")
	f.IntVar(&page, "page", 1, "Result page")
	f.StringVarP(&output, "output", "o", "table", "Output format: table or json")
	return cmd
}

func traceQueryTime(value string, relative bool) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if relative {
		if duration, err := time.ParseDuration(value); err == nil {
			if duration <= 0 {
				return time.Time{}, fmt.Errorf("--since duration must be positive")
			}
			return time.Now().Add(-duration), nil
		}
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid trace time %q; use RFC3339 with a timezone", value)
	}
	return t, nil
}

func traceCell(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, value)
}

func traceJSON(cmd *cobra.Command, data []byte) error {
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, data, "", "  "); err != nil {
		return fmt.Errorf("decode trace response: %w", err)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), formatted.String())
	return err
}

func traceShowCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{Use: "show <trace-id>", Short: "Inspect a trace and its model/tool observations", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if output != "table" && output != "json" {
			return fmt.Errorf("output must be table or json")
		}
		data, err := NewAPIClient().DoRaw("GET", "/api/v1/traces/"+url.PathEscape(args[0]))
		if err != nil {
			return err
		}
		if output == "json" {
			return traceJSON(cmd, data)
		}
		var result struct {
			ID, Name     string
			Observations []struct{ ID, ParentObservationID, Type, Name, Level, StartTime, EndTime, StatusMessage string }
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("decode trace: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Trace %s: %s\n", traceCell(result.ID), traceCell(result.Name))
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "SPAN ID\tPARENT\tTYPE\tLEVEL\tSTART\tEND\tNAME\tSTATUS")
		for _, o := range result.Observations {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", traceCell(o.ID), traceCell(o.ParentObservationID), traceCell(o.Type), traceCell(o.Level), traceCell(o.StartTime), traceCell(o.EndTime), traceCell(o.Name), traceCell(o.StatusMessage))
		}
		return w.Flush()
	}}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "Output format: table or json")
	return cmd
}
