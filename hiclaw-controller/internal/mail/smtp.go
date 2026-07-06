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

// ConfigFromEnv reads SMTP config from HICLAW_SMTP_* environment variables.
func ConfigFromEnv() *Config {
	host := os.Getenv("HICLAW_SMTP_HOST")
	if host == "" {
		return nil
	}
	return &Config{
		Host: host,
		Port: envOrDefault("HICLAW_SMTP_PORT", "465"),
		User: os.Getenv("HICLAW_SMTP_USER"),
		Pass: os.Getenv("HICLAW_SMTP_PASS"),
		From: envOrDefault("HICLAW_SMTP_FROM", "HiClaw <noreply@hiclaw.io>"),
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

— HiClaw`, sanitizeHeaderField(displayName), sanitizeHeaderField(matrixUserID), sanitizeHeaderField(password), sanitizeHeaderField(elementURL))

	// to/from/subject are interpolated directly into CRLF-joined header
	// lines below; a caller-supplied value containing \r or \n could inject
	// extra headers or SMTP commands. Strip CR/LF from every value that
	// lands in a header line before building msg.
	safeTo := sanitizeHeaderField(to)
	safeFrom := sanitizeHeaderField(cfg.From)
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
