package initializer

import "testing"

func TestResolveLLMBaseURL(t *testing.T) {
	tests := []struct {
		name, provider, baseURL, want string
	}{
		{"atlascloud default", "atlascloud", "", defaultAtlasCloudBaseURL},
		{"atlascloud explicit wins", "atlascloud", "https://proxy.example.com/v1", "https://proxy.example.com/v1"},
		{"openai-compat stays empty", "openai-compat", "", ""},
		{"qwen stays empty", "qwen", "", ""},
		{"unknown provider stays empty", "custom-vendor", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveLLMBaseURL(tt.provider, tt.baseURL); got != tt.want {
				t.Fatalf("resolveLLMBaseURL(%q, %q) = %q, want %q", tt.provider, tt.baseURL, got, tt.want)
			}
		})
	}
}
