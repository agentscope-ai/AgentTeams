package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
)

func TestProviderMaskRemovesHigressRawAPITokens(t *testing.T) {
	value := map[string]interface{}{"data": map[string]interface{}{"tokens": []interface{}{"primary-secret"}, "rawConfigs": map[string]interface{}{"apiTokens": []interface{}{"raw-secret"}, "apiUrl": "https://provider.example/v1"}}}
	maskProviderTokens(value)
	data, _ := json.Marshal(value)
	if strings.Contains(string(data), "secret") {
		t.Fatalf("provider credentials leaked: %s", data)
	}
	if !strings.Contains(string(data), `"tokenCount":1`) || !strings.Contains(string(data), "provider.example") {
		t.Fatalf("provider metadata lost: %s", data)
	}
}

type consumerDeleteRegressionClient struct {
	gateway.Client
	calls     []string
	unbindErr error
}

func (c *consumerDeleteRegressionClient) DeauthorizeAIRoutes(_ context.Context, name, filter string) error {
	c.calls = append(c.calls, "unbind:"+name)
	return c.unbindErr
}
func (c *consumerDeleteRegressionClient) DeleteConsumer(_ context.Context, name string) error {
	c.calls = append(c.calls, "delete:"+name)
	return nil
}

func TestDeleteConsumerRevokesRouteBindingsFirst(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "unbind failure"}[fail], func(t *testing.T) {
			c := &consumerDeleteRegressionClient{}
			if fail {
				c.unbindErr = errors.New("unbind failed")
			}
			h := NewGatewayHandler(c)
			r := httptest.NewRequest("DELETE", "/api/v1/gateway/consumers/test", nil)
			r.SetPathValue("id", "test")
			w := httptest.NewRecorder()
			h.DeleteConsumer(w, r)
			want := "unbind:test,delete:test"
			status := 204
			if fail {
				want = "unbind:test"
				status = 500
			}
			if strings.Join(c.calls, ",") != want || w.Code != status {
				t.Fatalf("calls=%v status=%d", c.calls, w.Code)
			}
		})
	}
}
