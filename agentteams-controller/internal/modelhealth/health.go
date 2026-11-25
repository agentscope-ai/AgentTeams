// Package modelhealth consumes Harness-owned model checks. It never calls a model API.
package modelhealth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"time"
)

const MaxAge = 10 * time.Minute

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var requestPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var runtimePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Report is shared by all Harnesses and deployment backends. Model health is
// independent from container liveness and Matrix readiness.
type Report struct {
	Version       int       `json:"version"`
	Runtime       string    `json:"runtime"`
	Member        string    `json:"member"`
	Configuration string    `json:"configuration"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason"`
	CheckedAt     time.Time `json:"checked_at"`
	RequestID     string    `json:"request_id,omitempty"`
}
type Request struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Config struct{ Runtime, Admin, Room string }
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

func Unknown(reason string) Report { return Report{Version: 1, Status: "unknown", Reason: reason} }

// Parse rejects stale, foreign or malformed reports without exposing their contents.
func Parse(data []byte, runtime, member string, now time.Time) Report {
	var r Report
	if len(data) > 16384 || json.Unmarshal(data, &r) != nil || r.Version != 1 || r.Member != member || !runtimePattern.MatchString(r.Runtime) || (runtime != "" && r.Runtime != runtime) {
		return Unknown("invalid_report")
	}
	if (r.Configuration != "" && !digestPattern.MatchString(r.Configuration)) || (r.RequestID != "" && !requestPattern.MatchString(r.RequestID)) {
		return Unknown("invalid_report")
	}
	if r.CheckedAt.IsZero() || r.CheckedAt.After(now.Add(30*time.Second)) {
		return Unknown("invalid_report")
	}
	if now.Sub(r.CheckedAt) > MaxAge {
		return Unknown("stale_report")
	}
	if r.Status == "unknown" && (r.Reason == "configuration" || r.Reason == "disabled" || r.Reason == "unsupported") {
		return r
	}
	if r.Status == "ok" && r.Reason == "ok" {
		return r
	}
	if r.Status == "failed" {
		switch r.Reason {
		case "authentication", "billing_or_quota", "model_or_endpoint", "rate_limit_or_quota", "upstream_unavailable", "configuration", "timeout", "network", "invalid_response", "empty_response":
			return r
		}
	}
	return Unknown("invalid_report")
}

// Check persists notice identity before sending and consumes only safe report fields.
func Check(ctx context.Context, store Store, key string, cfg Config, report Report, notify Notify) error {
	var s State
	b, err := store.GetObject(ctx, key)
	if err != nil && !os.IsNotExist(err) {
		return errors.New("model health state unavailable")
	}
	if err == nil && json.Unmarshal(b, &s) != nil {
		return errors.New("model health state invalid")
	}
	raw, _ := json.Marshal([]string{cfg.Runtime, cfg.Admin, cfg.Room, report.Configuration})
	sum := sha256.Sum256(raw)
	fp := hex.EncodeToString(sum[:])
	previous := s.Status
	hadFailure := s.Transaction != ""
	if report.Status != "unknown" && s.Fingerprint != fp {
		s = State{Fingerprint: fp}
	}
	s.Status, s.Reason, s.CheckedAt = report.Status, report.Reason, report.CheckedAt
	if report.Status == "ok" {
		if previous == "failed" || hadFailure {
			now := time.Now().UTC()
			s.RecoveredAt = &now
		}
		s.Transaction, s.NoticeStatus, s.Notified = "", "", false
	} else if report.Status == "failed" {
		if previous == "ok" {
			s.Transaction, s.Notified = "", false
		}
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
	if report.Status == "failed" && !s.Notified && cfg.Room != "" && cfg.Admin != "" && notify != nil {
		body := "AgentTeams Harness model check failed (" + report.Reason + "). Check the Harness model configuration and gateway connectivity. Diagnostic status: model-health.json."
		if notify(ctx, cfg.Room, s.Transaction, body) != nil {
			s.NoticeStatus = "delivery_failed"
			return save()
		}
		s.Notified, s.NoticeStatus = true, "delivered"
		return save()
	}
	return nil
}
