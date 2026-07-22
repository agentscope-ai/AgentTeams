package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// statusCmd shows a one-screen cluster overview of Workers, Teams, Managers,
// and Humans, with a phase breakdown per resource type and a compact table
// of names and key health signals. Use --watch for a live refresh or
// --output json for a single combined JSON payload.
func statusCmd() *cobra.Command {
	var (
		output string
		watch  bool
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster overview",
		Long: `Show a one-screen cluster overview of Workers, Teams, Managers, and Humans.

  hiclaw status                    # one-shot overview
  hiclaw status --watch            # refresh every 3 seconds
  hiclaw status --output json      # combined JSON for scripting`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if watch && output == "json" {
				return fmt.Errorf("--watch is not supported with --output json")
			}
			if watch {
				return runWatch()
			}
			return runOnce(output)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format (json)")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Re-render every 3 seconds")
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
			if err := client.DoJSON(context.Background(), "GET", "/api/v1/version", nil, &resp); err != nil {
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

// statusMaxRows caps how many rows a single table renders in the overview.
// Anything beyond this is summarised with a "... and N more" footer so the
// overview stays on one screen.
const statusMaxRows = 20

// overview is the combined dataset returned by `hiclaw status` and
// serialised for `hiclaw status --output json`.
type overview struct {
	Mode       string        `json:"mode"`
	Controller string        `json:"controller,omitempty"`
	Workers    []workerResp  `json:"workers"`
	Teams      []teamResp    `json:"teams"`
	Managers   []managerResp `json:"managers"`
	Humans     []humanResp   `json:"humans"`
	// Phase counts mirror the table headers so JSON consumers don't need to
	// recompute them. Empty phases are bucketed as "Pending".
	WorkerCounts  map[string]int `json:"workerCounts,omitempty"`
	TeamCounts    map[string]int `json:"teamCounts,omitempty"`
	ManagerCounts map[string]int `json:"managerCounts,omitempty"`
	HumanCounts   map[string]int `json:"humanCounts,omitempty"`
}

// runOnce prints (or JSON-encodes) a single overview.
func runOnce(output string) error {
	ov, err := fetchOverview()
	if err != nil {
		return fmt.Errorf("fetch overview: %w", err)
	}
	if output == "json" {
		printJSON(ov)
		return nil
	}
	printOverview(ov)
	return nil
}

// runWatch redraws the overview every 3 seconds until the user hits Ctrl-C.
// The redraw uses ANSI escape sequences that work in PowerShell 7, Windows
// Terminal, and any modern *nix terminal.
func runWatch() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		ov, err := fetchOverview()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hiclaw: %v\n", err)
		} else {
			fmt.Print("\033[H\033[2J")
			printOverview(ov)
		}
		select {
		case <-tick.C:
			continue
		case <-sigCh:
			return nil
		}
	}
}

// fetchOverview loads all four resource lists plus the controller version in
// parallel. If any list call fails, the first error is returned so the caller
// can surface a clear "fetch overview: ..." message.
func fetchOverview() (*overview, error) {
	client := NewAPIClient()

	var (
		workers  workerListResp
		teams    teamListResp
		managers managerListResp
		humans   humanListResp
		version  versionResp
		mu       sync.Mutex
		errs     []error
	)
	addErr := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		if err := client.DoJSON(context.Background(), "GET", "/api/v1/workers", nil, &workers); err != nil {
			addErr(err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := client.DoJSON(context.Background(), "GET", "/api/v1/teams", nil, &teams); err != nil {
			addErr(err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := client.DoJSON(context.Background(), "GET", "/api/v1/managers", nil, &managers); err != nil {
			addErr(err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := client.DoJSON(context.Background(), "GET", "/api/v1/humans", nil, &humans); err != nil {
			addErr(err)
		}
	}()
	go func() {
		defer wg.Done()
		// Version is best-effort: the controller currently hardcodes "dev" and
		// a 404/500 must not block the rest of the overview.
		if err := client.DoJSON(context.Background(), "GET", "/api/v1/version", nil, &version); err != nil {
			version = versionResp{KubeMode: "unknown"}
		}
	}()
	wg.Wait()

	if len(errs) > 0 {
		return nil, errs[0]
	}

	// Sort each list so non-Ready rows come first then alphabetically. This
	// affects both the table display and the JSON output (so script consumers
	// see the same urgent-first ordering).
	workersSorted := sortByHealth(workers.Workers,
		func(w workerResp) string { return w.Name },
		func(w workerResp) string { return or(w.Phase, "Pending") },
	)
	teamsSorted := sortByHealth(teams.Teams,
		func(t teamResp) string { return t.Name },
		func(t teamResp) string { return or(t.Phase, "Pending") },
	)
	managersSorted := sortByHealth(managers.Managers,
		func(m managerResp) string { return m.Name },
		func(m managerResp) string { return or(m.Phase, "Pending") },
	)
	humansSorted := sortByHealth(humans.Humans,
		func(h humanResp) string { return h.Name },
		func(h humanResp) string { return or(h.Phase, "Pending") },
	)

	return &overview{
		Mode:          version.KubeMode,
		Controller:    version.Controller,
		Workers:       workersSorted,
		Teams:         teamsSorted,
		Managers:      managersSorted,
		Humans:        humansSorted,
		WorkerCounts:  phaseCounts(phasesFromWorkers(workers.Workers)),
		TeamCounts:    phaseCounts(phasesFromTeams(teams.Teams)),
		ManagerCounts: phaseCounts(phasesFromManagers(managers.Managers)),
		HumanCounts:   phaseCounts(phasesFromHumans(humans.Humans)),
	}, nil
}

// printOverview renders the four resource tables plus a one-line Tip footer
// pointing the user at the next thing to look at.
func printOverview(o *overview) {
	fmt.Printf("Mode:       %s\n", or(o.Mode, "unknown"))
	// The controller currently hardcodes "dev" for the version string; only
	// show it when an actual release tag is set, otherwise the line is noise.
	if o.Controller != "" && o.Controller != "dev" {
		fmt.Printf("Controller: %s\n", o.Controller)
	}
	fmt.Println()

	printResourceTable(
		"Workers",
		o.Workers,
		[]string{"NAME", "PHASE", "STATE", "RUNTIME", "MODEL"},
		func(w workerResp) []string {
			return []string{
				w.Name,
				or(w.Phase, "Pending"),
				or(w.ContainerState, "unknown"),
				or(w.Runtime, "openclaw"),
				or(w.Model, "-"),
			}
		},
		func(w workerResp) string { return w.Name },
		func(w workerResp) string { return or(w.Phase, "Pending") },
	)

	printResourceTable(
		"Teams",
		o.Teams,
		[]string{"NAME", "PHASE", "LEADER", "READY"},
		func(t teamResp) []string {
			return []string{
				t.Name,
				or(t.Phase, "Pending"),
				t.LeaderName,
				fmt.Sprintf("%d/%d", t.ReadyWorkers, t.TotalWorkers),
			}
		},
		func(t teamResp) string { return t.Name },
		func(t teamResp) string { return or(t.Phase, "Pending") },
	)

	printResourceTable(
		"Managers",
		o.Managers,
		[]string{"NAME", "PHASE", "RUNTIME", "MODEL"},
		func(m managerResp) []string {
			return []string{
				m.Name,
				or(m.Phase, "Pending"),
				or(m.Runtime, "openclaw"),
				or(m.Model, "-"),
			}
		},
		func(m managerResp) string { return m.Name },
		func(m managerResp) string { return or(m.Phase, "Pending") },
	)

	printResourceTable(
		"Humans",
		o.Humans,
		[]string{"NAME", "PHASE", "DISPLAY-NAME"},
		func(h humanResp) []string {
			return []string{
				h.Name,
				or(h.Phase, "Pending"),
				or(h.DisplayName, "-"),
			}
		},
		func(h humanResp) string { return h.Name },
		func(h humanResp) string { return or(h.Phase, "Pending") },
	)

	tip := pickTip(o.Workers, o.Teams, o.Managers, o.Humans)
	if tip != "" {
		fmt.Println()
		fmt.Println("Tip:", tip)
	}
}

// printResourceTable renders one resource type with a header line that
// includes the phase summary, and the table itself. Rows are sorted so
// non-Ready rows come first, then alphabetically by name. Truncates to
// statusMaxRows with a footer so the overview stays on one screen.
func printResourceTable[T any](
	title string,
	items []T,
	headers []string,
	rowFn func(T) []string,
	nameFn func(T) string,
	phaseFn func(T) string,
) {
	phases := make([]string, 0, len(items))
	for _, item := range items {
		phases = append(phases, phaseFn(item))
	}
	fmt.Printf("%s (%s)\n", title, phaseSummary(phases))
	if len(items) == 0 {
		fmt.Println("  (none)")
		fmt.Println()
		return
	}
	sorted := sortByHealth(items, nameFn, phaseFn)
	shown := sorted
	truncated := 0
	if len(sorted) > statusMaxRows {
		shown = sorted[:statusMaxRows]
		truncated = len(sorted) - statusMaxRows
	}
	rows := make([][]string, len(shown))
	for i, item := range shown {
		rows[i] = rowFn(item)
	}
	printTable(headers, rows)
	if truncated > 0 {
		fmt.Printf("  ... and %d more. Run 'hiclaw get <resource>' to see all.\n", truncated)
	}
	fmt.Println()
}

// phaseSummary formats a slice of phases as "N total, X Ready, Y Failed, ..."
// with a stable order: Ready, Failed, Pending, then any other phases in
// alphabetical order. Empty phases are bucketed as Pending so the summary
// matches what the user sees in the table.
func phaseSummary(phases []string) string {
	if len(phases) == 0 {
		return "0 total"
	}
	counts := map[string]int{}
	for _, p := range phases {
		if p == "" {
			p = "Pending"
		}
		counts[p]++
	}
	parts := []string{fmt.Sprintf("%d total", len(phases))}
	for _, key := range []string{"Ready", "Failed", "Pending"} {
		if counts[key] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[key], key))
		}
	}
	others := make([]string, 0, len(counts))
	for k := range counts {
		if k != "Ready" && k != "Failed" && k != "Pending" {
			others = append(others, k)
		}
	}
	sort.Strings(others)
	for _, k := range others {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return strings.Join(parts, ", ")
}

// phaseCounts returns a map of phase -> count, bucketing empty phases as
// Pending. Used for the JSON output so consumers don't have to recompute.
func phaseCounts(phases []string) map[string]int {
	counts := map[string]int{}
	for _, p := range phases {
		if p == "" {
			p = "Pending"
		}
		counts[p]++
	}
	return counts
}

// sortByHealth sorts items so non-Ready rows come first, then alphabetically
// by name. Stable so an equal health/order pair preserves the original order.
func sortByHealth[T any](items []T, nameFn func(T) string, phaseFn func(T) string) []T {
	sorted := make([]T, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		iReady := phaseFn(sorted[i]) == "Ready"
		jReady := phaseFn(sorted[j]) == "Ready"
		if iReady != jReady {
			return !iReady
		}
		return nameFn(sorted[i]) < nameFn(sorted[j])
	})
	return sorted
}

// pickTip returns the highest-priority next-step hint for the user. Priority:
//   - any Failed resource across all types → suggest inspecting it
//   - any still-provisioning resource → suggest waiting
//   - otherwise → "all resources healthy"
func pickTip(workers []workerResp, teams []teamResp, managers []managerResp, humans []humanResp) string {
	for _, w := range workers {
		if w.Phase == "Failed" {
			return fmt.Sprintf("worker '%s' failed. Run `hiclaw get workers %s -o json` for details.", w.Name, w.Name)
		}
	}
	for _, t := range teams {
		if t.Phase == "Failed" {
			return fmt.Sprintf("team '%s' failed. Run `hiclaw get teams %s -o json` for details.", t.Name, t.Name)
		}
	}
	for _, m := range managers {
		if m.Phase == "Failed" {
			return fmt.Sprintf("manager '%s' failed. Run `hiclaw get managers %s -o json` for details.", m.Name, m.Name)
		}
	}
	for _, h := range humans {
		if h.Phase == "Failed" {
			return fmt.Sprintf("human '%s' failed. Run `hiclaw get humans %s -o json` for details.", h.Name, h.Name)
		}
	}

	if hasPending(workers) || hasPendingTeams(teams) || hasPendingManagers(managers) || hasPendingHumans(humans) {
		return "some resources are still provisioning. Re-run `hiclaw status` in a few seconds."
	}
	return "all resources healthy."
}

// hasPending / hasPendingTeams / hasPendingManagers / hasPendingHumans return
// true if any resource has a phase that is neither Ready nor Failed nor
// empty. Used to decide whether to nudge the user to wait.
func hasPending(ws []workerResp) bool {
	for _, w := range ws {
		if w.Phase != "" && w.Phase != "Ready" && w.Phase != "Failed" {
			return true
		}
	}
	return false
}
func hasPendingTeams(ts []teamResp) bool {
	for _, t := range ts {
		if t.Phase != "" && t.Phase != "Ready" && t.Phase != "Failed" {
			return true
		}
	}
	return false
}
func hasPendingManagers(ms []managerResp) bool {
	for _, m := range ms {
		if m.Phase != "" && m.Phase != "Ready" && m.Phase != "Failed" {
			return true
		}
	}
	return false
}
func hasPendingHumans(hs []humanResp) bool {
	for _, h := range hs {
		if h.Phase != "" && h.Phase != "Ready" && h.Phase != "Failed" {
			return true
		}
	}
	return false
}

// phasesFromWorkers / phasesFromTeams / phasesFromManagers / phasesFromHumans
// bucket empty phases as Pending so phaseSummary and phaseCounts get a clean
// input.
func phasesFromWorkers(items []workerResp) []string {
	out := make([]string, 0, len(items))
	for _, w := range items {
		out = append(out, or(w.Phase, "Pending"))
	}
	return out
}
func phasesFromTeams(items []teamResp) []string {
	out := make([]string, 0, len(items))
	for _, t := range items {
		out = append(out, or(t.Phase, "Pending"))
	}
	return out
}
func phasesFromManagers(items []managerResp) []string {
	out := make([]string, 0, len(items))
	for _, m := range items {
		out = append(out, or(m.Phase, "Pending"))
	}
	return out
}
func phasesFromHumans(items []humanResp) []string {
	out := make([]string, 0, len(items))
	for _, h := range items {
		out = append(out, or(h.Phase, "Pending"))
	}
	return out
}

// ---------------------------------------------------------------------------
// Response types (lightweight, no K8s dependency)
// ---------------------------------------------------------------------------

type versionResp struct {
	Controller string `json:"controller"`
	KubeMode   string `json:"kubeMode"`
}
