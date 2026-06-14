package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveLLMPreflightConfigDefaultsOpenAICompatible(t *testing.T) {
	cfg, err := resolveLLMPreflightConfig(llmPreflightOptions{
		Provider: "openai-compat",
		APIKey:   "sk-test",
		Model:    "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("resolveLLMPreflightConfig: %v", err)
	}
	if cfg.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("BaseURL=%q, want official OpenAI endpoint", cfg.BaseURL)
	}
}

func TestResolveLLMPreflightConfigDefaultsQwen(t *testing.T) {
	cfg, err := resolveLLMPreflightConfig(llmPreflightOptions{
		Provider: "qwen",
		APIKey:   "dashscope-test",
		Model:    "qwen3.5-plus",
	})
	if err != nil {
		t.Fatalf("resolveLLMPreflightConfig: %v", err)
	}
	if cfg.BaseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Fatalf("BaseURL=%q, want DashScope compatible endpoint", cfg.BaseURL)
	}
}

func TestResolveLLMPreflightConfigCustomProviderRequiresBaseURL(t *testing.T) {
	_, err := resolveLLMPreflightConfig(llmPreflightOptions{
		Provider: "custom-vendor",
		APIKey:   "sk-test",
		Model:    "custom-model",
	})
	if err == nil || !strings.Contains(err.Error(), "base URL is required") {
		t.Fatalf("error=%v, want base URL requirement", err)
	}
}

func TestResolveLLMPreflightConfigRejectsMissingAPIKey(t *testing.T) {
	_, err := resolveLLMPreflightConfig(llmPreflightOptions{
		Provider: "openai-compat",
		Model:    "gpt-5.4",
	})
	if err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("error=%v, want API key requirement", err)
	}
}

func TestRunLLMPreflightPostsChatCompletion(t *testing.T) {
	const apiKey = "sk-sensitive"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Fatalf("Authorization=%q", got)
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Model != "test-model" {
			t.Fatalf("model=%q, want test-model", body.Model)
		}
		if body.MaxTokens != 1 {
			t.Fatalf("max_tokens=%d, want 1", body.MaxTokens)
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
			t.Fatalf("messages=%+v", body.Messages)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	err := runLLMPreflight(context.Background(), llmPreflightOptions{
		Provider: "custom",
		APIKey:   apiKey,
		BaseURL:  srv.URL + "/v1",
		Model:    "test-model",
		Timeout:  time.Second,
		Retries:  0,
	})
	if err != nil {
		t.Fatalf("runLLMPreflight: %v", err)
	}
}

func TestRunLLMPreflightStatusErrorIsActionableAndRedacted(t *testing.T) {
	const apiKey = "sk-do-not-print"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key sk-do-not-print"}`))
	}))
	defer srv.Close()

	err := runLLMPreflight(context.Background(), llmPreflightOptions{
		Provider: "custom",
		APIKey:   apiKey,
		BaseURL:  srv.URL + "/v1",
		Model:    "test-model",
		Timeout:  time.Second,
	})
	if err == nil {
		t.Fatal("runLLMPreflight returned nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") || !strings.Contains(msg, "API key") {
		t.Fatalf("error=%q, want actionable 401 API key message", msg)
	}
	if strings.Contains(msg, apiKey) {
		t.Fatalf("error leaked API key: %q", msg)
	}
}

func TestRunLLMPreflightRetriesTransientFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`bad gateway`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := runLLMPreflight(context.Background(), llmPreflightOptions{
		Provider:     "custom",
		APIKey:       "sk-test",
		BaseURL:      srv.URL + "/v1",
		Model:        "test-model",
		Timeout:      time.Second,
		Retries:      1,
		RetryBackoff: -1,
	})
	if err != nil {
		t.Fatalf("runLLMPreflight: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls=%d, want 2", got)
	}
}

func TestRunLLMPreflightDoesNotRetryAfterContextCancellation(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
		cancel()
	}))
	defer srv.Close()

	start := time.Now()
	err := runLLMPreflight(ctx, llmPreflightOptions{
		Provider:     "custom",
		APIKey:       "sk-test",
		BaseURL:      srv.URL + "/v1",
		Model:        "test-model",
		Timeout:      time.Second,
		Retries:      1,
		RetryBackoff: 10 * time.Second,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls=%d, want 1", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("elapsed=%s, want cancellation before retry backoff finishes", elapsed)
	}
}
