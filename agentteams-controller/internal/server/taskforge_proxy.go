package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// taskforgeProxyTimeout bounds one upstream TaskForge round trip.
const taskforgeProxyTimeout = 30 * time.Second

var taskforgeHopByHopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "TE": true, "Trailers": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

var taskforgeForwardHeaders = []string{"Content-Type", "Accept", "X-Request-Id", internalRequestIDHeader}

func (a *internalAPI) proxyTaskForge(w http.ResponseWriter, r *http.Request) {
	if !a.taskforgeEnabled || a.taskforgeURL == "" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "taskforge proxy is not configured")
		return
	}
	suffix := r.PathValue("path")
	target := a.taskforgeURL + "/" + suffix
	if raw := r.URL.RawQuery; raw != "" {
		target += "?" + raw
	}
	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, internalMaxBodyBytes))
	if err != nil {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var body io.Reader
	if len(bodyBytes) > 0 {
		body = bytes.NewReader(bodyBytes)
	}
	ctx, cancel := context.WithTimeout(r.Context(), taskforgeProxyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, target, body)
	if err != nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "taskforge proxy: "+err.Error())
		return
	}
	for _, h := range taskforgeForwardHeaders {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Del("Authorization")
	if a.taskforgeToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.taskforgeToken)
	}
	resp, err := a.taskforgeClient.Do(req)
	if err != nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "taskforge upstream unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if taskforgeHopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
