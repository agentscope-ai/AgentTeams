package initializer

import "testing"

func TestCustomOpenAIURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ip port with path", "http://10.43.46.12:3000/v1", "http://10.43.46.12/v1"},
		{"ip port no path", "http://10.43.46.12:3000", "http://10.43.46.12"},
		{"https port with path", "https://example.com:8443/v1", "https://example.com/v1"},
		{"no port keeps url", "https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"unparseable returns input", "://bad", "://bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := customOpenAIURL(tc.in); got != tc.want {
				t.Fatalf("customOpenAIURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
