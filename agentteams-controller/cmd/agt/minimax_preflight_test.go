package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type miniMaxTransport func(*http.Request) (*http.Response, error)

func (f miniMaxTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMiniMaxPreflightSerializedBoundary(t *testing.T) {
	for _, endpoint := range []string{"https://api.minimax.io/v1", "https://api.minimax.cn/v1/"} {
		for _, selected := range []string{"", "MiniMax-M3", "MiniMax-M2.7", "saved-custom"} {
			t.Run(endpoint+"/"+selected, func(t *testing.T) {
				calls := 0
				client := &http.Client{Transport: miniMaxTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.String() != strings.TrimRight(endpoint, "/")+"/chat/completions" {
						t.Fatalf("URL: %s", r.URL)
					}
					var body map[string]interface{}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					want := selected
					if want == "" {
						want = "MiniMax-M3.1-Flash"
					}
					if body["model"] != want {
						t.Fatalf("model: %v", body)
					}
					for _, key := range []string{"thinking", "reasoning_effort", "service_tier", "reasoning_split"} {
						if _, ok := body[key]; ok {
							t.Fatalf("unexpected parameter: %s", key)
						}
					}
					max := float64(1)
					if selected == "" {
						max = 4096
					}
					if body["max_tokens"] != max {
						t.Fatalf("max tokens: %v", body)
					}
					if r.Header.Get("Authorization") != "Bearer fixture" {
						t.Fatal("fixture auth not preserved")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`)), Header: make(http.Header)}, nil
				})}
				if err := runLLMPreflight(context.Background(), llmPreflightOptions{Provider: "openai-compat", APIKey: "fixture", BaseURL: endpoint, Model: selected, HTTPClient: client}); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("calls: %d", calls)
				}
			})
		}
	}
}
