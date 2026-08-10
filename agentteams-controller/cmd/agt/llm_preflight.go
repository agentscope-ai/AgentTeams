package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultOpenAICompatibleBaseURL  = "https://api.openai.com/v1"
	defaultQwenCompatibleBaseURL    = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	defaultLLMPreflightTimeout      = 30 * time.Second
	defaultLLMPreflightRetries      = 2
	defaultLLMPreflightRetryBackoff = 500 * time.Millisecond
)

type llmPreflightOptions struct {
	Provider string
	APIKey   string
	BaseURL  string
	Model    string
	Timeout  time.Duration
	Retries  int
	Strict   bool

	HTTPClient   *http.Client
	RetryBackoff time.Duration
}

type llmPreflightConfig struct {
	Provider string
	APIKey   string
	BaseURL  string
	Model    string
	Timeout  time.Duration
	Retries  int
	Backoff  time.Duration
}

type llmPreflightStatusError struct {
	StatusCode int
	Body       string
	APIKey     string
}

func (e *llmPreflightStatusError) Error() string {
	body := sanitizePreflightBody(e.Body, e.APIKey)
	if body == "" {
		return fmt.Sprintf("LLM preflight failed with HTTP %d: %s", e.StatusCode, preflightStatusHint(e.StatusCode))
	}
	return fmt.Sprintf("LLM preflight failed with HTTP %d: %s. Response body: %s",
		e.StatusCode, preflightStatusHint(e.StatusCode), body)
}

type llmPreflightTransportError struct {
	URL string
	Err error
}

func (e *llmPreflightTransportError) Error() string {
	return fmt.Sprintf("LLM preflight request to %s failed: %v", e.URL, e.Err)
}

func (e *llmPreflightTransportError) Unwrap() error {
	return e.Err
}

func llmPreflightCmd() *cobra.Command {
	opts := llmPreflightOptionsFromEnv()
	timeoutSeconds := int(opts.Timeout.Seconds())

	cmd := &cobra.Command{
		Use:   "llm-preflight",
		Short: "Validate LLM API key, base URL, and model before startup",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Timeout = time.Duration(timeoutSeconds) * time.Second
			if err := runLLMPreflight(cmd.Context(), opts); err != nil {
				if !opts.Strict {
					fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %v\n", err)
					return nil
				}
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "LLM preflight passed")
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.Provider, "provider", opts.Provider, "LLM provider (openai-compat|qwen|custom)")
	cmd.Flags().StringVar(&opts.APIKey, "api-key", opts.APIKey, "LLM API key")
	cmd.Flags().StringVar(&opts.BaseURL, "base-url", opts.BaseURL, "OpenAI-compatible base URL")
	cmd.Flags().StringVar(&opts.Model, "model", opts.Model, "Model name to probe")
	cmd.Flags().IntVar(&timeoutSeconds, "timeout", timeoutSeconds, "HTTP timeout in seconds")
	cmd.Flags().IntVar(&opts.Retries, "retries", opts.Retries, "Retry count for transient failures")
	cmd.Flags().BoolVar(&opts.Strict, "strict", opts.Strict, "Return non-zero when preflight fails")
	return cmd
}

func llmPreflightOptionsFromEnv() llmPreflightOptions {
	return llmPreflightOptions{
		Provider: envOrDefaultLocal("AGENTTEAMS_LLM_PROVIDER", "openai-compat"),
		APIKey:   os.Getenv("AGENTTEAMS_LLM_API_KEY"),
		BaseURL:  os.Getenv("AGENTTEAMS_OPENAI_BASE_URL"),
		Model:    os.Getenv("AGENTTEAMS_DEFAULT_MODEL"),
		Timeout: time.Duration(envIntDefaultLocal(
			"AGENTTEAMS_LLM_PREFLIGHT_TIMEOUT_SECONDS",
			int(defaultLLMPreflightTimeout.Seconds()),
		)) * time.Second,
		Retries: envIntDefaultLocal("AGENTTEAMS_LLM_PREFLIGHT_RETRIES", defaultLLMPreflightRetries),
		Strict:  envBoolDefaultLocal("AGENTTEAMS_LLM_PREFLIGHT_STRICT", true),
	}
}

func resolveLLMPreflightConfig(opts llmPreflightOptions) (llmPreflightConfig, error) {
	cfg := llmPreflightConfig{
		Provider: strings.TrimSpace(opts.Provider),
		APIKey:   strings.TrimSpace(opts.APIKey),
		BaseURL:  strings.TrimSpace(opts.BaseURL),
		Model:    strings.TrimSpace(opts.Model),
		Timeout:  opts.Timeout,
		Retries:  opts.Retries,
		Backoff:  opts.RetryBackoff,
	}
	if cfg.Provider == "" {
		cfg.Provider = "openai-compat"
	}
	if cfg.APIKey == "" {
		return cfg, fmt.Errorf("LLM API key is required (set AGENTTEAMS_LLM_API_KEY or --api-key)")
	}
	if cfg.Model == "" {
		return cfg, fmt.Errorf("LLM model is required (set AGENTTEAMS_DEFAULT_MODEL or --model)")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultLLMPreflightTimeout
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Backoff == 0 {
		cfg.Backoff = defaultLLMPreflightRetryBackoff
	}
	if cfg.Backoff < 0 {
		cfg.Backoff = 0
	}

	baseURL, err := resolveLLMPreflightBaseURL(cfg.Provider, cfg.BaseURL)
	if err != nil {
		return cfg, err
	}
	cfg.BaseURL = baseURL
	return cfg, nil
}

func resolveLLMPreflightBaseURL(provider, baseURL string) (string, error) {
	provider = strings.TrimSpace(provider)
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		switch provider {
		case "", "openai-compat", "openai":
			baseURL = defaultOpenAICompatibleBaseURL
		case "qwen":
			baseURL = defaultQwenCompatibleBaseURL
		default:
			return "", fmt.Errorf("LLM base URL is required for provider %q (set AGENTTEAMS_OPENAI_BASE_URL or --base-url)", provider)
		}
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid LLM base URL %q: %w", baseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid LLM base URL %q: scheme must be http or https", baseURL)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid LLM base URL %q: host is required", baseURL)
	}
	return strings.TrimRight(baseURL, "/"), nil
}

func runLLMPreflight(ctx context.Context, opts llmPreflightOptions) error {
	cfg, err := resolveLLMPreflightConfig(opts)
	if err != nil {
		return err
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}

	var lastErr error
	for attempt := 0; attempt <= cfg.Retries; attempt++ {
		lastErr = runLLMPreflightAttempt(ctx, client, cfg)
		if lastErr == nil {
			return nil
		}
		if !isRetryableLLMPreflightError(lastErr) || attempt == cfg.Retries {
			break
		}
		if err := waitLLMPreflightRetryBackoff(ctx, cfg.Backoff, attempt); err != nil {
			return err
		}
	}
	return lastErr
}

func runLLMPreflightAttempt(ctx context.Context, client *http.Client, cfg llmPreflightConfig) error {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	payload := map[string]interface{}{
		"model":      cfg.Model,
		"max_tokens": 1,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with only one word: ok"},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal LLM preflight request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build LLM preflight request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AgentTeams/llm-preflight")

	resp, err := client.Do(req)
	if err != nil {
		return &llmPreflightTransportError{URL: endpoint, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &llmPreflightStatusError{
		StatusCode: resp.StatusCode,
		Body:       string(body),
		APIKey:     cfg.APIKey,
	}
}

func isRetryableLLMPreflightError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var transportErr *llmPreflightTransportError
	if errors.As(err, &transportErr) {
		return true
	}
	var statusErr *llmPreflightStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode >= 500
	}
	return false
}

func waitLLMPreflightRetryBackoff(ctx context.Context, baseDelay time.Duration, attempt int) error {
	if baseDelay <= 0 {
		return nil
	}
	delay := baseDelay
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func preflightStatusHint(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "model name or request format was rejected"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "API key is invalid, lacks permission, or the model is not enabled"
	case http.StatusNotFound:
		return "base URL path or model endpoint may be incorrect"
	case http.StatusTooManyRequests:
		return "quota is exhausted or the provider is rate limiting requests"
	default:
		if status >= 500 {
			return "provider service is unavailable"
		}
		return "provider rejected the preflight request"
	}
}

func sanitizePreflightBody(body, apiKey string) string {
	body = strings.TrimSpace(body)
	if apiKey != "" {
		body = strings.ReplaceAll(body, apiKey, "[REDACTED]")
	}
	if len(body) > 1000 {
		body = body[:1000] + "...(truncated)"
	}
	return body
}

func envOrDefaultLocal(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envIntDefaultLocal(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

func envBoolDefaultLocal(key string, defaultVal bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "y", "on":
			return true
		case "0", "false", "no", "n", "off":
			return false
		}
	}
	return defaultVal
}
