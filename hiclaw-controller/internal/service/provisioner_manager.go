package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ManagerProvisionRequest struct {
	Name string
}

// ManagerProvisionResult contains all outputs from a successful Manager provision.
type ManagerProvisionResult struct {
	MatrixUserID   string
	MatrixToken    string
	RoomID         string
	GatewayKey     string
	MinIOPassword  string
	MatrixPassword string
}

// ProvisionManager executes the full infrastructure setup for a Manager Agent:
// credentials, Matrix account, MinIO user, Admin DM room, Gateway consumer.
func (p *Provisioner) ProvisionManager(ctx context.Context, req ManagerProvisionRequest) (*ManagerProvisionResult, error) {
	logger := log.FromContext(ctx)
	managerName := req.Name
	matrixUsername := "manager"
	consumerName := "manager"
	managerMatrixID := p.matrix.UserID(matrixUsername)
	adminMatrixID := p.matrix.UserID(p.adminUser)

	// Step 1: Load or generate credentials
	creds, err := p.creds.Load(ctx, managerName)
	if err != nil {
		return nil, fmt.Errorf("load credentials: %w", err)
	}
	if creds == nil {
		creds, err = GenerateCredentials()
		if err != nil {
			return nil, fmt.Errorf("generate credentials: %w", err)
		}
		// Use pre-generated secrets from install script if available
		if p.managerPassword != "" {
			creds.MatrixPassword = p.managerPassword
		}
		if p.managerGatewayKey != "" {
			creds.GatewayKey = p.managerGatewayKey
		}
		if err := p.creds.Save(ctx, managerName, creds); err != nil {
			return nil, fmt.Errorf("save credentials: %w", err)
		}
	}

	// Step 2: Register Matrix account (always "manager", matching container script)
	logger.Info("registering Manager Matrix account", "matrixUser", matrixUsername)
	var userCreds *matrix.UserCredentials
	if p.MatrixAppServiceEnabled() {
		userCreds, err = p.matrix.EnsureAppServiceUser(ctx, matrixUsername)
		if err != nil {
			return nil, fmt.Errorf("Matrix AS registration failed: %w", err)
		}
		creds.MatrixPassword = "" // No password in AppService mode
	} else {
		userCreds, err = p.matrix.EnsureUser(ctx, matrix.EnsureUserRequest{
			Username: matrixUsername,
			Password: creds.MatrixPassword,
		})
		if err != nil {
			return nil, fmt.Errorf("Matrix registration failed: %w", err)
		}
		creds.MatrixPassword = userCreds.Password
	}
	// Cache the freshly issued access token so subsequent reconciles can
	// reuse it via RefreshManagerCredentials instead of issuing a new login
	// (which would rotate channels.matrix.accessToken in openclaw.json and
	// trigger a gateway restart).
	if userCreds.AccessToken != "" {
		creds.MatrixToken = userCreds.AccessToken
	}

	// Step 3: Create MinIO user (embedded mode only)
	if p.ossAdmin != nil {
		logger.Info("creating MinIO user for Manager", "name", managerName)
		if err := p.ossAdmin.EnsureUser(ctx, managerName, creds.MinIOPassword); err != nil {
			return nil, fmt.Errorf("MinIO user creation failed: %w", err)
		}
		if err := p.ossAdmin.EnsurePolicy(ctx, oss.PolicyRequest{
			WorkerName: managerName,
			IsManager:  true,
		}); err != nil {
			return nil, fmt.Errorf("MinIO policy creation failed: %w", err)
		}
	}

	// Step 4: Create Admin DM Room (Admin + Manager only)
	logger.Info("creating Manager Admin DM room", "name", managerName)
	powerLevels := map[string]int{
		adminMatrixID:   100,
		managerMatrixID: 100,
	}
	managerMeta := managerDMRoomMeta(managerName, managerMatrixID, adminMatrixID, p.adminUser)
	roomInfo, err := p.matrix.CreateRoom(ctx, matrix.CreateRoomRequest{
		Name:          fmt.Sprintf("Manager: %s", managerName),
		Topic:         fmt.Sprintf("Admin DM channel for Manager %s", managerName),
		Invite:        []string{adminMatrixID, managerMatrixID},
		PowerLevels:   powerLevels,
		IsDirect:      true,
		InitialState:  roomMetaState(managerMeta),
		RoomAliasName: roomAliasLocalpart("manager", managerName),
	})
	if err != nil {
		return nil, fmt.Errorf("Admin DM room creation failed: %w", err)
	}
	roomID := roomInfo.RoomID
	logger.Info("Manager Admin DM room ready", "roomID", roomID, "created", roomInfo.Created)

	if err := p.matrix.SetRoomState(ctx, roomID, roomMetaEventType, "", managerMeta, ""); err != nil {
		return nil, fmt.Errorf("set manager admin DM room meta: %w", err)
	}

	if err := p.creds.Save(ctx, managerName, creds); err != nil {
		logger.Error(err, "failed to persist credentials (non-fatal)")
	}

	// Step 5: Gateway consumer and authorization
	logger.Info("creating gateway consumer for Manager", "consumer", consumerName)
	consumerResult, err := p.gateway.EnsureConsumer(ctx, gateway.ConsumerRequest{
		Name:          consumerName,
		CredentialKey: creds.GatewayKey,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway consumer creation failed: %w", err)
	}
	if consumerResult.APIKey != "" && consumerResult.APIKey != creds.GatewayKey {
		creds.GatewayKey = consumerResult.APIKey
		_ = p.creds.Save(ctx, managerName, creds)
	}

	if err := p.gateway.AuthorizeAIRoutes(ctx, consumerName, ""); err != nil {
		return nil, fmt.Errorf("AI route authorization failed: %w", err)
	}
	// Higress WASM key-auth plugin needs ~1-2s to sync after route update.
	// Without this, the worker's first LLM call may get 401.
	time.Sleep(2 * time.Second)

	return &ManagerProvisionResult{
		MatrixUserID:   managerMatrixID,
		MatrixToken:    userCreds.AccessToken,
		RoomID:         roomID,
		GatewayKey:     creds.GatewayKey,
		MinIOPassword:  creds.MinIOPassword,
		MatrixPassword: creds.MatrixPassword,
	}, nil
}

// ManagerWelcomeRequest carries the locale hints that the controller
// renders into the first-boot onboarding prompt sent to a freshly
// provisioned Manager Agent.
type ManagerWelcomeRequest struct {
	// RoomID is the Admin DM room created by ProvisionManager (Step 4).
	RoomID string
	// Language is the install-time AGENTTEAMS_LANGUAGE selection ("zh" / "en").
	// Embedded as plain text in the prompt; the agent decides how to apply.
	Language string
	// Timezone is the install-time TZ env (IANA identifier, e.g.
	// "Asia/Shanghai"). Embedded as plain text so the agent can infer
	// the admin's likely region and offer additional language options.
	Timezone string
	// SoloOperator, when true, renders the non-interview welcome variant
	// (renderManagerWelcomeBodySolo) instead of the normal 4-question
	// onboarding interview. Sourced from Config.SoloOperator /
	// AGENTTEAMS_SOLO_OPERATOR.
	SoloOperator bool
}

// SendManagerWelcome delivers the first-boot onboarding prompt that asks
// the Manager Agent to greet the admin and collect identity preferences
// (name / language / communication style). It is the new-architecture
// replacement for the legacy in-container welcome flow that lived in
// `start-manager-agent.sh` (lines 535-608) and only ran when
// AGENTTEAMS_RUNTIME != "k8s".
//
// Idempotency is the caller's responsibility — the controller guards
// re-send via Manager.Status.WelcomeSent. This method only checks that
// the Manager Matrix user has joined the room before sending; if not,
// it returns (sent=false, err=nil) so the reconcile loop can requeue.
//
// Returns:
//   - (true, nil)  — message was successfully delivered.
//   - (false, nil) — manager not yet joined; caller should requeue.
//   - (false, err) — unrecoverable error (admin login / Matrix API).
//
// llmAuthProbePromptTemplate renders the chat-completions body the
// readiness probe sends. It uses the same model the Manager Agent will
// use for its real first reply, and asks for a one-word answer so the
// per-probe cost is negligible (~10-20 tokens total round-trip) even
// though we may issue several probes during the gateway's WASM
// key-auth propagation window per fresh install.
//
// Format chosen to maximise compatibility:
//   - Only the universally-supported `model` and `messages` fields. No
//     `max_tokens` — some openai-compat providers (notably Bedrock-fronted
//     models and o1/o3-style reasoning families) reject the parameter
//     outright with a 400, which would defeat the point of probing
//     (readiness would never go true on those backends).
//   - The user message is a direct, brevity-instructed prompt; the
//     assistant typically replies with 1-2 tokens. We do not parse the
//     response body — only the HTTP status matters.
const llmAuthProbePromptTemplate = `{"model":%q,"messages":[{"role":"user","content":"Reply with only one word: ok"}]}`

// IsManagerLLMAuthReady reports whether the manager's bearer token can
// currently drive a real LLM call through the AI gateway — i.e. whether
// (a) Higress's WASM key-auth filter has finished syncing the
// freshly-bound consumer credential onto the AI route, and (b) the
// upstream provider is reachable and serving the configured model.
// Together these are exactly what the Manager Agent needs in order to
// successfully compose its first reply to the welcome prompt. Joining
// the Admin DM Room (~5-10s after container start) is strictly faster
// than gateway propagation (~40-45s, the gap the legacy
// `start-manager-agent.sh` papered over with `sleep 45`); sending the
// welcome on the join signal alone would deliver a prompt the manager
// receives but cannot reply to, and the onboarding turn would be
// silently lost.
//
// Probe shape:
//   - POST <AIGatewayURL>/v1/chat/completions with the manager's bearer
//     token and a tiny chat body whose `model` is the actual
//     ManagerModel and whose only user message asks for a one-word
//     answer. This is the same code path the manager will exercise on
//     its first real reply, so a successful probe is end-to-end
//     proof-of-life rather than a synthetic "auth filter only" check.
//   - HTTP 200 → ready, return (true, nil).
//   - HTTP 401 / 403 → auth not yet propagated → return (false, nil).
//     This is the expected state during the propagation window; we do
//     NOT return an error here so the reconciler requeues quietly
//     without spamming WARN-level logs.
//   - Any other status (400, 404, 429, 5xx, …) → return (false, err).
//     The reconciler surfaces it at log-level so the operator can spot
//     persistent misconfigurations (wrong model name, upstream provider
//     down, quota exhausted). Better a delayed welcome than one the
//     manager cannot answer — we never give up, only the operator's
//     attention escalates as the warnings accumulate.
//   - Network / dial errors → returned as error; same WARN-and-retry.
//
// Empty AIGatewayURL, ManagerModel, or gatewayKey → return (true, nil)
// so unit tests and bring-your-own-gateway deploys (where the
// controller doesn't know the data-plane URL or the model) do not
// stall the welcome forever.
func (p *Provisioner) IsManagerLLMAuthReady(ctx context.Context, gatewayKey string) (bool, error) {
	if p.aiGatewayURL == "" || p.managerModel == "" || gatewayKey == "" {
		return true, nil
	}
	url := strings.TrimRight(p.aiGatewayURL, "/") + "/v1/chat/completions"
	body := fmt.Sprintf(llmAuthProbePromptTemplate, p.managerModel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("welcome: build llm probe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	req.Header.Set("Content-Type", "application/json")
	// 30s timeout: a real LLM call can legitimately take several seconds
	// (cold-start, slow upstream); we want to wait long enough for a
	// healthy answer but not so long that a wedged backend stalls every
	// welcome reconcile for this manager.
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("welcome: llm probe %s: %w", url, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, nil
	default:
		return false, fmt.Errorf("welcome: llm probe %s returned HTTP %d (model=%q)", url, resp.StatusCode, p.managerModel)
	}
}

// IsManagerJoinedDM reports whether the Manager's Matrix user is currently
// `join`ed to the supplied DM room. Pure read; safe to poll on every
// reconcile while waiting for the agent's first /sync to land its
// auto-join. See `reconcileManagerWelcome` for the rationale on why this
// MUST be separate from the actual send: claim-before-send would otherwise
// churn the status field with claim/rollback patches on every requeue.
func (p *Provisioner) IsManagerJoinedDM(ctx context.Context, roomID string) (bool, error) {
	if roomID == "" {
		return false, fmt.Errorf("welcome: empty RoomID")
	}
	managerMatrixID := p.matrix.UserID("manager")
	members, err := p.matrix.ListRoomMembers(ctx, roomID)
	if err != nil {
		return false, fmt.Errorf("welcome: list members of %s: %w", roomID, err)
	}
	for _, m := range members {
		if m.UserID == managerMatrixID && m.Membership == "join" {
			return true, nil
		}
	}
	return false, nil
}

// SendManagerWelcomeMessage posts the first-boot onboarding prompt as the
// homeserver admin into the given DM room. The caller (reconcile loop)
// MUST have already (a) verified membership via IsManagerJoinedDM and
// (b) committed the WelcomeSent=true claim to the API server, so that a
// racing reconcile cannot also reach this point and double-deliver.
func (p *Provisioner) SendManagerWelcomeMessage(ctx context.Context, req ManagerWelcomeRequest) error {
	if req.RoomID == "" {
		return fmt.Errorf("welcome: empty RoomID")
	}
	language := req.Language
	if language == "" {
		language = "zh"
	}
	timezone := req.Timezone
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	var body string
	if req.SoloOperator {
		body = renderManagerWelcomeBodySolo(language, timezone)
	} else {
		body = renderManagerWelcomeBody(language, timezone)
	}
	if err := p.matrix.SendMessageAsAdmin(ctx, req.RoomID, body); err != nil {
		return fmt.Errorf("welcome: send to %s: %w", req.RoomID, err)
	}
	return nil
}

// renderManagerWelcomeBody returns the verbatim onboarding prompt the
// Manager Agent receives on first boot. Kept identical in spirit to the
// legacy `_welcome_msg` heredoc in `manager/scripts/init/start-manager-agent.sh`
// so the resulting agent behavior (greeting + 4-question Q&A + write
// SOUL.md + touch ~/soul-configured) is unchanged across architectures.
func renderManagerWelcomeBody(language, timezone string) string {
	return fmt.Sprintf(`This is an automated message from the AgentTeams setup. This is a fresh installation.

--- Installation Context ---
User Language: %s  (zh = Chinese, en = English)
User Timezone: %s  (IANA timezone identifier)
---

You are an AI agent that manages a team of worker agents. Your identity and personality have not been configured yet — the human admin is about to meet you for the first time.

Please begin the onboarding conversation:

1. Greet the admin warmly and briefly describe what you can do (coordinate workers, manage tasks, run multi-agent projects)
2. The user has selected "%s" as their preferred language during installation. Use this language for your greeting and all subsequent communication.
3. The user's timezone is %s. Based on this timezone, you may infer their likely region and suggest additional language options.
4. Ask them: a) What would they like to call you? b) Communication style preference? c) Any behavior guidelines? d) Confirm default language
5. After they reply, write their preferences to ~/SOUL.md
6. Confirm what you wrote, and ask if they would like to adjust anything
7. Once confirmed, run: touch ~/soul-configured

The human admin will start chatting shortly.`, language, timezone, language, timezone)
}

// renderManagerWelcomeBodySolo returns the first-boot onboarding prompt used
// when Config.SoloOperator is true: a single human is running HiClaw alone,
// so the normal 4-question identity interview (name / communication style /
// behavior guidelines / language confirmation) is unnecessary ceremony —
// there's nobody else to introduce the agent to, and no org-wide SOUL.md
// consensus to negotiate. The agent still greets the admin and still writes
// ~/SOUL.md + touches ~/soul-configured so downstream gating (that checks
// for soul-configured) behaves identically to the interview path, but it
// picks sensible defaults itself instead of asking.
func renderManagerWelcomeBodySolo(language, timezone string) string {
	return fmt.Sprintf(`This is an automated message from the HiClaw setup. This is a fresh installation running in solo mode (single operator).

--- Installation Context ---
User Language: %s  (zh = Chinese, en = English)
User Timezone: %s  (IANA timezone identifier)
---

You are an AI agent that manages a team of worker agents. You are being run by a single human operator working solo — there is no larger organization to onboard.

Please do the following, without conducting an interview or asking the admin multiple setup questions:

1. Greet the admin briefly and describe what you can do (coordinate workers, manage tasks, run multi-agent projects)
2. Use "%s" as your default communication language (the user's install-time preference); you may switch languages if the admin addresses you in a different one
3. The user's timezone is %s — use it to interpret schedules and deadlines they mention
4. Write a minimal ~/SOUL.md using sensible defaults (default name, direct and concise communication style, no special behavior constraints), then touch ~/soul-configured
5. Let the admin know they can adjust your name or style at any time just by asking

The human admin will start chatting shortly.`, language, timezone, language, timezone)
}

// DeprovisionManager cleans up infrastructure for a deleted Manager.
func (p *Provisioner) DeprovisionManager(ctx context.Context, name string) error {
	logger := log.FromContext(ctx)
	consumerName := "manager"

	if err := p.gateway.DeauthorizeAIRoutes(ctx, consumerName, ""); err != nil {
		logger.Error(err, "failed to deauthorize AI routes (non-fatal)")
	}
	if err := p.gateway.DeleteConsumer(ctx, consumerName); err != nil {
		logger.Error(err, "failed to delete gateway consumer (non-fatal)")
	}

	if p.ossAdmin != nil {
		if err := p.ossAdmin.DeleteUser(ctx, name); err != nil {
			logger.Error(err, "failed to delete MinIO user (non-fatal)")
		}
	}

	return nil
}

// CredentialNames returns all credential store keys (worker/manager names).
