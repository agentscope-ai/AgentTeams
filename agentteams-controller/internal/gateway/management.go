package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	return c.doJSON(ctx, method, path, payload)
}
