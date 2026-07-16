package mail

import (
	"fmt"
	"net/smtp"
	"os"
	"strings"
)

// Config holds SMTP configuration from environment variables.
type Config struct {
	Host string
	Port string
	User string
	Pass string
	From string
}

// ConfigFromEnv reads SMTP config from AGENTTEAMS_SMTP_* environment variables.
func ConfigFromEnv() *Config {
	host := os.Getenv("AGENTTEAMS_SMTP_HOST")
	if host == "" {
		return nil
	}
	return &Config{
		Host: host,
		Port: envOrDefault("AGENTTEAMS_SMTP_PORT", "465"),
		User: os.Getenv("AGENTTEAMS_SMTP_USER"),
		Pass: os.Getenv("AGENTTEAMS_SMTP_PASS"),
		From: envOrDefault("AGENTTEAMS_SMTP_FROM", "AgentTeams <noreply@agentteams.io>"),
	}
}

// SendWelcome sends a welcome email to a newly created human user.
func SendWelcome(cfg *Config, to, displayName, matrixUserID, password, elementURL string) error {
	if cfg == nil {
		return fmt.Errorf("SMTP not configured")
	}

	subject := "Welcome to AgentTeams - Your Account Details"
	body := fmt.Sprintf(`Hi %s,

Your AgentTeams account has been created:

  Username: %s
  Password: %s
  Login URL: %s

Please log in using Element Web and change your password immediately.

— AgentTeams`, displayName, matrixUserID, password, elementURL)

	// Sanitize every field interpolated into a header line (or preceding the
	// body) to strip CR/LF, preventing header/SMTP-command injection via a
	// crafted display name, email address, or subject.
	safeFrom := sanitizeHeaderField(cfg.From)
	safeTo := sanitizeHeaderField(to)
	safeSubject := sanitizeHeaderField(subject)

	msg := strings.Join([]string{
		fmt.Sprintf("From: %s", safeFrom),
		fmt.Sprintf("To: %s", safeTo),
		fmt.Sprintf("Subject: %s", safeSubject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
	}, "\r\n")

	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	auth := smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)

	return smtp.SendMail(addr, auth, cfg.From, []string{safeTo}, []byte(msg))
}

// sanitizeHeaderField strips CR and LF from a value that will be
// interpolated into an SMTP header line (or the plain-text body, which sits
// immediately after the header block), preventing header/SMTP-command
// injection via a crafted display name, email address, or other field.
func sanitizeHeaderField(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
