package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	nacosAuthTypeNone     = "none"
	nacosAuthTypeNacos    = "nacos"
	nacosPreflightTimeout = 5 * time.Second
)

type nacosAgentSpecClient struct {
	serverAddr       string
	namespace        string
	authType         string
	username         string
	password         string
	accessToken      string
	tokenExpireAt    time.Time
	authLoginVersion string
	httpClient       *http.Client
}

type nacosV3Response struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type nacosAgentSpec struct {
	NamespaceID string                             `json:"namespaceId"`
	Name        string                             `json:"name"`
	Description string                             `json:"description"`
	BizTags     string                             `json:"bizTags,omitempty"`
	Content     string                             `json:"content"`
	Resource    map[string]*nacosAgentSpecResource `json:"resource,omitempty"`
}

type nacosAgentSpecResource struct {
	Name     string                 `json:"name"`
	Type     string                 `json:"type"`
	Content  string                 `json:"content"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

func newNacosAgentSpecClient(ctx context.Context, rawAddr, namespace string) (*nacosAgentSpecClient, error) {
	host, port, username, password, err := parseNacosAddr(rawAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid nacos address %q: %w", rawAddr, err)
	}

	authType := nacosAuthTypeNone
	if username != "" && password != "" {
		authType = nacosAuthTypeNacos
	} else if username != "" || password != "" {
		return nil, fmt.Errorf("both username and password are required in nacos URL or env (use nacos://user:pass@host:port or set HICLAW_NACOS_USERNAME/HICLAW_NACOS_PASSWORD)")
	}

	client := &nacosAgentSpecClient{
		serverAddr: net.JoinHostPort(host, port),
		namespace:  namespace,
		authType:   authType,
		username:   username,
		password:   password,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}

	if client.namespace == "" {
		client.namespace = "public"
	}

	if err := client.preflightConnect(ctx); err != nil {
		return nil, err
	}

	if client.authType == nacosAuthTypeNacos {
		if err := client.login(ctx); err != nil {
			return nil, fmt.Errorf("login failed: %w", err)
		}
	}

	return client, nil
}

func (c *nacosAgentSpecClient) preflightConnect(ctx context.Context) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, nacosPreflightTimeout)
		defer cancel()
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.serverAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", c.serverAddr, err)
	}
	return conn.Close()
}

func (c *nacosAgentSpecClient) GetAgentSpec(ctx context.Context, name, outputDir string, version, label string) error {
	if err := c.ensureTokenValid(ctx); err != nil {
		return err
	}

	params := url.Values{}
	params.Set("namespaceId", c.namespace)
	params.Set("name", name)
	if version != "" {
		params.Set("version", version)
	}
	if label != "" {
		params.Set("label", label)
	}

	apiURL := fmt.Sprintf("http://%s/nacos/v3/client/ai/agentspecs?%s", c.serverAddr, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	c.setAuthHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to get agentspec: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return parseNacosHTTPError(resp.StatusCode, respBody, "get agentspec")
	}

	var v3Resp nacosV3Response
	if err := json.Unmarshal(respBody, &v3Resp); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	if v3Resp.Code != 0 {
		return fmt.Errorf("get agentspec failed: code=%d, message=%s", v3Resp.Code, v3Resp.Message)
	}

	var spec nacosAgentSpec
	if err := json.Unmarshal(v3Resp.Data, &spec); err != nil {
		return fmt.Errorf("failed to parse agentspec: %w", err)
	}

	specDir := filepath.Join(outputDir, name)
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	for _, res := range spec.Resource {
		if res == nil || res.Content == "" {
			continue
		}

		rel := buildAgentSpecResourcePath(res)
		if rel == "" {
			continue
		}

		filePath := filepath.Join(specDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return fmt.Errorf("failed to create resource directory: %w", err)
		}

		data := []byte(res.Content)
		if encoding, ok := res.Metadata["encoding"].(string); ok && encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(res.Content)
			if err != nil {
				return fmt.Errorf("failed to decode base64 resource %s: %w", res.Name, err)
			}
			data = decoded
		}

		if err := os.WriteFile(filePath, data, 0o644); err != nil {
			return fmt.Errorf("failed to write resource file %s: %w", res.Name, err)
		}
	}

	return writeAgentSpecManifest(specDir, spec.Content)
}

func (c *nacosAgentSpecClient) ensureTokenValid(ctx context.Context) error {
	if c.authType != nacosAuthTypeNacos {
		return nil
	}
	if c.accessToken == "" {
		return c.login(ctx)
	}
	if !c.tokenExpireAt.IsZero() && time.Now().Add(5*time.Second).After(c.tokenExpireAt) {
		return c.login(ctx)
	}
	return nil
}

func (c *nacosAgentSpecClient) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)

	tryV3 := c.authLoginVersion == "" || c.authLoginVersion == "v3"
	if tryV3 {
		ok, err := c.tryLogin(ctx, fmt.Sprintf("http://%s/nacos/v3/auth/user/login", c.serverAddr), form)
		if err == nil && ok {
			c.authLoginVersion = "v3"
			return nil
		}
	}

	ok, err := c.tryLogin(ctx, fmt.Sprintf("http://%s/nacos/v1/auth/login", c.serverAddr), form)
	if err != nil {
		return err
	}
	if ok {
		c.authLoginVersion = "v1"
		return nil
	}

	return fmt.Errorf("login failed with both v3 and v1 auth endpoints")
}

func (c *nacosAgentSpecClient) tryLogin(ctx context.Context, loginURL string, form url.Values) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	return c.applyLoginResponse(body), nil
}

func (c *nacosAgentSpecClient) applyLoginResponse(body []byte) bool {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}
	if data, ok := result["data"].(map[string]interface{}); ok {
		return c.applyLoginMap(data)
	}
	return c.applyLoginMap(result)
}

func (c *nacosAgentSpecClient) applyLoginMap(data map[string]interface{}) bool {
	token, ok := data["accessToken"].(string)
	if !ok || token == "" {
		return false
	}
	c.accessToken = token

	var ttlSeconds int64
	switch value := data["tokenTtl"].(type) {
	case float64:
		ttlSeconds = int64(value)
	case int64:
		ttlSeconds = value
	case int:
		ttlSeconds = int64(value)
	}
	if ttlSeconds > 0 {
		c.tokenExpireAt = time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	} else {
		c.tokenExpireAt = time.Time{}
	}
	return true
}

func (c *nacosAgentSpecClient) setAuthHeaders(req *http.Request) {
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
}

func buildAgentSpecResourcePath(res *nacosAgentSpecResource) string {
	if res == nil {
		return ""
	}

	resourceType := strings.TrimSpace(res.Type)
	resourceName := strings.TrimSpace(res.Name)
	if resourceType == "" {
		return resourceName
	}

	prefix := resourceType + "/"
	if strings.HasPrefix(resourceName, prefix) {
		return resourceName
	}
	return prefix + resourceName
}

func writeAgentSpecManifest(specDir, content string) error {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(content), &raw); err == nil {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, raw, "", "  "); err == nil {
			content = pretty.String()
		}
	}

	return os.WriteFile(filepath.Join(specDir, "manifest.json"), []byte(content), 0o644)
}

func parseNacosHTTPError(statusCode int, body []byte, operation string) error {
	serverMessage := ""
	if len(body) > 0 {
		var response nacosV3Response
		if err := json.Unmarshal(body, &response); err == nil && response.Message != "" {
			serverMessage = response.Message
		}
	}

	switch statusCode {
	case http.StatusUnauthorized:
		return formatNacosHTTPError(operation, statusCode, serverMessage, "authentication required; check username:password in the nacos URL or set HICLAW_NACOS_USERNAME/HICLAW_NACOS_PASSWORD")
	case http.StatusForbidden:
		return formatNacosHTTPError(operation, statusCode, serverMessage, "access denied; token may be expired or permissions may be missing")
	case http.StatusNotFound:
		return formatNacosHTTPError(operation, statusCode, serverMessage, "resource not found; check the namespace, name, version, or label")
	case http.StatusInternalServerError:
		return formatNacosHTTPError(operation, statusCode, serverMessage, "server internal error; inspect Nacos logs for details")
	default:
		if serverMessage != "" {
			return fmt.Errorf("%s failed (HTTP %d): %s", operation, statusCode, serverMessage)
		}
		if len(body) > 0 {
			bodyText := strings.TrimSpace(string(body))
			if len(bodyText) > 200 {
				bodyText = bodyText[:200] + "..."
			}
			return fmt.Errorf("%s failed (HTTP %d): %s", operation, statusCode, bodyText)
		}
		return fmt.Errorf("%s failed (HTTP %d)", operation, statusCode)
	}
}

func formatNacosHTTPError(operation string, statusCode int, serverMessage string, hint string) error {
	if serverMessage != "" {
		return fmt.Errorf("%s failed (HTTP %d): %s; hint: %s", operation, statusCode, serverMessage, hint)
	}
	return fmt.Errorf("%s failed (HTTP %d): %s", operation, statusCode, hint)
}

func parseNacosAddr(raw string) (host, port, username, password string, err error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", "", "", err
	}
	if parsed.Hostname() == "" {
		return "", "", "", "", fmt.Errorf("missing host")
	}

	port = parsed.Port()
	if port == "" {
		port = "8848"
	}

	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}

	if username == "" && password == "" {
		username = os.Getenv("HICLAW_NACOS_USERNAME")
		password = os.Getenv("HICLAW_NACOS_PASSWORD")
	}

	return parsed.Hostname(), port, username, password, nil
}
