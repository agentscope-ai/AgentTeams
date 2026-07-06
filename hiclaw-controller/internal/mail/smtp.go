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
		From: envOrDefault("AGENTTEAMS_SMTP_FROM", "HiClaw <noreply@agentteams.io>"),
	}
}

// SendWelcome sends a welcome email to a newly created human user.
func SendWelcome(cfg *Config, to, displayName, matrixUserID, password, elementURL string) error {
	if cfg == nil {
		return fmt.Errorf("SMTP not configured")
	}

	subject := "Welcome to HiClaw - Your Account Details"
	body := fmt.Sprintf(`Hi %s,

Your HiClaw account has been created:

  Username: %s
  Password: %s
  Login URL: %s

Please log in using Element Web and change your password immediately.

— HiClaw`, displayName, matrixUserID, password, elementURL)

	msg := strings.Join([]string{
		fmt.Sprintf("From: %s", cfg.From),
		fmt.Sprintf("To: %s", to),
		fmt.Sprintf("Subject: %s", subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
	}, "\r\n")

	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	auth := smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)

	return smtp.SendMail(addr, auth, cfg.From, []string{to}, []byte(msg))
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
