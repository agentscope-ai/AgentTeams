package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hiclaw/hiclaw-controller/internal/executor"
	"github.com/spf13/cobra"
)

// Backend abstraction: embedded mode uses MinIO (mc), incluster mode uses client-go
var kubeMode string

func init() {
	kubeMode = os.Getenv("HICLAW_KUBE_MODE")
	if kubeMode == "" {
		kubeMode = "embedded"
	}
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "hiclaw",
		Short: "HiClaw declarative resource management CLI",
	}

	rootCmd.AddCommand(applyCmd())
	rootCmd.AddCommand(getCmd())
	rootCmd.AddCommand(deleteCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// --- MinIO helpers (embedded mode) ---

func mcExec(args ...string) (string, error) {
	cmd := exec.Command("mc", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func storagePrefix() string {
	prefix := os.Getenv("HICLAW_STORAGE_PREFIX")
	if prefix == "" {
		prefix = "hiclaw/hiclaw-storage"
	}
	return prefix
}

func configPath(kind, name string) string {
	return fmt.Sprintf("%s/hiclaw-config/%ss/%s.yaml", storagePrefix(), kind, name)
}

func configDir(kind string) string {
	return fmt.Sprintf("%s/hiclaw-config/%ss/", storagePrefix(), kind)
}

// --- apply (parent command with subcommands) ---

func applyCmd() *cobra.Command {
	var files []string
	var prune bool
	var dryRun bool
	var yes bool

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply resource configuration",
		Long: `Apply creates or updates resources.

  hiclaw apply -f resource.yaml              # from YAML file
  hiclaw apply -f resource.yaml --prune      # full sync (delete extras)
  hiclaw apply worker --name alice --zip w.zip
  hiclaw apply worker --name alice --package nacos://inst/ns/spec/v1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// If -f is provided, run generic YAML apply
			if len(files) > 0 {
				resources, err := loadResources(files)
				if err != nil {
					return err
				}

				if dryRun {
					fmt.Println("Dry-run mode: showing planned changes")
					fmt.Println()
				}

				if kubeMode == "incluster" {
					return applyInCluster(resources, prune, dryRun)
				}
				return applyEmbedded(resources, prune, dryRun, yes)
			}

			// No -f and no subcommand → show help
			return cmd.Help()
		},
	}

	cmd.Flags().StringArrayVarP(&files, "file", "f", nil, "YAML resource file(s)")
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete resources not in YAML")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show changes without applying")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip delete confirmation")

	// Add resource-specific subcommands
	cmd.AddCommand(applyWorkerCmd())

	return cmd
}

// --- apply worker ---

func applyWorkerCmd() *cobra.Command {
	var name string
	var model string
	var zipFile string
	var packageURI string
	var skills string
	var mcpServers string
	var runtime string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Apply a Worker resource",
		Long: `Create or update a Worker from CLI parameters.

  hiclaw apply worker --name alice --zip worker.zip
  hiclaw apply worker --name alice --model claude-sonnet-4-6 --package nacos://inst/ns/spec/v1
  hiclaw apply worker --name bob --model qwen3.5-plus
  hiclaw apply worker --name charlie --model gpt-5-mini --skills github-operations --mcp-servers github`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}

			// --zip: legacy ZIP import (upload ZIP + generate YAML from manifest)
			if zipFile != "" {
				return applyZip(zipFile, name, dryRun)
			}

			// Generate Worker YAML from CLI params
			if model == "" {
				model = "qwen3.5-plus"
			}

			return applyWorkerFromParams(name, model, packageURI, skills, mcpServers, runtime, dryRun)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (required)")
	cmd.Flags().StringVar(&model, "model", "", "LLM model ID (default: qwen3.5-plus)")
	cmd.Flags().StringVar(&zipFile, "zip", "", "Local ZIP package (manifest.json)")
	cmd.Flags().StringVar(&packageURI, "package", "", "Remote package URI (nacos://, http://, oss://)")
	cmd.Flags().StringVar(&skills, "skills", "", "Comma-separated built-in skills")
	cmd.Flags().StringVar(&mcpServers, "mcp-servers", "", "Comma-separated MCP servers")
	cmd.Flags().StringVar(&runtime, "runtime", "openclaw", "Agent runtime (openclaw|copaw)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show changes without applying")

	return cmd
}

// applyWorkerFromParams generates a Worker YAML from CLI params and writes to MinIO
func applyWorkerFromParams(name, model, packageURI, skills, mcpServers, runtime string, dryRun bool) error {
	// Preflight: validate nacos:// URI before persisting
	if strings.HasPrefix(packageURI, "nacos://") {
		fmt.Printf("  Validating nacos URI: %s\n", packageURI)
		if err := executor.ValidateNacosURI(context.Background(), packageURI); err != nil {
			return err
		}
	}

	// Build YAML
	var specLines []string
	specLines = append(specLines, fmt.Sprintf("  model: %s", model))
	if runtime != "" && runtime != "openclaw" {
		specLines = append(specLines, fmt.Sprintf("  runtime: %s", runtime))
	}
	if packageURI != "" {
		specLines = append(specLines, fmt.Sprintf("  package: %s", packageURI))
	}
	if skills != "" {
		specLines = append(specLines, "  skills:")
		for _, s := range strings.Split(skills, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				specLines = append(specLines, fmt.Sprintf("    - %s", s))
			}
		}
	}
	if mcpServers != "" {
		specLines = append(specLines, "  mcpServers:")
		for _, m := range strings.Split(mcpServers, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				specLines = append(specLines, fmt.Sprintf("    - %s", m))
			}
		}
	}

	yamlContent := fmt.Sprintf(`apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: %s
spec:
%s
`, name, strings.Join(specLines, "\n"))

	if dryRun {
		fmt.Printf("Would apply Worker/%s:\n%s", name, yamlContent)
		return nil
	}

	dest := configPath("worker", name)

	// Check if exists
	_, existErr := mcExec("stat", dest)
	action := "created"
	if existErr == nil {
		action = "configured"
		fmt.Printf("  WARNING: worker/%s already exists. This update will:\n", name)
		fmt.Printf("    - Overwrite all config (model, openclaw.json, SOUL.md)\n")
		fmt.Printf("    - Skills: merged (existing updated, new added, old kept)\n")
		fmt.Printf("    - Memory: preserved (MEMORY.md and memory/ NOT overwritten)\n")
	}

	tmpFile, err := writeTempYAML(yamlContent)
	if err != nil {
		return fmt.Errorf("failed to write temp YAML: %w", err)
	}
	defer os.Remove(tmpFile)

	if _, err := mcExec("cp", tmpFile, dest); err != nil {
		return fmt.Errorf("failed to upload worker/%s to MinIO: %w", name, err)
	}
	fmt.Printf("  worker/%s %s\n", name, action)
	return nil
}

// applyEmbedded writes YAML files to MinIO hiclaw-config/{kind}s/{name}.yaml
func applyEmbedded(resources []resource, prune, dryRun, yes bool) error {
	applied := map[string]map[string]bool{
		"worker": {},
		"team":   {},
		"human":  {},
	}

	// Apply order: Team → Worker → Human
	ordered := orderForApply(resources)

	// Preflight: validate all nacos:// package URIs before applying anything
	for _, r := range ordered {
		if strings.ToLower(r.Kind) != "worker" {
			continue
		}
		pkg := extractPackageField(r.Raw)
		if strings.HasPrefix(pkg, "nacos://") {
			fmt.Printf("  Validating nacos URI for worker/%s: %s\n", r.Name, pkg)
			if err := executor.ValidateNacosURI(context.Background(), pkg); err != nil {
				return err
			}
		}
	}

	for _, r := range ordered {
		kind := strings.ToLower(r.Kind)
		dest := configPath(kind, r.Name)

		// Check if resource already exists
		_, existErr := mcExec("stat", dest)
		action := "created"
		if existErr == nil {
			action = "configured"
			if kind == "worker" || kind == "team" {
				fmt.Printf("  WARNING: %s/%s already exists. This update will:\n", r.Kind, r.Name)
				fmt.Printf("    - Overwrite all config (model, openclaw.json, SOUL.md)\n")
				fmt.Printf("    - Skills: merged (existing updated, new added, old kept)\n")
				fmt.Printf("    - Memory: preserved (MEMORY.md and memory/ NOT overwritten)\n")
			}
		}

		if dryRun {
			fmt.Printf("  %s/%s → %s (%s, dry-run)\n", r.Kind, r.Name, dest, action)
		} else {
			// Write YAML to temp file, then mc cp to MinIO
			tmpFile, err := writeTempYAML(r.Raw)
			if err != nil {
				return fmt.Errorf("failed to write temp file for %s/%s: %w", r.Kind, r.Name, err)
			}
			defer os.Remove(tmpFile)

			if _, err := mcExec("cp", tmpFile, dest); err != nil {
				return fmt.Errorf("failed to upload %s/%s to MinIO: %w", r.Kind, r.Name, err)
			}
			fmt.Printf("  %s/%s %s\n", r.Kind, r.Name, action)
		}

		if applied[kind] != nil {
			applied[kind][r.Name] = true
		}
	}

	// Prune: delete MinIO files not in YAML
	if prune {
		deleted := 0
		for _, kind := range []string{"human", "worker", "team"} { // delete order: Human → Worker → Team
			existing, err := listMinIOResources(kind)
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: failed to list %ss from MinIO: %v\n", kind, err)
				continue
			}
			for _, name := range existing {
				if !applied[kind][name] {
					path := configPath(kind, name)
					if dryRun {
						fmt.Printf("  %s/%s would be deleted (dry-run)\n", kind, name)
						deleted++
					} else {
						if !yes {
							fmt.Printf("  Delete %s/%s? [y/N] ", kind, name)
							reader := bufio.NewReader(os.Stdin)
							answer, _ := reader.ReadString('\n')
							if strings.TrimSpace(strings.ToLower(answer)) != "y" {
								continue
							}
						}
						if _, err := mcExec("rm", path); err != nil {
							fmt.Fprintf(os.Stderr, "WARNING: failed to delete %s: %v\n", path, err)
						} else {
							fmt.Printf("  %s/%s deleted\n", kind, name)
							deleted++
						}
					}
				}
			}
		}
		if deleted > 0 {
			fmt.Printf("\n%d resource(s) pruned\n", deleted)
		}
	}

	return nil
}

// applyInCluster uses client-go to apply resources to K8s API Server
func applyInCluster(resources []resource, prune, dryRun bool) error {
	// TODO: implement client-go apply for incluster mode
	fmt.Println("incluster mode: not yet implemented")
	return nil
}

// listMinIOResources lists resource names from MinIO hiclaw-config/{kind}s/
func listMinIOResources(kind string) ([]string, error) {
	dir := configDir(kind)
	out, err := mcExec("ls", "--json", dir)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// mc ls --json outputs {"key":"name.yaml",...}
		if idx := strings.Index(line, `"key":"`); idx >= 0 {
			rest := line[idx+7:]
			if end := strings.Index(rest, `"`); end >= 0 {
				filename := rest[:end]
				if (strings.HasSuffix(filename, ".yaml") || strings.HasSuffix(filename, ".yml")) && filename != ".gitkeep" {
					name := strings.TrimSuffix(strings.TrimSuffix(filename, ".yaml"), ".yml")
					names = append(names, name)
				}
			}
		}
	}
	return names, nil
}

// --- get ---

func getCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <resource-type> [name]",
		Short: "Display resources",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resourceType := args[0]
			name := ""
			if len(args) > 1 {
				name = args[1]
			}

			kind := strings.TrimSuffix(resourceType, "s")
			switch kind {
			case "worker", "team", "human":
			default:
				return fmt.Errorf("unknown resource type %q (use: workers, teams, humans)", resourceType)
			}

			if kubeMode == "incluster" {
				// TODO: client-go list/get
				fmt.Println("incluster mode: not yet implemented")
				return nil
			}

			if name != "" {
				// Get single resource from MinIO
				path := configPath(kind, name)
				out, err := mcExec("cat", path)
				if err != nil {
					return fmt.Errorf("%s/%s not found", kind, name)
				}
				fmt.Println(out)
			} else {
				// List all resources
				names, err := listMinIOResources(kind)
				if err != nil {
					return fmt.Errorf("failed to list %ss: %w", kind, err)
				}
				if len(names) == 0 {
					fmt.Printf("No %ss found.\n", kind)
					return nil
				}
				for _, n := range names {
					fmt.Printf("  %s/%s\n", kind, n)
				}
				fmt.Printf("Total: %d %s(s)\n", len(names), kind)
			}
			return nil
		},
	}
	return cmd
}

// --- delete ---

func deleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <resource-type> <name>",
		Short: "Delete a resource",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := strings.TrimSuffix(args[0], "s")
			name := args[1]

			switch kind {
			case "worker", "team", "human":
			default:
				return fmt.Errorf("unknown resource type %q", args[0])
			}

			if kubeMode == "incluster" {
				// TODO: client-go delete
				fmt.Println("incluster mode: not yet implemented")
				return nil
			}

			path := configPath(kind, name)
			if _, err := mcExec("rm", path); err != nil {
				return fmt.Errorf("failed to delete %s/%s: %w", kind, name, err)
			}
			fmt.Printf("%s/%s deleted\n", kind, name)
			return nil
		},
	}
	return cmd
}

// --- YAML parsing ---

type resource struct {
	APIVersion string
	Kind       string
	Name       string
	Raw        string
}

func loadResources(files []string) ([]resource, error) {
	var resources []resource

	for _, f := range files {
		data, err := readFile(f)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", f, err)
		}

		docs := splitYAMLDocs(string(data))
		for _, doc := range docs {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}

			r := resource{Raw: doc}
			inMetadata := false
			for _, rawLine := range strings.Split(doc, "\n") {
				line := strings.TrimSpace(rawLine)
				if strings.HasPrefix(line, "apiVersion:") {
					r.APIVersion = strings.TrimSpace(strings.TrimPrefix(line, "apiVersion:"))
				}
				if strings.HasPrefix(line, "kind:") {
					r.Kind = strings.TrimSpace(strings.TrimPrefix(line, "kind:"))
				}
				if line == "metadata:" {
					inMetadata = true
					continue
				}
				if inMetadata && len(rawLine) > 0 && rawLine[0] != ' ' && rawLine[0] != '\t' {
					inMetadata = false
				}
				if inMetadata && strings.HasPrefix(line, "name:") && r.Name == "" {
					r.Name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
				}
			}

			if r.Kind == "" || r.Name == "" {
				continue
			}
			resources = append(resources, r)
		}
	}

	return resources, nil
}

// orderForApply sorts resources: Team first, then Worker, then Human
func orderForApply(resources []resource) []resource {
	var teams, workers, humans, other []resource
	for _, r := range resources {
		switch r.Kind {
		case "Team":
			teams = append(teams, r)
		case "Worker":
			workers = append(workers, r)
		case "Human":
			humans = append(humans, r)
		default:
			other = append(other, r)
		}
	}
	result := make([]resource, 0, len(resources))
	result = append(result, teams...)
	result = append(result, workers...)
	result = append(result, humans...)
	result = append(result, other...)
	return result
}

func readFile(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func splitYAMLDocs(content string) []string {
	var docs []string
	current := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "---" {
			if strings.TrimSpace(current) != "" {
				docs = append(docs, current)
			}
			current = ""
			continue
		}
		current += line + "\n"
	}
	if strings.TrimSpace(current) != "" {
		docs = append(docs, current)
	}
	return docs
}

func writeTempYAML(content string) (string, error) {
	f, err := os.CreateTemp("", "hiclaw-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	f.Close()
	return f.Name(), nil
}

// --- ZIP import ---

// applyZip converts a legacy ZIP package (manifest.json) to CRD YAML,
// uploads the ZIP to MinIO hiclaw-config/packages/, and writes the YAML
// to MinIO hiclaw-config/{kind}s/{name}.yaml.
func applyZip(zipPath string, name string, dryRun bool) error {
	// 1. Extract ZIP to temp dir
	tmpDir, err := os.MkdirTemp("", "hiclaw-zip-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("unzip", "-q", zipPath, "-d", tmpDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to extract ZIP: %s: %w", string(out), err)
	}

	// 2. Read manifest.json
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("manifest.json not found in ZIP: %w", err)
	}

	manifestType := jsonField(string(manifestData), "type")
	if manifestType == "" {
		manifestType = "worker"
	}

	// 3. Convert manifest to CRD YAML
	var yamlContent string
	var kind string

	model := jsonField(string(manifestData), "model")
	if model == "" {
		model = "qwen3.5-plus"
	}

	// Compute SHA256 of ZIP for content-addressable storage
	zipData, err := os.ReadFile(zipPath)
	if err != nil {
		return fmt.Errorf("failed to read ZIP for hashing: %w", err)
	}
	zipHash := fmt.Sprintf("%x", sha256.Sum256(zipData))[:16]
	packageFileName := fmt.Sprintf("%s-%s.zip", name, zipHash)

	switch manifestType {
	case "worker":
		kind = "worker"
		yamlContent = fmt.Sprintf(`apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: %s
spec:
  model: %s
  package: oss://hiclaw-config/packages/%s
`, name, model, packageFileName)

	case "team":
		kind = "team"
		yamlContent = fmt.Sprintf(`apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: %s
spec:
  leader:
    name: %s-lead
    package: oss://hiclaw-config/packages/%s
  workers: []
`, name, name, packageFileName)

	default:
		return fmt.Errorf("unsupported manifest type: %s", manifestType)
	}

	if dryRun {
		fmt.Printf("Would create %s/%s from ZIP:\n", kind, name)
		fmt.Println(yamlContent)
		return nil
	}

	// 4. Upload ZIP to MinIO hiclaw-config/packages/{name}-{md5}.zip
	packageDest := fmt.Sprintf("%s/hiclaw-config/packages/%s", storagePrefix(), packageFileName)
	if _, err := mcExec("cp", zipPath, packageDest); err != nil {
		return fmt.Errorf("failed to upload ZIP to MinIO: %w", err)
	}
	fmt.Printf("  Package uploaded: %s\n", packageDest)

	// 5. Check if resource already exists (for create vs update message)
	yamlDest := configPath(kind, name)
	_, existErr := mcExec("stat", yamlDest)
	action := "created"
	if existErr == nil {
		action = "updated"
		fmt.Printf("  WARNING: %s/%s already exists. This update will:\n", kind, name)
		fmt.Printf("    - Overwrite all config (model, openclaw.json, SOUL.md)\n")
		fmt.Printf("    - Skills: merged (existing updated, new added, old kept)\n")
		fmt.Printf("    - Memory: preserved (MEMORY.md and memory/ NOT overwritten)\n")
	}

	// 6. Write generated YAML to MinIO hiclaw-config/{kind}s/{name}.yaml
	tmpYAML, err := writeTempYAML(yamlContent)
	if err != nil {
		return fmt.Errorf("failed to write temp YAML: %w", err)
	}
	defer os.Remove(tmpYAML)

	if _, err := mcExec("cp", tmpYAML, yamlDest); err != nil {
		return fmt.Errorf("failed to upload YAML to MinIO: %w", err)
	}
	fmt.Printf("  %s/%s %s (from ZIP)\n", kind, name, action)

	return nil
}

// jsonField extracts a simple string field from JSON using jq.
// Handles both top-level and nested "worker.field" / "team.field" patterns.
func jsonField(jsonStr, field string) string {
	cmd := exec.Command("jq", "-r",
		fmt.Sprintf(`.%s // .worker.%s // .team.%s // ""`, field, field, field),
	)
	cmd.Stdin = strings.NewReader(jsonStr)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	result := strings.TrimSpace(string(out))
	if result == "null" {
		return ""
	}
	return result
}

// extractPackageField extracts the spec.package value from raw Worker YAML.
func extractPackageField(raw string) string {
	inSpec := false
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		// Detect top-level "spec:" section
		if trimmed == "spec:" {
			inSpec = true
			continue
		}
		// Left-aligned line (no leading space) exits spec section
		if inSpec && len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			inSpec = false
		}
		if inSpec && strings.HasPrefix(trimmed, "package:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "package:"))
		}
	}
	return ""
}
