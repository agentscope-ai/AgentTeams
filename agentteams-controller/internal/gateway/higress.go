package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HigressClient implements Client for self-hosted Higress gateway.
type HigressClient struct {
	config Config
	http   *http.Client

	mu      sync.Mutex
	cookies []*http.Cookie

	aiRouteMu sync.Mutex
}

// NewHigressClient creates a gateway Client for Higress Console API.
func NewHigressClient(cfg Config, httpClient *http.Client) *HigressClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &HigressClient{config: cfg, http: httpClient}
}

// ensureSession logs in to Higress Console and caches the session cookie.
func (c *HigressClient) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.cookies) > 0 {
		return nil
	}

	// Initialize admin account on first boot (idempotent — succeeds if already initialized).
	// Higress Console requires /system/init before login works.
	initBody := fmt.Sprintf(`{"adminUser":{"name":%q,"password":%q,"displayName":%q}}`,
		c.config.AdminUser, c.config.AdminPassword, c.config.AdminUser)
	initReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.config.ConsoleURL+"/system/init",
		strings.NewReader(initBody))
	if err != nil {
		return err
	}
	initReq.Header.Set("Content-Type", "application/json")
	initResp, err := c.http.Do(initReq)
	if err != nil {
		return fmt.Errorf("higress init: %w", err)
	}
	initResp.Body.Close()

	// Steady state should use the configured password. Embedded all-in-one boot
	// may briefly win the race and initialize itself with the upstream default
	// admin/admin before AgentTeams reaches /system/init. In that bootstrap-only
	// case, recover with admin/admin once, converge the password, then re-login
	// with the configured credentials.
	if err := c.loginLocked(ctx, c.config.AdminUser, c.config.AdminPassword); err == nil {
		return nil
	} else if !c.config.AllowDefaultAdminFallback || c.config.AdminUser != "admin" || c.config.AdminPassword == "admin" {
		return err
	}

	if err := c.loginLocked(ctx, "admin", "admin"); err != nil {
		return fmt.Errorf("higress login with configured credentials failed; fallback admin/admin login also failed: %w", err)
	}

	if err := c.changePasswordLocked(ctx, "admin", c.config.AdminPassword); err != nil {
		return fmt.Errorf("higress default admin/admin login succeeded but password convergence failed: %w", err)
	}

	c.cookies = nil
	if err := c.loginLocked(ctx, c.config.AdminUser, c.config.AdminPassword); err != nil {
		return fmt.Errorf("higress password converged but relogin with configured credentials failed: %w", err)
	}
	return nil
}

func (c *HigressClient) loginLocked(ctx context.Context, username, password string) error {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.config.ConsoleURL+"/session/login",
		strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("higress login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		c.cookies = resp.Cookies()
		return nil
	}

	return fmt.Errorf("higress login failed for user %q: HTTP %d (check higress-console state/secret)", username, resp.StatusCode)
}

func (c *HigressClient) changePasswordLocked(ctx context.Context, oldPassword, newPassword string) error {
	body := fmt.Sprintf(`{"oldPassword":%q,"newPassword":%q}`, oldPassword, newPassword)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.config.ConsoleURL+"/user/changePassword",
		strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("higress change password: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("higress change password failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

func (c *HigressClient) EnsureConsumer(ctx context.Context, req ConsumerRequest) (*ConsumerResult, error) {
	body := map[string]interface{}{
		"name": req.Name,
		"credentials": []map[string]interface{}{
			{
				"type":   "key-auth",
				"source": "BEARER",
				"values": []string{req.CredentialKey},
			},
		},
	}

	_, statusCode, err := c.doJSON(ctx, http.MethodPost, "/v1/consumers", body)
	if err != nil {
		return nil, fmt.Errorf("ensure consumer %s: %w", req.Name, err)
	}

	status := "created"
	if statusCode == http.StatusConflict {
		status = "exists"
	} else if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return nil, fmt.Errorf("ensure consumer %s: HTTP %d", req.Name, statusCode)
	}

	return &ConsumerResult{
		Status: status,
		APIKey: req.CredentialKey,
	}, nil
}

func (c *HigressClient) DeleteConsumer(ctx context.Context, name string) error {
	_, statusCode, err := c.doJSON(ctx, http.MethodDelete, "/v1/consumers/"+name, nil)
	if err != nil {
		return fmt.Errorf("delete consumer %s: %w", name, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusNoContent && statusCode != http.StatusNotFound {
		return fmt.Errorf("delete consumer %s: HTTP %d", name, statusCode)
	}
	return nil
}

func (c *HigressClient) AuthorizeAIRoutes(ctx context.Context, consumerName string, modelAPIID string) error {
	return c.modifyAIRoutes(ctx, consumerName, modelAPIID, true)
}

func (c *HigressClient) DeauthorizeAIRoutes(ctx context.Context, consumerName string, modelAPIID string) error {
	return c.modifyAIRoutes(ctx, consumerName, modelAPIID, false)
}

// modifyAIRoutes adds or removes the consumer from AI routes' allowedConsumers.
// When providerFilter is non-empty, AuthorizeAIRoutes keeps the consumer only
// on matching routes and removes it from non-matching routes; DeauthorizeAIRoutes
// removes it only from matching routes. Empty providerFilter keeps the legacy
// all-route behavior.
func (c *HigressClient) modifyAIRoutes(ctx context.Context, consumerName string, providerFilter string, add bool) error {
	c.aiRouteMu.Lock()
	defer c.aiRouteMu.Unlock()

	respBody, statusCode, err := c.doJSON(ctx, http.MethodGet, "/v1/ai/routes", nil)
	if err != nil {
		return fmt.Errorf("list AI routes: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("list AI routes: HTTP %d", statusCode)
	}

	var listResp struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &listResp); err != nil {
		return fmt.Errorf("decode AI routes list: %w", err)
	}

	const maxRetries = 5

	var firstErr error
	recordErr := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}

	for _, raw := range listResp.Data {
		var routeInfo struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &routeInfo); err != nil || routeInfo.Name == "" {
			continue
		}

		var lastErr error
		for attempt := 0; attempt < maxRetries; attempt++ {
			if err := ctx.Err(); err != nil {
				return err
			}

			routeBody, sc, err := c.doJSON(ctx, http.MethodGet,
				"/v1/ai/routes/"+routeInfo.Name, nil)
			if err != nil {
				lastErr = fmt.Errorf("get AI route %s: %w", routeInfo.Name, err)
				break
			}
			if sc != http.StatusOK {
				lastErr = fmt.Errorf("get AI route %s: HTTP %d", routeInfo.Name, sc)
				break
			}

			var routeResp struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(routeBody, &routeResp); err != nil {
				lastErr = fmt.Errorf("decode AI route %s envelope: %w", routeInfo.Name, err)
				break
			}
			routeData := routeResp.Data
			if routeData == nil {
				routeData = routeBody
			}

			var route map[string]interface{}
			if err := json.Unmarshal(routeData, &route); err != nil {
				lastErr = fmt.Errorf("decode AI route %s: %w", routeInfo.Name, err)
				break
			}

			matchesProvider := providerFilter == "" || routeMatchesProvider(route, providerFilter)
			if providerFilter != "" && !add && !matchesProvider {
				break
			}

			authConfig, _ := route["authConfig"].(map[string]interface{})
			if authConfig == nil {
				authConfig = make(map[string]interface{})
			}

			consumers := toStringSlice(authConfig["allowedConsumers"])

			changed := true
			if add && matchesProvider {
				if !containsString(consumers, consumerName) {
					consumers = append(consumers, consumerName)
				}
				// Always PUT to trigger WASM key-auth resync — the consumer
				// may have been created after the route was last written,
				// so WASM needs to reload credentials even if the name was
				// already in allowedConsumers.
			} else {
				before := len(consumers)
				consumers = removeString(consumers, consumerName)
				changed = before != len(consumers)
			}

			if !changed {
				break
			}

			authConfig["allowedConsumers"] = consumers
			route["authConfig"] = authConfig

			_, sc, err = c.doJSON(ctx, http.MethodPut,
				"/v1/ai/routes/"+routeInfo.Name, route)
			if err != nil {
				lastErr = fmt.Errorf("put AI route %s: %w", routeInfo.Name, err)
				break
			}
			if sc == http.StatusOK {
				lastErr = nil
				break
			}
			if sc == http.StatusConflict {
				lastErr = fmt.Errorf("put AI route %s: HTTP 409 (conflict)", routeInfo.Name)
				time.Sleep(time.Duration(rand.Intn(3)+1) * time.Second)
				continue
			}
			lastErr = fmt.Errorf("put AI route %s: HTTP %d", routeInfo.Name, sc)
			break
		}
		if lastErr != nil {
			recordErr(lastErr)
		}
	}

	return firstErr
}

// routeMatchesProvider checks if any upstream in the route references the given provider.
func routeMatchesProvider(route map[string]interface{}, provider string) bool {
	upstreams, ok := route["upstreams"].([]interface{})
	if !ok {
		return false
	}
	for _, u := range upstreams {
		ups, ok := u.(map[string]interface{})
		if !ok {
			continue
		}
		if p, _ := ups["provider"].(string); p == provider {
			return true
		}
	}
	return false
}

func (c *HigressClient) ExposePort(ctx context.Context, req PortExposeRequest) error {
	svcSrc := fmt.Sprintf("worker-%s-%d", req.WorkerName, req.Port)
	routeN := svcSrc
	domain := req.Domain
	if domain == "" {
		domain = fmt.Sprintf("worker-%s-%d-local.agentteams.io", req.WorkerName, req.Port)
	}
	dnsHost := req.ServiceHost
	if dnsHost == "" {
		dnsHost = fmt.Sprintf("%s.local", req.WorkerName)
	}

	if err := c.ensureDomain(ctx, domain); err != nil {
		return fmt.Errorf("expose port %d: %w", req.Port, err)
	}
	if err := c.ensureServiceSource(ctx, svcSrc, dnsHost, req.Port, "http"); err != nil {
		return fmt.Errorf("expose port %d: %w", req.Port, err)
	}
	if err := c.ensureRoute(ctx, routeN, []string{domain}, svcSrc+".dns", req.Port, "/"); err != nil {
		return fmt.Errorf("expose port %d: %w", req.Port, err)
	}
	return nil
}

func (c *HigressClient) UnexposePort(ctx context.Context, req PortExposeRequest) error {
	svcSrc := fmt.Sprintf("worker-%s-%d", req.WorkerName, req.Port)
	routeN := svcSrc
	domain := req.Domain
	if domain == "" {
		domain = fmt.Sprintf("worker-%s-%d-local.agentteams.io", req.WorkerName, req.Port)
	}

	c.deleteRoute(ctx, routeN)
	c.deleteServiceSource(ctx, svcSrc)
	c.deleteDomain(ctx, domain)
	return nil
}

// --- Public infrastructure init methods (used by Initializer) ---

func (c *HigressClient) EnsureServiceSource(ctx context.Context, name, domain string, port int, protocol string) error {
	return c.ensureServiceSource(ctx, name, domain, port, protocol)
}

func (c *HigressClient) EnsureStaticServiceSource(ctx context.Context, name, address string, port int) error {
	return c.ensureStaticServiceSource(ctx, name, address, port)
}

func (c *HigressClient) EnsureRoute(ctx context.Context, name string, domains []string, serviceName string, port int, pathPrefix string) error {
	return c.ensureRoute(ctx, name, domains, serviceName, port, pathPrefix)
}

func (c *HigressClient) DeleteRoute(ctx context.Context, name string) error {
	c.deleteRoute(ctx, name)
	return nil
}

func (c *HigressClient) EnsureAIProvider(ctx context.Context, req AIProviderRequest) error {
	body := map[string]interface{}{
		"name":     req.Name,
		"type":     req.Type,
		"tokens":   req.Tokens,
		"protocol": req.Protocol,
	}
	if req.Raw != nil {
		body["rawConfigs"] = req.Raw
	}
	_, sc, err := c.doJSON(ctx, http.MethodPost, "/v1/ai/providers", body)
	if err != nil {
		return fmt.Errorf("ensure AI provider %s: %w", req.Name, err)
	}
	if sc == 200 || sc == 201 || sc == 409 {
		return nil
	}
	return fmt.Errorf("ensure AI provider %s: HTTP %d", req.Name, sc)
}

func (c *HigressClient) EnsureStreamIdleTimeout(ctx context.Context, seconds int) error {
	if seconds <= 0 {
		seconds = 900
	}

	respBody, sc, err := c.doJSON(ctx, http.MethodGet, "/system/higress-config", nil)
	if err != nil {
		return fmt.Errorf("ensure stream idle timeout: read higress config: %w", err)
	}
	if sc != http.StatusOK {
		return fmt.Errorf("ensure stream idle timeout: read higress config HTTP %d", sc)
	}

	var resp struct {
		Success bool        `json:"success"`
		Data    interface{} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("ensure stream idle timeout: parse higress config response: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("ensure stream idle timeout: higress config response was unsuccessful")
	}
	config, ok := resp.Data.(string)
	if !ok {
		return fmt.Errorf("ensure stream idle timeout: higress config data was %T, want string", resp.Data)
	}

	patched := patchDownstreamIdleTimeout(config, seconds)
	_, putSC, putErr := c.doJSON(ctx, http.MethodPut, "/system/higress-config", map[string]interface{}{"config": patched})
	if putErr != nil {
		return fmt.Errorf("ensure stream idle timeout: update higress config: %w", putErr)
	}
	if putSC != http.StatusOK && putSC != http.StatusCreated && putSC != http.StatusNoContent {
		return fmt.Errorf("ensure stream idle timeout: update higress config HTTP %d", putSC)
	}
	return nil
}

func patchDownstreamIdleTimeout(config string, seconds int) string {
	lines := strings.Split(config, "\n")
	out := make([]string, 0, len(lines)+1)
	inDownstream := false
	patched := false
	value := "      idleTimeout: " + strconv.Itoa(seconds)

	for _, line := range lines {
		if line == "    downstream:" {
			inDownstream = true
			out = append(out, line)
			continue
		}
		if inDownstream && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") {
			if !patched {
				out = append(out, value)
				patched = true
			}
			inDownstream = false
		}
		if inDownstream && strings.HasPrefix(strings.TrimSpace(line), "idleTimeout:") {
			out = append(out, value)
			patched = true
			continue
		}
		out = append(out, line)
	}

	if inDownstream && !patched {
		out = append(out, value)
	}
	return strings.Join(out, "\n")
}

// EnsureAIRoute creates the AI route skeleton (name, path, upstream, key-auth
// framework) only if it does not already exist. It deliberately never writes
// authConfig.allowedConsumers: that field is owned by Manager/Worker
// reconcilers via AuthorizeAIRoutes / DeauthorizeAIRoutes. Re-running this
// function on an already-provisioned cluster is a true no-op and will never
// touch the authorization state, eliminating the restart-time race that
// previously reset allowedConsumers and produced 403s.
func (c *HigressClient) EnsureAIRoute(ctx context.Context, req AIRouteRequest) error {
	getBody, sc, err := c.doJSON(ctx, http.MethodGet, "/v1/ai/routes/"+req.Name, nil)
	if err != nil {
		return fmt.Errorf("ensure AI route %s: check existence: %w", req.Name, err)
	}

	switch sc {
	case http.StatusOK:
		var resp struct {
			Data map[string]interface{} `json:"data"`
		}
		if jerr := json.Unmarshal(getBody, &resp); jerr == nil && resp.Data != nil {
			existingPath := ""
			if p, ok := resp.Data["pathPredicate"].(map[string]interface{}); ok {
				existingPath, _ = p["matchValue"].(string)
			}
			existingProvider := ""
			if ups, ok := resp.Data["upstreams"].([]interface{}); ok && len(ups) > 0 {
				if u0, ok := ups[0].(map[string]interface{}); ok {
					existingProvider, _ = u0["provider"].(string)
				}
			}
			if existingPath != req.PathPrefix || existingProvider != req.Provider {
				log.Printf("[WARN] AI route %s already exists with divergent skeleton (path=%q provider=%q, want path=%q provider=%q); leaving auth state untouched",
					req.Name, existingPath, existingProvider, req.PathPrefix, req.Provider)
			}
		}
		return nil

	case http.StatusNotFound:
		body := map[string]interface{}{
			"name":    req.Name,
			"domains": []string{},
			"pathPredicate": map[string]interface{}{
				"matchType":     "PRE",
				"matchValue":    req.PathPrefix,
				"caseSensitive": false,
			},
			"upstreams": []map[string]interface{}{
				{"provider": req.Provider, "weight": 100, "modelMapping": map[string]interface{}{}},
			},
			// Enable the key-auth framework, but deliberately omit
			// allowedConsumers: Higress defaults it to [] and Manager/Worker
			// reconcilers will populate it via AuthorizeAIRoutes once their
			// consumers exist. We never write this field here.
			"authConfig": map[string]interface{}{
				"enabled":                true,
				"allowedCredentialTypes": []string{"key-auth"},
			},
		}
		_, psc, perr := c.doJSON(ctx, http.MethodPost, "/v1/ai/routes", body)
		if perr != nil {
			return fmt.Errorf("ensure AI route %s: create: %w", req.Name, perr)
		}
		if psc == http.StatusOK || psc == http.StatusCreated || psc == http.StatusConflict {
			return nil
		}
		return fmt.Errorf("ensure AI route %s: create: HTTP %d", req.Name, psc)

	default:
		return fmt.Errorf("ensure AI route %s: check existence: HTTP %d", req.Name, sc)
	}
}

func (c *HigressClient) ResolveModelProvider(ctx context.Context, name string) (*ModelProviderInfo, error) {
	// Verify the provider exists.
	_, sc, err := c.doJSON(ctx, http.MethodGet, "/v1/ai/providers/"+name, nil)
	if err != nil {
		return nil, fmt.Errorf("higress: get AI provider %q: %w", name, err)
	}
	if sc == http.StatusNotFound {
		return nil, fmt.Errorf("higress: model provider %q not found", name)
	}
	if sc != http.StatusOK {
		return nil, fmt.Errorf("higress: get AI provider %q: HTTP %d", name, sc)
	}

	// Find the AI route that uses this provider.
	routesBody, sc, err := c.doJSON(ctx, http.MethodGet, "/v1/ai/routes", nil)
	if err != nil {
		return nil, fmt.Errorf("higress: list AI routes: %w", err)
	}
	if sc != http.StatusOK {
		return nil, fmt.Errorf("higress: list AI routes: HTTP %d", sc)
	}

	var listResp struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(routesBody, &listResp); err != nil {
		return nil, fmt.Errorf("higress: decode AI routes list: %w", err)
	}

	for _, raw := range listResp.Data {
		var routeInfo struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &routeInfo); err != nil || routeInfo.Name == "" {
			continue
		}

		routeBody, sc, err := c.doJSON(ctx, http.MethodGet, "/v1/ai/routes/"+routeInfo.Name, nil)
		if err != nil || sc != http.StatusOK {
			continue
		}

		var routeResp struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(routeBody, &routeResp); err != nil {
			continue
		}
		routeData := routeResp.Data
		if routeData == nil {
			routeData = routeBody
		}

		var route struct {
			PathPredicate struct {
				MatchValue string `json:"matchValue"`
			} `json:"pathPredicate"`
			Upstreams []struct {
				Provider string `json:"provider"`
			} `json:"upstreams"`
		}
		if err := json.Unmarshal(routeData, &route); err != nil {
			continue
		}

		for _, ups := range route.Upstreams {
			if ups.Provider == name {
				basePath := route.PathPredicate.MatchValue
				intranetURL := strings.TrimRight(c.config.DataPlaneURL, "/") + basePath
				return &ModelProviderInfo{
					HttpApiID:   name,
					BasePath:    basePath,
					IntranetURL: intranetURL,
				}, nil
			}
		}
	}

	return nil, fmt.Errorf("higress: no AI route found for model provider %q", name)
}

func (c *HigressClient) Healthy(ctx context.Context) error {
	_, sc, err := c.doJSON(ctx, http.MethodGet, "/v1/consumers", nil)
	if err != nil {
		return err
	}
	if sc != http.StatusOK {
		return fmt.Errorf("higress health check: HTTP %d", sc)
	}
	return nil
}

// ── Higress Console primitives (migrated from controller/higress_client.go) ──

func (c *HigressClient) ensureDomain(ctx context.Context, name string) error {
	body := map[string]interface{}{"name": name, "enableHttps": "off"}
	_, sc, err := c.doJSON(ctx, http.MethodPost, "/v1/domains", body)
	if err != nil {
		return fmt.Errorf("ensure domain %s: %w", name, err)
	}
	if sc != 200 && sc != 201 && sc != 409 {
		return fmt.Errorf("ensure domain %s: HTTP %d", name, sc)
	}
	return nil
}

func (c *HigressClient) ensureServiceSource(ctx context.Context, name, dnsDomain string, port int, protocol string) error {
	if protocol == "" {
		protocol = "http"
	}
	body := map[string]interface{}{
		"type": "dns", "name": name, "domain": dnsDomain,
		"port": port, "protocol": protocol,
		"properties": map[string]interface{}{},
		"authN":      map[string]interface{}{"enabled": false},
	}
	_, sc, err := c.doJSON(ctx, http.MethodPost, "/v1/service-sources", body)
	if err != nil {
		return fmt.Errorf("ensure service source %s: %w", name, err)
	}
	if sc != 200 && sc != 201 && sc != 409 {
		return fmt.Errorf("ensure service source %s: HTTP %d", name, sc)
	}
	return nil
}

func (c *HigressClient) ensureStaticServiceSource(ctx context.Context, name, address string, port int) error {
	body := map[string]interface{}{
		"type": "static", "name": name, "domain": fmt.Sprintf("%s:%d", address, port),
		"port": port, "protocol": "http",
		"properties": map[string]interface{}{},
		"authN":      map[string]interface{}{"enabled": false},
	}
	_, sc, err := c.doJSON(ctx, http.MethodPost, "/v1/service-sources", body)
	if err != nil {
		return fmt.Errorf("ensure static service source %s: %w", name, err)
	}
	if sc != 200 && sc != 201 && sc != 409 {
		return fmt.Errorf("ensure static service source %s: HTTP %d", name, sc)
	}
	return nil
}

func (c *HigressClient) ensureRoute(ctx context.Context, name string, domains []string, serviceName string, port int, pathPrefix string) error {
	if pathPrefix == "" {
		pathPrefix = "/"
	}
	body := map[string]interface{}{
		"name":    name,
		"domains": domains,
		"path":    map[string]interface{}{"matchType": "PRE", "matchValue": pathPrefix, "caseSensitive": false},
		"services": []map[string]interface{}{
			{"name": serviceName, "port": port, "weight": 100},
		},
	}
	_, sc, err := c.doJSON(ctx, http.MethodPost, "/v1/routes", body)
	if err != nil {
		return fmt.Errorf("ensure route %s: %w", name, err)
	}
	if sc == 200 || sc == 201 || sc == 409 {
		return nil
	}
	return fmt.Errorf("ensure route %s: HTTP %d", name, sc)
}

func (c *HigressClient) deleteRoute(ctx context.Context, name string) {
	c.doJSON(ctx, http.MethodDelete, "/v1/routes/"+name, nil)
}

func (c *HigressClient) deleteServiceSource(ctx context.Context, name string) {
	c.doJSON(ctx, http.MethodDelete, "/v1/service-sources/"+name, nil)
}

func (c *HigressClient) deleteDomain(ctx context.Context, name string) {
	c.doJSON(ctx, http.MethodDelete, "/v1/domains/"+name, nil)
}

// doJSON performs an HTTP request with session cookies.
func (c *HigressClient) doJSON(ctx context.Context, method, path string, reqBody interface{}) ([]byte, int, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, 0, err
	}

	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	url := strings.TrimRight(c.config.ConsoleURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.mu.Lock()
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	c.mu.Unlock()

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		c.mu.Lock()
		c.cookies = nil
		c.mu.Unlock()
	}

	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}

// ── helpers ──

func toStringSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch arr := v.(type) {
	case []interface{}:
		var result []string
		for _, item := range arr {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return arr
	}
	return nil
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	var result []string
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}
