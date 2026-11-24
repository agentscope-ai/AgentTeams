package service

import (
	"crypto/sha256"
	"encoding/hex"
)

// TeamAssistantRecipient keeps room identity independent of the current Leader
// and distinguishes a recreated Human even when its Matrix name is reused.
func TeamAssistantRecipient(user, humanUID string) string {
	sum := sha256.Sum256([]byte(user + "\x00" + humanUID))
	return hex.EncodeToString(sum[:16])
}
