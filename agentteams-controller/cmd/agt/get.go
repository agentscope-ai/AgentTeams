package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func getCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Display resources",
	}
	cmd.AddCommand(getWorkersCmd())
	cmd.AddCommand(getTeamsCmd())
	cmd.AddCommand(getHumansCmd())
	cmd.AddCommand(getManagersCmd())
	cmd.AddCommand(getProjectsCmd())
	return cmd
}

// ---------------------------------------------------------------------------
// get workers
// ---------------------------------------------------------------------------

func getWorkersCmd() *cobra.Command {
	var team string
	var output string

	cmd := &cobra.Command{
		Use:   "workers [name]",
		Short: "Display Workers",
		Long: `List all Workers or get a specific Worker by name.

  agt get workers
  agt get workers alice
  agt get workers --team alpha-team
  agt get workers alice -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewAPIClient()

			if len(args) == 1 {
				var resp workerResp
				if err := client.DoJSON("GET", "/api/v1/workers/"+args[0], nil, &resp); err != nil {
					return fmt.Errorf("get worker: %w", err)
				}
				if output == "json" {
					printJSON(resp)
					return nil
				}
				printDetail(workerDetail(resp))
				return nil
			}

			path := "/api/v1/workers"
			if team != "" {
				path += "?team=" + team
			}
			var resp workerListResp
			if err := client.DoJSON("GET", path, nil, &resp); err != nil {
				return fmt.Errorf("list workers: %w", err)
			}
			if output == "json" {
				printJSON(resp)
				return nil
			}
			if resp.Total == 0 {
				fmt.Println("No workers found.")
				return nil
			}
			headers := []string{"NAME", "PHASE", "MODEL", "TEAM", "RUNTIME"}
			var rows [][]string
			for _, w := range resp.Workers {
				rows = append(rows, []string{
					w.Name,
					or(w.Phase, "Pending"),
					w.Model,
					or(w.Team, "-"),
					or(w.Runtime, "openclaw"),
				})
			}
			printTable(headers, rows)
			return nil
		},
	}

	cmd.Flags().StringVar(&team, "team", "", "Filter by team name")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	return cmd
}

// ---------------------------------------------------------------------------
// get projects
// ---------------------------------------------------------------------------

func getProjectsCmd() *cobra.Command {
	var team string
	var mermaid bool
	var output string

	cmd := &cobra.Command{
		Use:   "projects [name]",
		Short: "Display projects / workflow",
		Long: `List all projects or show the workflow detail of one project.

  agt get projects
  agt get projects --team alpha-team
  agt get projects demo-project-001
  agt get projects demo-project-001 -o json
  agt get projects demo-project-001 --mermaid`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewAPIClient()

			if len(args) == 1 {
				var resp map[string]any
				path := "/api/v1/projects/" + args[0] + "/workflow"
				if err := client.DoJSON("GET", path, nil, &resp); err != nil {
					return fmt.Errorf("get project workflow: %w", err)
				}
				if mermaid {
					fmt.Println(workflowMermaid(resp))
					return nil
				}
				if output == "json" {
					printJSON(resp)
					return nil
				}
				fields := []KeyValue{
					{"Project", toStr(resp["project_id"])},
					{"Title", toStr(resp["title"])},
					{"Status", toStr(resp["status"])},
					{"Plan", toStr(resp["plan_type"])},
					{"Team", or(toStr(resp["team_id"]), "-")},
					{"Next", listStr(resp["next"])},
					{"Nodes", listStr(resp["nodes"])},
					{"Interrupts", listStr(resp["interrupts"])},
				}
				printDetail(fields)
				return nil
			}

			path := "/api/v1/projects"
			if team != "" {
				path += "?team=" + team
			}
			var resp struct {
				Projects []map[string]any `json:"projects"`
				Total    int              `json:"total"`
			}
			if err := client.DoJSON("GET", path, nil, &resp); err != nil {
				return fmt.Errorf("list projects: %w", err)
			}
			if output == "json" {
				printJSON(resp)
				return nil
			}
			if resp.Total == 0 {
				fmt.Println("No projects found.")
				return nil
			}
			headers := []string{"PROJECT", "TITLE", "STATUS", "PLAN", "TEAM"}
			var rows [][]string
			for _, p := range resp.Projects {
				rows = append(rows, []string{
					toStr(p["project_id"]),
					toStr(p["title"]),
					toStr(p["status"]),
					toStr(p["plan_type"]),
					or(toStr(p["team_id"]), "-"),
				})
			}
			printTable(headers, rows)
			return nil
		},
	}

	cmd.Flags().StringVar(&team, "team", "", "Filter by team name")
	cmd.Flags().BoolVar(&mermaid, "mermaid", false, "Render workflow as a Mermaid flowchart")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	return cmd
}

// listStr renders a JSON-decoded array as a comma-separated list for table
// output (e.g. workflow nodes, next, interrupts).
func listStr(v any) string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	parts := make([]string, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			if id := toStr(m["id"]); id != "" {
				parts = append(parts, id)
				continue
			}
		}
		parts = append(parts, toStr(item))
	}
	return strings.Join(parts, ", ")
}

// workflowMermaid renders a workflow response as a Mermaid flowchart
// (flowchart LR), mirroring LangGraph's draw_mermaid helper. Status is
// appended to each node label; next/ready nodes are highlighted.
func workflowMermaid(resp map[string]any) string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	nodes, _ := resp["nodes"].([]any)
	edges, _ := resp["edges"].([]any)
	nextSet := map[string]bool{}
	for _, n := range resp["next"].([]any) {
		if id, ok := n.(string); ok {
			nextSet[id] = true
		}
	}
	for _, raw := range nodes {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := toStr(m["id"])
		name := toStr(m["name"])
		status := toStr(m["status"])
		label := name
		if status != "" {
			label += ": " + status
		}
		style := ""
		if nextSet[id] {
			style = ":::ready"
		}
		fmt.Fprintf(&b, "    %s[%q]%s\n", id, label, style)
	}
	for _, raw := range edges {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		src := toStr(m["source"])
		dst := toStr(m["target"])
		fmt.Fprintf(&b, "    %s --> %s\n", src, dst)
	}
	b.WriteString("    classDef ready fill:#d4edda,stroke:#28a745;\n")
	return b.String()
}

// toStr converts a JSON-decoded value to its string form for table output.
func toStr(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(s)
	case []any, map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func getTeamsCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "teams [name]",
		Short: "Display Teams",
		Long: `List all Teams or get a specific Team by name.

  agt get teams
  agt get teams alpha-team
  agt get teams alpha-team -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewAPIClient()

			if len(args) == 1 {
				var resp teamResp
				if err := client.DoJSON("GET", "/api/v1/teams/"+args[0], nil, &resp); err != nil {
					return fmt.Errorf("get team: %w", err)
				}
				if output == "json" {
					printJSON(resp)
					return nil
				}
				printDetail(teamDetail(resp))
				return nil
			}

			var resp teamListResp
			if err := client.DoJSON("GET", "/api/v1/teams", nil, &resp); err != nil {
				return fmt.Errorf("list teams: %w", err)
			}
			if output == "json" {
				printJSON(resp)
				return nil
			}
			if resp.Total == 0 {
				fmt.Println("No teams found.")
				return nil
			}
			headers := []string{"NAME", "PHASE", "LEADER", "WORKERS", "READY"}
			var rows [][]string
			for _, t := range resp.Teams {
				ready := fmt.Sprintf("%d/%d", t.ReadyWorkers, t.TotalWorkers)
				rows = append(rows, []string{
					t.Name,
					or(t.Phase, "Pending"),
					t.LeaderName,
					strings.Join(t.WorkerNames, ","),
					ready,
				})
			}
			printTable(headers, rows)
			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	return cmd
}

// ---------------------------------------------------------------------------
// get humans
// ---------------------------------------------------------------------------

func getHumansCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "humans [name]",
		Short: "Display Humans",
		Long: `List all Humans or get a specific Human by name.

  agt get humans
  agt get humans bob
  agt get humans bob -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewAPIClient()

			if len(args) == 1 {
				var resp humanResp
				if err := client.DoJSON("GET", "/api/v1/humans/"+args[0], nil, &resp); err != nil {
					return fmt.Errorf("get human: %w", err)
				}
				if output == "json" {
					printJSON(resp)
					return nil
				}
				printDetail(humanDetail(resp))
				return nil
			}

			var resp humanListResp
			if err := client.DoJSON("GET", "/api/v1/humans", nil, &resp); err != nil {
				return fmt.Errorf("list humans: %w", err)
			}
			if output == "json" {
				printJSON(resp)
				return nil
			}
			if resp.Total == 0 {
				fmt.Println("No humans found.")
				return nil
			}
			headers := []string{"NAME", "PHASE", "DISPLAY-NAME", "MATRIX-ID"}
			var rows [][]string
			for _, h := range resp.Humans {
				rows = append(rows, []string{
					h.Name,
					or(h.Phase, "Pending"),
					h.DisplayName,
					or(h.MatrixUserID, "-"),
				})
			}
			printTable(headers, rows)
			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	return cmd
}

// ---------------------------------------------------------------------------
// get managers
// ---------------------------------------------------------------------------

func getManagersCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "managers [name]",
		Short: "Display Managers",
		Long: `List all Managers or get a specific Manager by name.

  agt get managers
  agt get managers default
  agt get managers default -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewAPIClient()

			if len(args) == 1 {
				var resp managerResp
				if err := client.DoJSON("GET", "/api/v1/managers/"+args[0], nil, &resp); err != nil {
					return fmt.Errorf("get manager: %w", err)
				}
				if output == "json" {
					printJSON(resp)
					return nil
				}
				printDetail(managerDetail(resp))
				return nil
			}

			var resp managerListResp
			if err := client.DoJSON("GET", "/api/v1/managers", nil, &resp); err != nil {
				return fmt.Errorf("list managers: %w", err)
			}
			if output == "json" {
				printJSON(resp)
				return nil
			}
			if resp.Total == 0 {
				fmt.Println("No managers found.")
				return nil
			}
			headers := []string{"NAME", "PHASE", "MODEL", "RUNTIME"}
			var rows [][]string
			for _, m := range resp.Managers {
				rows = append(rows, []string{
					m.Name,
					or(m.Phase, "Pending"),
					m.Model,
					or(m.Runtime, "openclaw"),
				})
			}
			printTable(headers, rows)
			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	return cmd
}

// ---------------------------------------------------------------------------
// Response types (lightweight, no K8s dependency)
// ---------------------------------------------------------------------------

type workerResp struct {
	Name             string                   `json:"name"`
	WorkerName       string                   `json:"workerName,omitempty"`
	Phase            string                   `json:"phase"`
	ContainerManaged bool                     `json:"containerManaged"`
	State            string                   `json:"state,omitempty"`
	Model            string                   `json:"model,omitempty"`
	Runtime          string                   `json:"runtime,omitempty"`
	Image            string                   `json:"image,omitempty"`
	Identity         string                   `json:"identity,omitempty"`
	Skills           []string                 `json:"skills,omitempty"`
	McpServers       []map[string]interface{} `json:"mcpServers,omitempty"`
	ContainerState   string                   `json:"containerState,omitempty"`
	MatrixUserID     string                   `json:"matrixUserID,omitempty"`
	RoomID           string                   `json:"roomID,omitempty"`
	Message          string                   `json:"message,omitempty"`
	Team             string                   `json:"team,omitempty"`
	Role             string                   `json:"role,omitempty"`
}

type workerListResp struct {
	Workers []workerResp `json:"workers"`
	Total   int          `json:"total"`
}

type teamResp struct {
	Name              string              `json:"name"`
	TeamName          string              `json:"teamName,omitempty"`
	Phase             string              `json:"phase"`
	Description       string              `json:"description,omitempty"`
	Admin             *teamAdminResp      `json:"admin,omitempty"`
	HumanMembers      []teamHumanResp     `json:"humanMembers,omitempty"`
	LeaderName        string              `json:"leaderName"`
	LeaderHeartbeat   *teamHeartbeatResp  `json:"leaderHeartbeat,omitempty"`
	WorkerIdleTimeout string              `json:"workerIdleTimeout,omitempty"`
	TeamRoomID        string              `json:"teamRoomID,omitempty"`
	LeaderDMRoomID    string              `json:"leaderDMRoomID,omitempty"`
	LeaderReady       bool                `json:"leaderReady"`
	ReadyWorkers      int                 `json:"readyWorkers"`
	TotalWorkers      int                 `json:"totalWorkers"`
	Message           string              `json:"message,omitempty"`
	WorkerNames       []string            `json:"workerNames,omitempty"`
	WorkerMembers     []map[string]string `json:"workerMembers"`
}

type teamAdminResp struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrixUserId,omitempty"`
}

type teamHumanResp struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrixUserId,omitempty"`
	Role         string `json:"role,omitempty"`
}

type teamHeartbeatResp struct {
	Enabled bool   `json:"enabled,omitempty"`
	Every   string `json:"every,omitempty"`
}

type teamListResp struct {
	Teams []teamResp `json:"teams"`
	Total int        `json:"total"`
}

type humanResp struct {
	Name              string   `json:"name"`
	Phase             string   `json:"phase"`
	DisplayName       string   `json:"displayName"`
	PermissionLevel   int      `json:"permissionLevel"`
	AccessibleTeams   []string `json:"accessibleTeams,omitempty"`
	AccessibleWorkers []string `json:"accessibleWorkers,omitempty"`
	MatrixUserID      string   `json:"matrixUserID,omitempty"`
	InitialPassword   string   `json:"initialPassword,omitempty"`
	Rooms             []string `json:"rooms,omitempty"`
	Message           string   `json:"message,omitempty"`
}

type humanListResp struct {
	Humans []humanResp `json:"humans"`
	Total  int         `json:"total"`
}

type managerResp struct {
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	Model        string `json:"model,omitempty"`
	Runtime      string `json:"runtime,omitempty"`
	Image        string `json:"image,omitempty"`
	MatrixUserID string `json:"matrixUserID,omitempty"`
	RoomID       string `json:"roomID,omitempty"`
	Version      string `json:"version,omitempty"`
	Message      string `json:"message,omitempty"`
	WelcomeSent  bool   `json:"welcomeSent"`
}

type managerListResp struct {
	Managers []managerResp `json:"managers"`
	Total    int           `json:"total"`
}

// ---------------------------------------------------------------------------
// Detail formatters
// ---------------------------------------------------------------------------

func workerDetail(w workerResp) []KeyValue {
	return []KeyValue{
		{"Name", w.Name},
		{"Phase", or(w.Phase, "Pending")},
		{"Model", w.Model},
		{"Runtime", or(w.Runtime, "openclaw")},
		{"ContainerState", w.ContainerState},
		{"Image", w.Image},
		{"Team", w.Team},
		{"Role", w.Role},
		{"MatrixUserID", w.MatrixUserID},
		{"RoomID", w.RoomID},
		{"Message", w.Message},
	}
}

func teamDetail(t teamResp) []KeyValue {
	return []KeyValue{
		{"Name", t.Name},
		{"TeamName", t.TeamName},
		{"Phase", or(t.Phase, "Pending")},
		{"Description", t.Description},
		{"Leader", t.LeaderName},
		{"LeaderHeartbeat", teamHeartbeatText(t.LeaderHeartbeat)},
		{"WorkerIdleTimeout", t.WorkerIdleTimeout},
		{"LeaderReady", strconv.FormatBool(t.LeaderReady)},
		{"Workers", strings.Join(t.WorkerNames, ", ")},
		{"ReadyWorkers", fmt.Sprintf("%d/%d", t.ReadyWorkers, t.TotalWorkers)},
		{"TeamRoomID", t.TeamRoomID},
		{"LeaderDMRoomID", t.LeaderDMRoomID},
		{"Message", t.Message},
	}
}

func teamHeartbeatText(hb *teamHeartbeatResp) string {
	if hb == nil {
		return ""
	}
	if hb.Every != "" {
		return hb.Every
	}
	if hb.Enabled {
		return "enabled"
	}
	return "disabled"
}

func humanDetail(h humanResp) []KeyValue {
	return []KeyValue{
		{"Name", h.Name},
		{"Phase", or(h.Phase, "Pending")},
		{"DisplayName", h.DisplayName},
		{"MatrixUserID", h.MatrixUserID},
		{"InitialPassword", h.InitialPassword},
		{"Rooms", strings.Join(h.Rooms, ", ")},
		{"Message", h.Message},
	}
}

func managerDetail(m managerResp) []KeyValue {
	welcome := "false"
	if m.WelcomeSent {
		welcome = "true"
	}
	return []KeyValue{
		{"Name", m.Name},
		{"Phase", or(m.Phase, "Pending")},
		{"Model", m.Model},
		{"Runtime", or(m.Runtime, "openclaw")},
		{"Image", m.Image},
		{"MatrixUserID", m.MatrixUserID},
		{"RoomID", m.RoomID},
		{"Version", m.Version},
		{"WelcomeSent", welcome},
		{"Message", m.Message},
	}
}
