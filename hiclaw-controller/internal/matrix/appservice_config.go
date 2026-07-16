package matrix

import (
	"crypto/rand"
	"encoding/hex"
)

// AppserviceConfig holds the configuration for registering the controller
// as a Matrix Application Service with the homeserver (Conduwuit/Tuwunel).
type AppserviceConfig struct {
	Enabled bool
	ID      string // e.g. "agentteams-watcher"
	ASToken string // appservice → homeserver authentication token
	HSToken string // homeserver → appservice authentication token
	URL     string // controller HTTP endpoint reachable from homeserver
}

// EnsureTokens fills in any empty token with a random 64-byte hex string.
// Safe to call multiple times; already-set values are left untouched.
func (c *AppserviceConfig) EnsureTokens() {
	if c.ASToken == "" {
		c.ASToken = randomHex(32)
	}
	if c.HSToken == "" {
		c.HSToken = randomHex(32)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
