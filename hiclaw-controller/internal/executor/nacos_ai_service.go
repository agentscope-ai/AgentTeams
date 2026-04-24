package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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

type nacosAgentSpecMeta struct {
	NamespaceID string                          `json:"namespaceId"`
	Name        string                          `json:"name"`
	Description string                          `json:"description"`
	OnlineCnt   int                             `json:"onlineCnt"`
	Labels      map[string]string               `json:"labels,omitempty"`
	Versions    []nacosAgentSpecVersionMetadata `json:"versions,omitempty"`
}

type nacosAgentSpecVersionMetadata struct {
	Version string `json:"version"`
	Status  string `json:"status"`
}

type nacosAgentSpecSummary struct {
	NamespaceID string            `json:"namespaceId"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Enable      bool              `json:"enable"`
	Labels      map[string]string `json:"labels,omitempty"`
	OnlineCnt   int               `json:"onlineCnt"`
}

type nacosAgentSpecListResponse struct {
	TotalCount int                     `json:"totalCount"`
	PageItems  []nacosAgentSpecSummary `json:"pageItems"`
}

// GetSkill fetches a Skill ZIP from Nacos and extracts it into outputDir/{name}/.
// version may be empty (latest) or a specific version string.
// label may be used instead of version; both may not be set simultaneously.
func (c *NacosAIClient) GetSkill(ctx context.Context, name, outputDir, version, label string) error {
	params := url.Values{}
	params.Set("namespaceId", c.namespace)
	params.Set("name", name)
	if version != "" {
		params.Set("version", version)
	}
	if label != "" {
		params.Set("label", label)
	}

	apiURL := fmt.Sprintf("http://%s/nacos/v3/client/ai/skills?%s", c.serverAddr, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	if err := c.prepareRequest(ctx, req); err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to get skill: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return parseNacosHTTPError(resp.StatusCode, body, "get skill")
	}

	// Write the ZIP response to a temp file.
	tmp, err := os.CreateTemp("", "nacos-skill-*.zip")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to download skill ZIP: %w", err)
	}
	tmp.Close()

	// Extract into outputDir/{name}/.
	destDir := filepath.Join(outputDir, name)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("failed to create skill directory: %w", err)
	}

	cmd := exec.CommandContext(ctx, "unzip", "-q", "-o", tmpPath, "-d", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to extract skill ZIP for %s: %s: %w", name, string(out), err)
	}
	return nil
}

func (c *NacosAIClient) GetAgentSpec(ctx context.Context, name, outputDir string, version, label string) error {
	spec, err := c.fetchAgentSpec(ctx, name, version, label)
	if err != nil {
		return err
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

func (c *NacosAIClient) CheckAgentSpecExists(ctx context.Context, name, version, label string) error {
	summary, err := c.fetchAgentSpecSummary(ctx, name)
	if err != nil {
		return err
	}

	if !summary.Enable {
		return formatNacosHTTPError("check agentspec", http.StatusNotFound, "", fmt.Sprintf("agentspec %q is disabled", name))
	}
	if summary.OnlineCnt <= 0 {
		return formatNacosHTTPError("check agentspec", http.StatusNotFound, "", fmt.Sprintf("agentspec %q has no online version", name))
	}
	if version == "" && label == "" {
		return nil
	}

	if _, err := c.fetchAgentSpec(ctx, name, version, label); err != nil {
		if isNacosHTTPStatus(err, http.StatusNotFound) {
			if version != "" {
				return formatNacosHTTPError("check agentspec", http.StatusNotFound, "", fmt.Sprintf("online version %q not found for agentspec %q", version, name))
			}
			if label != "" {
				return formatNacosHTTPError("check agentspec", http.StatusNotFound, "", fmt.Sprintf("label %q for agentspec %q does not point to an online version", label, name))
			}
		}
		return err
	}
	return nil
}

func isNacosHTTPStatus(err error, statusCode int) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), fmt.Sprintf("(HTTP %d)", statusCode))
}

func (c *NacosAIClient) fetchAgentSpecSummary(ctx context.Context, name string) (*nacosAgentSpecSummary, error) {
	params := url.Values{}
	params.Set("namespaceId", c.namespace)
	params.Set("agentSpecName", name)
	params.Set("search", "accurate")
	params.Set("pageNo", "1")
	params.Set("pageSize", "1")

	apiURL := fmt.Sprintf("http://%s/nacos/v3/admin/ai/agentspecs/list?%s", c.serverAddr, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	if err := c.prepareRequest(ctx, req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get agentspec meta: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, parseNacosHTTPError(resp.StatusCode, respBody, "check agentspec")
	}

	var v3Resp nacosV3Response
	if err := json.Unmarshal(respBody, &v3Resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if v3Resp.Code != 0 {
		return nil, fmt.Errorf("check agentspec failed: code=%d, message=%s", v3Resp.Code, v3Resp.Message)
	}

	var listResp nacosAgentSpecListResponse
	if err := json.Unmarshal(v3Resp.Data, &listResp); err != nil {
		return nil, fmt.Errorf("failed to parse agentspec list: %w", err)
	}
	for _, item := range listResp.PageItems {
		if item.Name == name {
			return &item, nil
		}
	}
	return nil, formatNacosHTTPError("check agentspec", http.StatusNotFound, "", fmt.Sprintf("agentspec %q not found", name))
}

func (c *NacosAIClient) fetchAgentSpec(ctx context.Context, name, version, label string) (*nacosAgentSpec, error) {
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
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	if err := c.prepareRequest(ctx, req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get agentspec: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, parseNacosHTTPError(resp.StatusCode, respBody, "get agentspec")
	}

	var v3Resp nacosV3Response
	if err := json.Unmarshal(respBody, &v3Resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if v3Resp.Code != 0 {
		return nil, fmt.Errorf("get agentspec failed: code=%d, message=%s", v3Resp.Code, v3Resp.Message)
	}

	var spec nacosAgentSpec
	if err := json.Unmarshal(v3Resp.Data, &spec); err != nil {
		return nil, fmt.Errorf("failed to parse agentspec: %w", err)
	}
	return &spec, nil
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
