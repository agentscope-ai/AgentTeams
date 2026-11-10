package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ManagementClient exposes only the provider and AI route management surface.
// Cloud gateway implementations can omit this interface when unsupported.
type ManagementClient interface {
	ManageAI(context.Context, string, string, string, json.RawMessage) ([]byte, int, error)
}

func (c *HigressClient) ManageAI(ctx context.Context, method, resource, name string, body json.RawMessage) ([]byte, int, error) {
	if resource != "providers" && resource != "routes" {
		return nil, 0, fmt.Errorf("unsupported AI resource")
	}
	if strings.ContainsAny(name, "/\\?#") || name == "." || name == ".." {
		return nil, 0, fmt.Errorf("invalid resource name")
	}
	switch method {
	case http.MethodGet:
	case http.MethodPost:
		if name != "" {
			return nil, 0, fmt.Errorf("POST requires a collection")
		}
	case http.MethodPut, http.MethodDelete:
		if name == "" {
			return nil, 0, fmt.Errorf("resource name is required")
		}
	default:
		return nil, 0, fmt.Errorf("unsupported method")
	}
	path := "/v1/ai/" + resource
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	var payload interface{}
	if len(body) > 0 {
		payload = body
	}
	// Providers added after installation need the same service-source setup as
	// install-time custom endpoints. A URL alone does not create a Higress upstream.
	if resource == "providers" && (method == http.MethodPost || method == http.MethodPut) {
		var provider map[string]interface{}
		if err := json.Unmarshal(body, &provider); err != nil {
			return nil, 0, fmt.Errorf("invalid provider body")
		}
		raw, _ := provider["rawConfigs"].(map[string]interface{})
		customURL, _ := raw["openaiCustomUrl"].(string)
		if customURL != "" {
			u, err := url.Parse(customURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || strings.Contains(u.Hostname(), ":") {
				return nil, 0, fmt.Errorf("invalid custom provider endpoint")
			}
			providerName, _ := provider["name"].(string)
			if name != "" {
				providerName = name
			}
			if providerName == "" || strings.ContainsAny(providerName, "/\\?#") || providerName == "." || providerName == ".." {
				return nil, 0, fmt.Errorf("invalid provider name")
			}
			port := 80
			if u.Scheme == "https" {
				port = 443
			}
			if u.Port() != "" {
				port, err = strconv.Atoi(u.Port())
				if err != nil || port < 1 || port > 65535 {
					return nil, 0, fmt.Errorf("invalid custom provider port")
				}
			}
			kind := "dns"
			if net.ParseIP(u.Hostname()) != nil {
				kind = "static"
				err = c.EnsureStaticServiceSource(ctx, providerName, u.Hostname(), port, u.Scheme)
			} else {
				err = c.EnsureServiceSource(ctx, providerName, u.Hostname(), port, u.Scheme)
			}
			if err != nil {
				return nil, 0, err
			}
			u.Host = u.Hostname()
			raw["openaiCustomUrl"] = u.String()
			raw["openaiCustomServiceName"] = providerName + "." + kind
			raw["openaiCustomServicePort"] = port
			payload = provider
		}
	}
	return c.doJSON(ctx, method, path, payload)
}
