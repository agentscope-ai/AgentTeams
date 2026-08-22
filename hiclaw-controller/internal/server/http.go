package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// HTTPServer provides a REST API for cloud management integration.
// In embedded mode, it writes YAML to MinIO (controller picks up via file watcher).
// In incluster mode, it operates K8s API directly.
type HTTPServer struct {
	KubeMode      string // "embedded" or "incluster"
	StoragePrefix string // e.g. "hiclaw/hiclaw-storage"
	Addr          string
}

func NewHTTPServer(addr, kubeMode string) *HTTPServer {
	prefix := os.Getenv("HICLAW_STORAGE_PREFIX")
	if prefix == "" {
		prefix = "hiclaw/hiclaw-storage"
	}
	return &HTTPServer{
		KubeMode:      kubeMode,
		StoragePrefix: prefix,
		Addr:          addr,
	}
}

func (s *HTTPServer) Start() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/v1/apply", s.handleApply)
	mux.HandleFunc("/api/v1/workers", s.handleList("worker"))
	mux.HandleFunc("/api/v1/teams", s.handleList("team"))
	mux.HandleFunc("/api/v1/humans", s.handleList("human"))
	// Single resource: /api/v1/workers/{name}
	mux.HandleFunc("/api/v1/workers/", s.handleResource("worker"))
	mux.HandleFunc("/api/v1/teams/", s.handleResource("team"))
	mux.HandleFunc("/api/v1/humans/", s.handleResource("human"))

	logger := log.Log.WithName("http-server")
	logger.Info("starting HTTP API server", "addr", s.Addr, "mode", s.KubeMode)
	return http.ListenAndServe(s.Addr, mux)
}

func (s *HTTPServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

// handleApply accepts YAML body, splits multi-doc, writes each to MinIO.
func (s *HTTPServer) handleApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	prune := r.URL.Query().Get("prune") == "true"

	docs := splitYAMLDocs(string(body))
	applied := make(map[string]map[string]bool)
	applied["worker"] = map[string]bool{}
	applied["team"] = map[string]bool{}
	applied["human"] = map[string]bool{}

	var results []map[string]string

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		kind, name := extractKindName(doc)
		if kind == "" || name == "" {
			results = append(results, map[string]string{"error": "missing kind or name in document"})
			continue
		}

		kindLower := strings.ToLower(kind)
		dest := fmt.Sprintf("%s/hiclaw-config/%ss/%s.yaml", s.StoragePrefix, kindLower, name)

		if err := s.writeToMinIO(doc, dest); err != nil {
			results = append(results, map[string]string{
				"kind": kind, "name": name, "status": "error", "message": err.Error(),
			})
			continue
		}

		results = append(results, map[string]string{
			"kind": kind, "name": name, "status": "applied",
		})
		if applied[kindLower] != nil {
			applied[kindLower][name] = true
		}
	}

	// Prune: delete resources in MinIO not present in YAML
	if prune {
		for _, kind := range []string{"human", "worker", "team"} {
			existing := s.listMinIONames(kind)
			for _, name := range existing {
				if !applied[kind][name] {
					dest := fmt.Sprintf("%s/hiclaw-config/%ss/%s.yaml", s.StoragePrefix, kind, name)
					if err := mcRm(dest); err == nil {
						results = append(results, map[string]string{
							"kind": kind, "name": name, "status": "deleted",
						})
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"results": results,
	})
}

func (s *HTTPServer) handleList(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		names := s.listMinIONames(kind)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"kind":  kind,
			"items": names,
			"total": len(names),
		})
	}
}

func (s *HTTPServer) handleResource(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract name from URL: /api/v1/workers/alice → alice
		parts := strings.Split(strings.TrimRight(r.URL.Path, "/"), "/")
		if len(parts) == 0 {
			http.Error(w, "missing resource name", http.StatusBadRequest)
			return
		}
		name := parts[len(parts)-1]
		path := fmt.Sprintf("%s/hiclaw-config/%ss/%s.yaml", s.StoragePrefix, kind, name)

		switch r.Method {
		case http.MethodGet:
			out, err := mcCat(path)
			if err != nil {
				http.Error(w, fmt.Sprintf("%s/%s not found", kind, name), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/yaml")
			fmt.Fprint(w, out)

		case http.MethodDelete:
			if err := mcRm(path); err != nil {
				http.Error(w, fmt.Sprintf("failed to delete %s/%s: %v", kind, name, err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"kind": kind, "name": name, "status": "deleted",
			})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// --- MinIO helpers ---

func (s *HTTPServer) writeToMinIO(yamlContent, dest string) error {
	tmpFile, err := os.CreateTemp("", "hiclaw-api-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(yamlContent); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()

	cmd := exec.Command("mc", "cp", tmpFile.Name(), dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mc cp failed: %s: %w", string(out), err)
	}
	return nil
}

func (s *HTTPServer) listMinIONames(kind string) []string {
	dir := fmt.Sprintf("%s/hiclaw-config/%ss/", s.StoragePrefix, kind)
	cmd := exec.Command("mc", "ls", dir)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// mc ls output: "[date] [size] filename.yaml"
		parts := strings.Fields(line)
		if len(parts) < 1 {
			continue
		}
		filename := parts[len(parts)-1]
		if (strings.HasSuffix(filename, ".yaml") || strings.HasSuffix(filename, ".yml")) && filename != ".gitkeep" {
			name := strings.TrimSuffix(strings.TrimSuffix(filename, ".yaml"), ".yml")
			names = append(names, name)
		}
	}
	return names
}

func mcCat(path string) (string, error) {
	cmd := exec.Command("mc", "cat", path)
	out, err := cmd.Output()
	return string(out), err
}

func mcRm(path string) error {
	cmd := exec.Command("mc", "rm", path)
	_, err := cmd.CombinedOutput()
	return err
}

// --- YAML helpers ---

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

func extractKindName(doc string) (kind, name string) {
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "kind:") {
			kind = strings.TrimSpace(strings.TrimPrefix(line, "kind:"))
		}
		if strings.HasPrefix(line, "  name:") && name == "" {
			name = strings.TrimSpace(strings.TrimPrefix(line, "  name:"))
		}
	}
	return
}
