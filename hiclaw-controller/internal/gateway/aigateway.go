package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	apig "github.com/alibabacloud-go/apig-20240327/v6/client"
	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	"github.com/alibabacloud-go/tea/tea"
	credential "github.com/aliyun/credentials-go/credentials"
)

// ErrUnsupportedOp is returned by AIGatewayClient for operations that only
// make sense on a self-hosted gateway (route / service-source / AI
// provider management). On Alibaba Cloud AI Gateway these resources are
// provisioned out-of-band through the APIG console or Terraform and the
// hiclaw control plane must not try to create them itself.
var ErrUnsupportedOp = errors.New("operation not supported on ai-gateway provider")

// AIGatewayConfig holds the Alibaba Cloud APIG parameters that
// AIGatewayClient needs in order to manage consumers and bind them to the
// hiclaw AI Model API.
type AIGatewayConfig struct {
	Region     string // e.g. "cn-hangzhou"
	Endpoint   string // optional APIG OpenAPI endpoint override
	GatewayID  string // APIG gateway instance id
	ModelAPIID string // LLM Model API id that consumers are bound to
	EnvID      string // APIG environment id
}

// apigClient is the subset of the apig SDK that AIGatewayClient uses. It
// exists so that tests can stub the cloud API without instantiating a
// real SDK client.
type apigClient interface {
	CreateConsumer(req *apig.CreateConsumerRequest) (*apig.CreateConsumerResponse, error)
	GetConsumer(consumerID *string) (*apig.GetConsumerResponse, error)
	DeleteConsumer(consumerID *string) (*apig.DeleteConsumerResponse, error)
	ListConsumers(req *apig.ListConsumersRequest) (*apig.ListConsumersResponse, error)
	CreateConsumerAuthorizationRules(req *apig.CreateConsumerAuthorizationRulesRequest) (*apig.CreateConsumerAuthorizationRulesResponse, error)
	QueryConsumerAuthorizationRules(req *apig.QueryConsumerAuthorizationRulesRequest) (*apig.QueryConsumerAuthorizationRulesResponse, error)
	DeleteConsumerAuthorizationRule(consumerAuthorizationRuleID *string, consumerID *string) (*apig.DeleteConsumerAuthorizationRuleResponse, error)
	ListHttpApis(req *apig.ListHttpApisRequest) (*apig.ListHttpApisResponse, error)
}

// AIGatewayClient implements gateway.Client against Alibaba Cloud APIG.
//
// Only the consumer-oriented operations (EnsureConsumer, DeleteConsumer,
// AuthorizeAIRoutes, DeauthorizeAIRoutes) are functional. Route and
// provider initialization is the responsibility of the APIG platform:
// they are expected to exist before hiclaw is installed, and the
// corresponding methods on this client return ErrUnsupportedOp so that
// accidental invocations fail fast (per K4 decision: cloud failures
// should not silently degrade).
type AIGatewayClient struct {
	config AIGatewayConfig
	client apigClient
}

// NewAIGatewayClient builds an AIGatewayClient using the Alibaba Cloud
// APIG SDK, signing requests with cred (typically obtained from
// credprovider.NewAliyunCredential wrapping a TokenManager).
func NewAIGatewayClient(cfg AIGatewayConfig, cred credential.Credential) (*AIGatewayClient, error) {
	if cfg.Region == "" {
		return nil, errors.New("ai-gateway: region is required")
	}
	if cfg.GatewayID == "" {
		return nil, errors.New("ai-gateway: gateway id is required")
	}
	if cred == nil {
		return nil, errors.New("ai-gateway: credential is required")
	}

	apiCfg := &openapi.Config{}
	apiCfg.SetCredential(cred).
		SetRegionId(cfg.Region).
		SetEndpoint(apigEndpoint(cfg.Region, cfg.Endpoint))

	cli, err := apig.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("ai-gateway: create APIG client: %w", err)
	}
	return &AIGatewayClient{config: cfg, client: cli}, nil
}

func apigEndpoint(region, override string) string {
	override = strings.TrimSpace(override)
	if override != "" {
		return override
	}
	return fmt.Sprintf("apig.%s.aliyuncs.com", region)
}

// NewAIGatewayClientWithClient is the testing constructor: it accepts a
// pre-built apigClient stub.
func NewAIGatewayClientWithClient(cfg AIGatewayConfig, cli apigClient) *AIGatewayClient {
	return &AIGatewayClient{config: cfg, client: cli}
}

// --- consumer operations ---

// consumerName prefixes the logical name with the gateway id so that
// consumer names do not collide across tenants sharing the same APIG
// account. This mirrors the behaviour of the old backend.APIGBackend.
func (a *AIGatewayClient) consumerName(name string) string {
	if a.config.GatewayID == "" {
		return name
	}
	return a.config.GatewayID + "-" + name
}

func (a *AIGatewayClient) EnsureConsumer(_ context.Context, req ConsumerRequest) (*ConsumerResult, error) {
	name := a.consumerName(req.Name)

	if id, key, err := a.findConsumer(name); err != nil {
		return nil, err
	} else if id != "" {
		return &ConsumerResult{Status: "exists", ConsumerID: id, APIKey: key}, nil
	}

	createReq := (&apig.CreateConsumerRequest{}).
		SetName(name).
		SetGatewayType("AI").
		SetEnable(true).
		SetDescription(fmt.Sprintf("AgentTeams consumer: %s", req.Name)).
		SetApikeyIdentityConfig(&apig.ApiKeyIdentityConfig{
			Type: tea.String("Apikey"),
			ApikeySource: &apig.ApiKeyIdentityConfigApikeySource{
				Source: tea.String("Default"),
				Value:  tea.String("Authorization"),
			},
			Credentials: []*apig.ApiKeyIdentityConfigCredentials{
				{GenerateMode: tea.String("System")},
			},
		})

	resp, err := a.client.CreateConsumer(createReq)
	if err != nil {
		// Handle the same race as the old APIGBackend: a concurrent
		// reconcile may have just created the consumer. Re-query and
		// return the existing one if we can find it.
		if isDuplicateErr(err) {
			log.Printf("[ai-gateway] CreateConsumer race for %s; re-querying", name)
			if id, key, qerr := a.findConsumer(name); qerr == nil && id != "" {
				return &ConsumerResult{Status: "exists", ConsumerID: id, APIKey: key}, nil
			}
		}
		return nil, fmt.Errorf("ai-gateway: CreateConsumer: %w", err)
	}

	consumerID := ""
	if resp != nil && resp.Body != nil && resp.Body.Data != nil && resp.Body.Data.ConsumerId != nil {
		consumerID = *resp.Body.Data.ConsumerId
	}
	apiKey, _ := a.getConsumerAPIKey(consumerID)

	log.Printf("[ai-gateway] created consumer %s (%s)", name, consumerID)
	return &ConsumerResult{Status: "created", ConsumerID: consumerID, APIKey: apiKey}, nil
}

func (a *AIGatewayClient) DeleteConsumer(_ context.Context, name string) error {
	full := a.consumerName(name)
	id, _, err := a.findConsumer(full)
	if err != nil {
		return err
	}
	if id == "" {
		return nil // already gone; stay idempotent
	}
	_, err = a.client.DeleteConsumer(tea.String(id))
	if err != nil {
		return fmt.Errorf("ai-gateway: DeleteConsumer: %w", err)
	}
	log.Printf("[ai-gateway] deleted consumer %s (%s)", full, id)
	return nil
}

func (a *AIGatewayClient) AuthorizeAIRoutes(_ context.Context, consumerName string, modelAPIID string) error {
	effectiveModelAPIID := a.config.ModelAPIID
	if modelAPIID != "" {
		effectiveModelAPIID = modelAPIID
	}
	if effectiveModelAPIID == "" || a.config.EnvID == "" {
		return fmt.Errorf("ai-gateway: ModelAPIID and EnvID must be configured to authorize consumers")
	}
	full := a.consumerName(consumerName)
	id, _, err := a.findConsumer(full)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("ai-gateway: consumer %s not found", full)
	}

	query := (&apig.QueryConsumerAuthorizationRulesRequest{}).
		SetConsumerId(id).
		SetResourceId(effectiveModelAPIID).
		SetEnvironmentId(a.config.EnvID).
		SetResourceType("LLM").
		SetPageNumber(1).
		SetPageSize(100)
	if qresp, qerr := a.client.QueryConsumerAuthorizationRules(query); qerr == nil &&
		qresp != nil && qresp.Body != nil && qresp.Body.Data != nil &&
		len(qresp.Body.Data.Items) > 0 {
		return nil
	}

	create := &apig.CreateConsumerAuthorizationRulesRequest{}
	create.SetAuthorizationRules([]*apig.CreateConsumerAuthorizationRulesRequestAuthorizationRules{{
		ConsumerId:   tea.String(id),
		ResourceType: tea.String("LLM"),
		ExpireMode:   tea.String("LongTerm"),
		ResourceIdentifier: &apig.CreateConsumerAuthorizationRulesRequestAuthorizationRulesResourceIdentifier{
			ResourceId:    tea.String(effectiveModelAPIID),
			EnvironmentId: tea.String(a.config.EnvID),
		},
	}})
	if _, err := a.client.CreateConsumerAuthorizationRules(create); err != nil {
		return fmt.Errorf("ai-gateway: CreateConsumerAuthorizationRules: %w", err)
	}
	log.Printf("[ai-gateway] authorized consumer %s on model %s", full, effectiveModelAPIID)
	return nil
}

func (a *AIGatewayClient) DeauthorizeAIRoutes(_ context.Context, consumerName string, modelAPIID string) error {
	effectiveModelAPIID := a.config.ModelAPIID
	if modelAPIID != "" {
		effectiveModelAPIID = modelAPIID
	}
	if effectiveModelAPIID == "" || a.config.EnvID == "" {
		return nil // nothing to revoke
	}
	full := a.consumerName(consumerName)
	id, _, err := a.findConsumer(full)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	query := (&apig.QueryConsumerAuthorizationRulesRequest{}).
		SetConsumerId(id).
		SetResourceId(effectiveModelAPIID).
		SetEnvironmentId(a.config.EnvID).
		SetResourceType("LLM").
		SetPageNumber(1).
		SetPageSize(100)
	qresp, err := a.client.QueryConsumerAuthorizationRules(query)
	if err != nil {
		return fmt.Errorf("ai-gateway: query auth rules: %w", err)
	}
	if qresp == nil || qresp.Body == nil || qresp.Body.Data == nil {
		return nil
	}
	for _, item := range qresp.Body.Data.Items {
		if item == nil || item.ConsumerAuthorizationRuleId == nil {
			continue
		}
		if _, err := a.client.DeleteConsumerAuthorizationRule(item.ConsumerAuthorizationRuleId, tea.String(id)); err != nil {
			return fmt.Errorf("ai-gateway: delete auth rule %s: %w",
				tea.StringValue(item.ConsumerAuthorizationRuleId), err)
		}
	}
	return nil
}

// ExposePort / UnexposePort: cloud-platform ingress is expected to be
// provisioned out-of-band. The hiclaw control plane does not manage APIG
// routes, so requesting port exposure through this client is an error.
func (a *AIGatewayClient) ExposePort(_ context.Context, _ PortExposeRequest) error {
	return ErrUnsupportedOp
}

func (a *AIGatewayClient) UnexposePort(_ context.Context, _ PortExposeRequest) error {
	return ErrUnsupportedOp
}

// --- Infrastructure operations: all fail-fast ---

func (a *AIGatewayClient) EnsureServiceSource(_ context.Context, _, _ string, _ int, _ string) error {
	return ErrUnsupportedOp
}

func (a *AIGatewayClient) EnsureStaticServiceSource(_ context.Context, _, _ string, _ int) error {
	return ErrUnsupportedOp
}

func (a *AIGatewayClient) EnsureRoute(_ context.Context, _ string, _ []string, _ string, _ int, _ string) error {
	return ErrUnsupportedOp
}

func (a *AIGatewayClient) DeleteRoute(_ context.Context, _ string) error {
	return ErrUnsupportedOp
}

func (a *AIGatewayClient) EnsureAIProvider(_ context.Context, _ AIProviderRequest) error {
	return ErrUnsupportedOp
}

func (a *AIGatewayClient) EnsureStreamIdleTimeout(_ context.Context, _ int) error {
	return ErrUnsupportedOp
}

func (a *AIGatewayClient) EnsureAIRoute(_ context.Context, _ AIRouteRequest) error {
	return ErrUnsupportedOp
}

// Healthy makes a lightweight ListConsumers call to verify that both the
// SDK credential (via the sidecar) and the APIG endpoint are reachable.
// Any error is bubbled up so that the initializer's waitForGateway can
// retry until the control-plane is ready.
func (a *AIGatewayClient) Healthy(_ context.Context) error {
	req := (&apig.ListConsumersRequest{}).
		SetGatewayType("AI").
		SetPageNumber(1).
		SetPageSize(1)
	_, err := a.client.ListConsumers(req)
	if err != nil {
		return fmt.Errorf("ai-gateway: list consumers: %w", err)
	}
	return nil
}

// ResolveModelProvider looks up a named APIG Model API (HttpApi of type LLM)
// and returns its basePath, Intranet subdomain URL, and httpApiId.
func (a *AIGatewayClient) ResolveModelProvider(_ context.Context, name string) (*ModelProviderInfo, error) {
	if a.config.GatewayID == "" {
		return nil, fmt.Errorf("ai-gateway: gateway ID is required to resolve model provider")
	}

	page := int32(1)
	for {
		req := (&apig.ListHttpApisRequest{}).
			SetGatewayId(a.config.GatewayID).
			SetGatewayType("AI").
			SetTypes("LLM").
			SetName(name).
			SetPageNumber(page).
			SetPageSize(100)
		resp, err := a.client.ListHttpApis(req)
		if err != nil {
			return nil, fmt.Errorf("ai-gateway: ListHttpApis: %w", err)
		}
		if resp == nil || resp.Body == nil || resp.Body.Data == nil {
			return nil, fmt.Errorf("ai-gateway: model provider %q not found", name)
		}
		for _, item := range resp.Body.Data.Items {
			if item == nil {
				continue
			}
			for _, api := range item.VersionedHttpApis {
				if api == nil || api.Name == nil || *api.Name != name {
					continue
				}
				info := &ModelProviderInfo{
					HttpApiID: tea.StringValue(api.HttpApiId),
					BasePath:  tea.StringValue(api.BasePath),
				}
				info.IntranetURL = resolveIntranetURL(api.DeployConfigs, info.BasePath)
				if info.IntranetURL == "" {
					return nil, fmt.Errorf("ai-gateway: model provider %q has no Intranet subdomain", name)
				}
				return info, nil
			}
		}
		if len(resp.Body.Data.Items) < 100 {
			break
		}
		page++
	}
	return nil, fmt.Errorf("ai-gateway: model provider %q not found", name)
}

// resolveIntranetURL finds the Intranet subdomain in deploy configs and
// constructs the full URL with basePath.
func resolveIntranetURL(deployConfigs []*apig.HttpApiDeployConfig, basePath string) string {
	for _, dc := range deployConfigs {
		if dc == nil {
			continue
		}
		for _, sd := range dc.SubDomains {
			if sd == nil || sd.NetworkType == nil || *sd.NetworkType != "Intranet" {
				continue
			}
			domain := tea.StringValue(sd.Name)
			if domain == "" {
				continue
			}
			protocol := "http"
			if sd.Protocol != nil && strings.EqualFold(*sd.Protocol, "HTTPS") {
				protocol = "https"
			}
			return protocol + "://" + domain + basePath
		}
	}
	return ""
}

// TriggerPush is a no-op on APIG: configuration propagates
// automatically. Preserving the method keeps the gateway.Client
// interface uniform with HigressClient.
func (a *AIGatewayClient) TriggerPush() {}

// --- helpers ---

func (a *AIGatewayClient) findConsumer(name string) (string, string, error) {
	page := int32(1)
	for {
		req := (&apig.ListConsumersRequest{}).
			SetGatewayType("AI").
			SetNameLike(name).
			SetPageNumber(page).
			SetPageSize(100)
		resp, err := a.client.ListConsumers(req)
		if err != nil {
			return "", "", fmt.Errorf("ai-gateway: ListConsumers: %w", err)
		}
		if resp == nil || resp.Body == nil || resp.Body.Data == nil {
			return "", "", nil
		}
		for _, c := range resp.Body.Data.Items {
			if c != nil && c.Name != nil && *c.Name == name {
				id := tea.StringValue(c.ConsumerId)
				key, _ := a.getConsumerAPIKey(id)
				return id, key, nil
			}
		}
		if len(resp.Body.Data.Items) < 100 {
			return "", "", nil
		}
		page++
	}
}

func (a *AIGatewayClient) getConsumerAPIKey(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	resp, err := a.client.GetConsumer(tea.String(id))
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Body == nil || resp.Body.Data == nil ||
		resp.Body.Data.ApiKeyIdentityConfig == nil {
		return "", nil
	}
	creds := resp.Body.Data.ApiKeyIdentityConfig.Credentials
	if len(creds) == 0 || creds[0] == nil || creds[0].Apikey == nil {
		return "", nil
	}
	return *creds[0].Apikey, nil
}

func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "ConsumerNameDuplicate") || strings.Contains(msg, "409")
}
