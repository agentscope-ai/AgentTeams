// Package modelhealth probes the configured data plane without running an agent.
// Only fixed diagnostic categories leave this package; upstream bodies and URLs
// (which may contain credentials) must never be written to logs or storage.
package modelhealth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct{ URL, Key, Model, Protocol, Admin, Room, Runtime string }
type State struct {
	Fingerprint  string     `json:"fingerprint"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason"`
	CheckedAt    time.Time  `json:"checked_at"`
	RecoveredAt  *time.Time `json:"recovered_at,omitempty"`
	Transaction  string     `json:"transaction,omitempty"`
	Notified     bool       `json:"notified"`
	NoticeStatus string     `json:"notice_status,omitempty"`
}
type Store interface {
	GetObject(context.Context, string) ([]byte, error)
	PutObject(context.Context, string, []byte) error
}
type Notify func(context.Context, string, string, string) error

// Check persists a pending transaction before sending, then marks delivery only
// after Matrix acknowledgement. Retrying the same transaction is idempotent even
// when Matrix accepted a request whose HTTP response was lost.
func Check(ctx context.Context, store Store, key string, cfg Config, client *http.Client, notify Notify) error {
	var s State
	b, err := store.GetObject(ctx, key)
	if err != nil && !os.IsNotExist(err) {
		return errors.New("model health state unavailable")
	}
	if err == nil && json.Unmarshal(b, &s) != nil {
		return errors.New("model health state invalid")
	}
	raw, _ := json.Marshal(cfg)
	sum := sha256.Sum256(raw)
	fp := hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	// Success expires after five minutes; failures retry inference at most once
	// per minute. Pending notice delivery may retry on every reconcile.
	if s.Fingerprint == fp && s.Status == "ok" && now.Sub(s.CheckedAt) < 5*time.Minute {
		return nil
	}
	reason := s.Reason
	fresh := s.Fingerprint != fp || s.CheckedAt.IsZero() || now.Sub(s.CheckedAt) >= time.Minute
	if fresh {
		reason = Probe(ctx, client, cfg)
	}
	previous := s.Status
	if s.Fingerprint != fp {
		s = State{Fingerprint: fp}
	}
	if fresh {
		s.CheckedAt = now
	}
	s.Reason = reason
	if reason == "ok" {
		s.Status = "ok"
		if previous == "failed" {
			s.RecoveredAt = &now
		}
		s.Transaction = ""
		s.Notified = false
		s.NoticeStatus = ""
	} else {
		if s.Status != "failed" {
			s.Transaction = ""
			s.Notified = false
		}
		s.Status = "failed"
		if s.Transaction == "" {
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return errors.New("model health transaction unavailable")
			}
			s.Transaction = "model-health-" + hex.EncodeToString(id[:])
		}
		if cfg.Room == "" || cfg.Admin == "" {
			s.NoticeStatus = "missing_admin_dm"
		} else if !s.Notified {
			s.NoticeStatus = "pending"
		}
	}
	save := func() error {
		data, _ := json.Marshal(s)
		if store.PutObject(ctx, key, data) != nil {
			return errors.New("model health state write failed")
		}
		return nil
	}
	if err := save(); err != nil {
		return err
	}
	if s.Status == "failed" && !s.Notified && cfg.Room != "" && cfg.Admin != "" && notify != nil {
		// Fixed body deliberately excludes even the model name: custom IDs may contain secrets.
		body := "AgentTeams model preflight failed (" + reason + "). Check model credentials, quota, protocol and gateway connectivity. Diagnostic status: model-health.json."
		if notify(ctx, cfg.Room, s.Transaction, body) != nil {
			s.NoticeStatus = "delivery_failed"
			return save()
		}
		s.Notified = true
		s.NoticeStatus = "delivered"
		return save()
	}
	return nil
}

func Probe(ctx context.Context, client *http.Client, cfg Config) string {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || cfg.Model == "" || cfg.Key == "" {
		return "configuration"
	}
	payload := map[string]any{"model": cfg.Model, "max_tokens": 256, "messages": []map[string]string{{"role": "user", "content": "Reply with one word: ok"}}, "stream": false}
	path := "/chat/completions"
	switch cfg.Protocol {
	case "anthropic-messages":
		path = "/messages"
	case "chat", "openai-completions":
	case "responses", "openai-responses":
		path = "/responses"
		payload = map[string]any{"model": cfg.Model, "input": "Reply with one word: ok", "max_output_tokens": 256, "stream": false, "store": false}
	default:
		return "unsupported_protocol"
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.URL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return "configuration"
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	if cfg.Protocol == "anthropic-messages" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req = req.WithContext(attemptCtx)
	resp, err := copyClient.Do(req)
	if err != nil {
		return transportReason(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil {
		return transportReason(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case 401, 403:
			return "authentication"
		case 402:
			return "billing_or_quota"
		case 404:
			return "model_or_endpoint"
		case 429:
			return "rate_limit_or_quota"
		default:
			if resp.StatusCode >= 500 {
				return "upstream_unavailable"
			}
			return "configuration"
		}
	}
	return Validate(b, cfg.Protocol)
}
func transportReason(err error) string {
	var dns *net.DNSError
	var tlsErr *tls.CertificateVerificationError
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &tlsErr):
		return "tls"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	default:
		return "network"
	}
}
func Validate(b []byte, protocol string) string {
	var r struct {
		Error    json.RawMessage `json:"error"`
		BaseResp struct {
			Code int `json:"status_code"`
		} `json:"base_resp"`
		Status  string `json:"status"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if len(b) > 65536 || json.Unmarshal(b, &r) != nil {
		return "invalid_response"
	}
	if (len(r.Error) > 0 && string(r.Error) != "null") || r.BaseResp.Code != 0 {
		// Inspect in memory only, never return any provider-controlled string.
		text := strings.ToLower(string(b))
		switch {
		case r.BaseResp.Code == 1008 || strings.Contains(text, "balance") || strings.Contains(text, "quota"):
			return "billing_or_quota"
		case strings.Contains(text, "unauthorized") || strings.Contains(text, "api_key") || strings.Contains(text, "authentication"):
			return "authentication"
		default:
			return "provider_error"
		}
	}
	if protocol == "anthropic-messages" {
		if r.Type != "message" || r.Role != "assistant" {
			return "invalid_response"
		}
		for _, block := range r.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				return "ok"
			}
		}
		return "empty_response"
	}
	if protocol == "responses" || protocol == "openai-responses" {
		if r.Status != "completed" && r.Status != "incomplete" {
			return "invalid_response"
		}
		for _, o := range r.Output {
			for _, c := range o.Content {
				if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
					return "ok"
				}
			}
		}
	} else {
		for _, c := range r.Choices {
			if strings.TrimSpace(c.Message.Content) != "" {
				return "ok"
			}
		}
	}
	return "empty_response"
}
