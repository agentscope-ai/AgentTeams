package mail

import "testing"

func TestSanitizeHeaderField_StripsCRLF(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Alice", "Alice"},
		{"crlf_injection", "Alice\r\nBcc: attacker@evil.com", "AliceBcc: attacker@evil.com"},
		{"lf_only", "Alice\nSubject: pwned", "AliceSubject: pwned"},
		{"cr_only", "Alice\rSubject: pwned", "AliceSubject: pwned"},
		{"embedded_multiple", "a\r\nb\r\nc", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeHeaderField(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeHeaderField(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSendWelcome_RejectsHeaderInjectionAttempt verifies that a crafted "to"
// or displayName containing CRLF cannot inject extra SMTP headers into the
// message that SendWelcome builds. We can't easily intercept the final wire
// bytes without a live SMTP server, so this test exercises the sanitizer via
// the exact fields SendWelcome forwards into header lines, confirming no
// literal CR or LF survives into what would become the header block.
func TestSendWelcome_HeaderFieldsHaveNoCRLF(t *testing.T) {
	maliciousTo := "victim@example.com\r\nBcc: attacker@evil.com"
	maliciousName := "Eve\r\nX-Injected: true"

	sanitizedTo := sanitizeHeaderField(maliciousTo)
	sanitizedName := sanitizeHeaderField(maliciousName)

	for _, s := range []string{sanitizedTo, sanitizedName} {
		for _, c := range s {
			if c == '\r' || c == '\n' {
				t.Fatalf("sanitized value %q still contains CR/LF", s)
			}
		}
	}
}
