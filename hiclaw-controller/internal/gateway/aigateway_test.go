package gateway

import (
	"context"
	"testing"

	apig "github.com/alibabacloud-go/apig-20240327/v6/client"
	"github.com/alibabacloud-go/tea/tea"
)

type fakeAPIGClient struct {
	consumerName string
	ruleID       string
	deleteErr    error
	deleteCalls  int
}

func (f *fakeAPIGClient) CreateConsumer(*apig.CreateConsumerRequest) (*apig.CreateConsumerResponse, error) {
	return nil, nil
}

func (f *fakeAPIGClient) GetConsumer(*string) (*apig.GetConsumerResponse, error) {
	return nil, nil
}

func (f *fakeAPIGClient) DeleteConsumer(*string) (*apig.DeleteConsumerResponse, error) {
	return nil, nil
}

func (f *fakeAPIGClient) ListConsumers(*apig.ListConsumersRequest) (*apig.ListConsumersResponse, error) {
	return &apig.ListConsumersResponse{
		Body: &apig.ListConsumersResponseBody{
			Data: &apig.ListConsumersResponseBodyData{
				Items: []*apig.ListConsumersResponseBodyDataItems{{
					ConsumerId: tea.String("cs-test"),
					Name:       tea.String(f.consumerName),
				}},
			},
		},
	}, nil
}

func (f *fakeAPIGClient) CreateConsumerAuthorizationRules(*apig.CreateConsumerAuthorizationRulesRequest) (*apig.CreateConsumerAuthorizationRulesResponse, error) {
	return nil, nil
}

func (f *fakeAPIGClient) QueryConsumerAuthorizationRules(*apig.QueryConsumerAuthorizationRulesRequest) (*apig.QueryConsumerAuthorizationRulesResponse, error) {
	return &apig.QueryConsumerAuthorizationRulesResponse{
		Body: &apig.QueryConsumerAuthorizationRulesResponseBody{
			Data: &apig.QueryConsumerAuthorizationRulesResponseBodyData{
				Items: []*apig.QueryConsumerAuthorizationRulesResponseBodyDataItems{{
					ConsumerAuthorizationRuleId: tea.String(f.ruleID),
				}},
			},
		},
	}, nil
}

func (f *fakeAPIGClient) DeleteConsumerAuthorizationRule(*string, *string) (*apig.DeleteConsumerAuthorizationRuleResponse, error) {
	f.deleteCalls++
	return nil, f.deleteErr
}

func (f *fakeAPIGClient) ListHttpApis(*apig.ListHttpApisRequest) (*apig.ListHttpApisResponse, error) {
	return nil, nil
}

func TestAIGatewayDeauthorizeAIRoutesTreatsDeleteRule404AsGone(t *testing.T) {
	statusCode := 404
	cli := &fakeAPIGClient{
		consumerName: "gw-worker-alice",
		ruleID:       "car-test",
		deleteErr:    &tea.SDKError{StatusCode: &statusCode},
	}
	c := NewAIGatewayClientWithClient(AIGatewayConfig{
		GatewayID:  "gw",
		ModelAPIID: "model-api",
		EnvID:      "env",
	}, cli)

	if err := c.DeauthorizeAIRoutes(context.Background(), "worker-alice", ""); err != nil {
		t.Fatalf("DeauthorizeAIRoutes: %v", err)
	}
	if cli.deleteCalls != 1 {
		t.Fatalf("DeleteConsumerAuthorizationRule calls=%d, want 1", cli.deleteCalls)
	}
}
