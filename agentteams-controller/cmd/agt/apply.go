package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	sigyaml "sigs.k8s.io/yaml"
)

func applyCmd() *cobra.Command {
	var files []string

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply resource configuration (create or update)",
		Long: `Apply creates or updates resources declaratively.

  agt apply -f resource.yaml
  agt apply worker --name alice --zip worker.zip
  agt apply worker --name alice --model qwen3.6-plus`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(files) > 0 {
				return applyFromFiles(files)
			}
			return cmd.Help()
		},
	}

	cmd.Flags().StringArrayVarP(&files, "file", "f", nil, "YAML resource file(s)")
	cmd.AddCommand(applyWorkerSubCmd())

	return cmd
}

// ---------------------------------------------------------------------------
// apply -f <yaml>
// ---------------------------------------------------------------------------

type yamlResource struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   yamlMetadata           `json:"metadata"`
	Spec       map[string]interface{} `json:"spec"`
}

type yamlMetadata struct {
	Name string `json:"name"`
}

func applyFromFiles(files []string) error {
	client := NewAPIClient()

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}

		docs := splitYAMLDocs(string(data))
		for _, doc := range docs {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}

			var res yamlResource
			if err := sigyaml.Unmarshal([]byte(doc), &res); err != nil {
				return fmt.Errorf("parse YAML in %s: %w", f, err)
			}
			if res.Kind == "" || res.Metadata.Name == "" {
				continue
			}

			if err := applyOneResource(client, res); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyOneResource(client *APIClient, res yamlResource) error {
	kind := strings.ToLower(res.Kind)
	name := res.Metadata.Name

	// Build plural endpoint
	endpoint := "/api/v1/" + kind + "s"

	exists, err := client.ResourceExists(endpoint + "/" + name)
	if err != nil {
		return fmt.Errorf("check %s/%s: %w", kind, name, err)
	}

	var resp map[string]interface{}
	if exists {
		// PUT update — send only spec fields (no name in body for PUT)
		updateBody := buildApplyBody(res, false)
		if err := client.DoJSON("PUT", endpoint+"/"+name, updateBody, &resp); err != nil {
			return fmt.Errorf("update %s/%s: %w", kind, name, err)
		}
		fmt.Printf("  %s/%s configured\n", kind, name)
	} else {
		body := buildApplyBody(res, true)
		if err := client.DoJSON("POST", endpoint, body, &resp); err != nil {
			return fmt.Errorf("create %s/%s: %w", kind, name, err)
		}
		fmt.Printf("  %s/%s created\n", kind, name)
	}

	return nil
}

func buildApplyBody(res yamlResource, includeName bool) map[string]interface{} {
	body := make(map[string]interface{})
	if includeName {
		body["name"] = res.Metadata.Name
	}
	for k, v := range res.Spec {
		body[k] = v
	}
	return body
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

// ---------------------------------------------------------------------------
// apply worker
// ---------------------------------------------------------------------------

func applyWorkerSubCmd() *cobra.Command {
	var (
		name       string
		model      string
		zipFile    string
		runtime    string
		image      string
		identity   string
		soul       string
		soulFile   string
		skills     string
		packageURI string
		expose     string
		team       string
		role       string
	)

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Apply a Worker resource (create or update)",
		Long: `Create or update a Worker from CLI parameters or a ZIP package.

  agt apply worker --name alice --zip worker.zip
  agt apply worker --name alice --model qwen3.6-plus
  agt apply worker --name bob --model claude-sonnet-4-6 --skills github-operations`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if err := validateWorkerName(name); err != nil {
				return err
			}

			if zipFile != "" {
				return applyWorkerZip(name, zipFile, runtime)
			}

			return applyWorkerParams(name, model, runtime, image, identity, soul, soulFile,
				skills, packageURI, expose, team, role)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Worker name (required)")
	cmd.Flags().StringVar(&model, "model", "", "LLM model ID (default: $AGENTTEAMS_DEFAULT_MODEL, else qwen3.6-plus)")
	cmd.Flags().StringVar(&zipFile, "zip", "", "Local ZIP package (manifest.json)")
	cmd.Flags().StringVar(&runtime, "runtime", "", "Agent runtime (openclaw|copaw|hermes|openhuman)")
	cmd.Flags().StringVar(&image, "image", "", "Container image override")
	cmd.Flags().StringVar(&identity, "identity", "", "Worker identity description")
	cmd.Flags().StringVar(&soul, "soul", "", "Worker SOUL.md content (inline)")
	cmd.Flags().StringVar(&soulFile, "soul-file", "", "Path to SOUL.md file")
	cmd.Flags().StringVar(&skills, "skills", "", "Comma-separated built-in skills")
	cmd.Flags().StringVar(&packageURI, "package", "", "Package URI (nacos://[?authType=...], http://, oss://)")
	cmd.Flags().StringVar(&expose, "expose", "", "Comma-separated ports to expose")
	cmd.Flags().StringVar(&team, "team", "", "Team name")
	cmd.Flags().StringVar(&role, "role", "", "Role within team (team_leader|worker)")
	return cmd
}

// applyWorkerZip uploads a ZIP to the controller, then creates/updates the Worker.
//
// runtimeOverride wins over whatever the ZIP's manifest declares; both win over
// the controller's defaultRuntime() (which silently falls back to openclaw and
// hides cross-runtime test coverage gaps — see fix in this commit).
func applyWorkerZip(name, zipPath, runtimeOverride string) error {
	zipData, err := os.ReadFile(zipPath)
	if err != nil {
		return fmt.Errorf("read ZIP %s: %w", zipPath, err)
	}

	model, manifestRuntime := extractWorkerFieldsFromZip(zipData)
	if model == "" {
		model = defaultWorkerModel()
	}
	runtime := runtimeOverride
	if runtime == "" {
		runtime = manifestRuntime
	}

	client := NewAPIClient()

	// Upload ZIP → POST /api/v1/packages
	var pkgResp struct {
		PackageUri string `json:"packageUri"`
	}
	if err := client.DoMultipart("/api/v1/packages", "file", filepath.Base(zipPath), zipData,
		map[string]string{"name": name}, &pkgResp); err != nil {
		return fmt.Errorf("upload package: %w", err)
	}

	// Upsert Worker
	exists, err := client.ResourceExists("/api/v1/workers/" + name)
	if err != nil {
		return fmt.Errorf("check worker/%s: %w", name, err)
	}

	var resp map[string]interface{}
	if exists {
		updateBody := map[string]interface{}{
			"model":   model,
			"package": pkgResp.PackageUri,
		}
		setIfNotEmpty(updateBody, "runtime", runtime)
		if err := client.DoJSON("PUT", "/api/v1/workers/"+name, updateBody, &resp); err != nil {
			return fmt.Errorf("update worker/%s: %w", name, err)
		}
		fmt.Printf("  worker/%s updated\n", name)
	} else {
		createBody := map[string]interface{}{
			"name":    name,
			"model":   model,
			"package": pkgResp.PackageUri,
		}
		setIfNotEmpty(createBody, "runtime", runtime)
		if err := client.DoJSON("POST", "/api/v1/workers", createBody, &resp); err != nil {
			return fmt.Errorf("create worker/%s: %w", name, err)
		}
		fmt.Printf("  worker/%s created\n", name)
	}

	return nil
}

// applyWorkerParams creates or updates a Worker from CLI flags (upsert semantics).
func applyWorkerParams(name, model, runtime, image, identity, soul, soulFile,
	skills, packageURI, expose, team, role string) error {

	if model == "" {
		model = defaultWorkerModel()
	}
	if soulFile != "" {
		data, err := os.ReadFile(soulFile)
		if err != nil {
			return fmt.Errorf("read --soul-file %q: %w", soulFile, err)
		}
		soul = string(data)
	}
	if packageURI != "" {
		var err error
		packageURI, err = expandPackageURI(packageURI)
		if err != nil {
			return err
		}
	}

	client := NewAPIClient()

	exists, err := client.ResourceExists("/api/v1/workers/" + name)
	if err != nil {
		return fmt.Errorf("check worker/%s: %w", name, err)
	}

	req := map[string]interface{}{
		"model": model,
	}
	setIfNotEmpty(req, "runtime", runtime)
	setIfNotEmpty(req, "image", image)
	setIfNotEmpty(req, "identity", identity)
	setIfNotEmpty(req, "soul", soul)
	setIfNotEmpty(req, "package", packageURI)
	setIfNotEmpty(req, "team", team)
	setIfNotEmpty(req, "role", role)
	if skills != "" {
		req["skills"] = splitCSV(skills)
	}
	if expose != "" {
		req["expose"] = parseExposePorts(expose)
	}

	var resp map[string]interface{}
	if exists {
		if err := client.DoJSON("PUT", "/api/v1/workers/"+name, req, &resp); err != nil {
			return fmt.Errorf("update worker/%s: %w", name, err)
		}
		fmt.Printf("  worker/%s configured\n", name)
	} else {
		req["name"] = name
		if err := client.DoJSON("POST", "/api/v1/workers", req, &resp); err != nil {
			return fmt.Errorf("create worker/%s: %w", name, err)
		}
		fmt.Printf("  worker/%s created\n", name)
	}

	return nil
}

// ---------------------------------------------------------------------------
// ZIP manifest helpers
// ---------------------------------------------------------------------------

// extractWorkerFieldsFromZip reads manifest.json from the ZIP and extracts the
// model and runtime fields. Both top-level and `worker.<field>` placements are
// honored; the worker block takes precedence to match the documented schema in
// docs/import-worker.md.
//
// Either return value may be empty when the manifest does not declare it (or
// when the ZIP has no manifest at all). Callers are expected to fall back to
// their own defaults (model → defaultWorkerModel(), which prefers
// $AGENTTEAMS_DEFAULT_MODEL; runtime → server-side default).
func extractWorkerFieldsFromZip(zipData []byte) (model, runtime string) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return "", ""
	}

	for _, f := range r.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", ""
		}
		defer rc.Close()

		var manifest map[string]interface{}
		if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
			return "", ""
		}

		if m, ok := manifest["model"].(string); ok && m != "" {
			model = m
		}
		if rt, ok := manifest["runtime"].(string); ok && rt != "" {
			runtime = rt
		}
		if w, ok := manifest["worker"].(map[string]interface{}); ok {
			if m, ok := w["model"].(string); ok && m != "" {
				model = m
			}
			if rt, ok := w["runtime"].(string); ok && rt != "" {
				runtime = rt
			}
		}
		return model, runtime
	}
	return "", ""
}
