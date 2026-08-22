package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"regexp"
)

var (
	// URL patterns for POST/DELETE allowlist
	containerAction = regexp.MustCompile(`^(/v[\d.]+)?/containers/[a-zA-Z0-9_.-]+/(start|stop|kill|restart|wait|resize|attach|logs)$`)
	containerExec   = regexp.MustCompile(`^(/v[\d.]+)?/containers/[a-zA-Z0-9_.-]+/exec$`)
	containerCreate = regexp.MustCompile(`^(/v[\d.]+)?/containers/create$`)
	containerDelete = regexp.MustCompile(`^(/v[\d.]+)?/containers/[a-zA-Z0-9_.-]+$`)
	execStart       = regexp.MustCompile(`^(/v[\d.]+)?/exec/[a-zA-Z0-9]+/(start|resize|json)$`)
	imageCreate     = regexp.MustCompile(`^(/v[\d.]+)?/images/create$`)
)

func main() {
	socketPath := os.Getenv("HICLAW_PROXY_SOCKET")
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}

	listenAddr := os.Getenv("HICLAW_PROXY_LISTEN")
	if listenAddr == "" {
		listenAddr = ":2375"
	}

	validator := NewSecurityValidator()

	transport := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "localhost"
		},
		Transport: transport,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// GET requests are read-only, always allow
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			proxy.ServeHTTP(w, r)
			return
		}

		// POST/DELETE allowlist
		switch {
		case r.Method == http.MethodPost && containerCreate.MatchString(path):
			handleContainerCreate(w, r, proxy, validator)
			return

		case r.Method == http.MethodPost && containerAction.MatchString(path):
			// start/stop/kill/restart/wait/resize/attach/logs — allow
		case r.Method == http.MethodPost && containerExec.MatchString(path):
			// exec create — allow
		case r.Method == http.MethodPost && execStart.MatchString(path):
			// exec start — allow
		case r.Method == http.MethodPost && imageCreate.MatchString(path):
			// image pull — allow
		case r.Method == http.MethodDelete && containerDelete.MatchString(path):
			// container remove — allow

		default:
			log.Printf("[DENIED] %s %s", r.Method, r.URL.String())
			http.Error(w, fmt.Sprintf(`{"message":"hiclaw-docker-proxy: %s %s is not allowed"}`, r.Method, path), http.StatusForbidden)
			return
		}

		proxy.ServeHTTP(w, r)
	})

	log.Printf("hiclaw-docker-proxy listening on %s, backend: %s", listenAddr, socketPath)
	if len(validator.AllowedRegistries) > 0 {
		log.Printf("Allowed registries: %v", validator.AllowedRegistries)
	}
	if err := http.ListenAndServe(listenAddr, handler); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func handleContainerCreate(w http.ResponseWriter, r *http.Request, proxy *httputil.ReverseProxy, v *SecurityValidator) {
	// Read body
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, `{"message":"hiclaw-docker-proxy: failed to read request body"}`, http.StatusBadRequest)
		return
	}

	// Parse container name from query param
	containerName := r.URL.Query().Get("name")

	// Parse request
	var req ContainerCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"message":"hiclaw-docker-proxy: invalid JSON in request body"}`, http.StatusBadRequest)
		return
	}

	// Validate
	if err := v.ValidateContainerCreate(req, containerName); err != nil {
		log.Printf("[BLOCKED] POST /containers/create name=%s: %s", containerName, err)
		msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("hiclaw-docker-proxy: %s", err)})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write(msg)
		return
	}

	log.Printf("[ALLOWED] POST /containers/create name=%s image=%s", containerName, req.Image)

	// Restore body and forward
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	proxy.ServeHTTP(w, r)
}
