package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// Tasks and ordinary Matrix messages use the same checkpoint invariants.
// The caller supplies the authoritative object's permitted artifact namespace.
func validateWorkCheckpoint(ctx context.Context, store oss.StorageClient, key, artifactPrefix string, old, cp *taskCheckpoint) error {
	version := uint64(1)
	if old != nil {
		version = old.Version + 1
	}
	if cp == nil || cp.Version != version {
		return fmt.Errorf("checkpoint version must advance from the authoritative snapshot")
	}
	if old != nil && old.Delivery == "pending" && (old.Result == nil || cp.Result == nil || *old.Result != *cp.Result || cp.ReplyTxn != old.ReplyTxn || (cp.Delivery != "pending" && cp.Delivery != "delivered")) {
		return fmt.Errorf("acknowledge delivery before changing a pending saved reply")
	}
	if unresolvedOperation(old) && (cp.Operation == nil || cp.Operation.ID != old.Operation.ID || cp.Operation.State != "verified" || strings.TrimSpace(cp.Operation.Evidence) == "") {
		return fmt.Errorf("verify the previous operation before continuing")
	}
	if op := cp.Operation; op != nil && (op.ID == "" || (op.State != "started" && op.State != "unknown" && op.State != "verified") || (op.State == "verified" && strings.TrimSpace(op.Evidence) == "")) {
		return fmt.Errorf("operation requires an identity, state and verification evidence")
	}
	if op := cp.Operation; op != nil && op.State == "verified" && (old == nil || old.Operation == nil || old.Operation.ID != op.ID) {
		return fmt.Errorf("record the operation start before verifying it")
	}
	completed := map[string]bool{}
	for _, step := range cp.CompletedSteps {
		if step == "" || completed[step] {
			return fmt.Errorf("completed steps must have unique identities")
		}
		completed[step] = true
	}
	if old != nil {
		for _, step := range old.CompletedSteps {
			if !completed[step] {
				return fmt.Errorf("a checkpoint cannot forget completed steps")
			}
		}
		retained := map[taskArtifact]bool{}
		for _, artifact := range cp.Artifacts {
			retained[artifact] = true
		}
		for _, artifact := range old.Artifacts {
			if !retained[artifact] {
				return fmt.Errorf("a checkpoint cannot discard persisted artifact references")
			}
		}
		if old.Operation != nil && old.Operation.State == "verified" && (cp.Operation == nil || (cp.Operation.ID == old.Operation.ID && *cp.Operation != *old.Operation)) {
			return fmt.Errorf("a verified operation cannot be restarted or its evidence rewritten")
		}
	}
	for _, step := range cp.RemainingSteps {
		if step == "" || completed[step] {
			return fmt.Errorf("remaining steps overlap completed work")
		}
	}
	artifacts := append([]taskArtifact{}, cp.Artifacts...)
	if cp.Result != nil {
		artifacts = append(artifacts, *cp.Result)
	}
	for _, artifact := range artifacts {
		if artifact.Size < 0 || len(artifact.SHA256) != 64 || path.Clean(artifact.Key) != artifact.Key || !strings.HasPrefix(artifact.Key, artifactPrefix) || artifact.Key == key {
			return fmt.Errorf("artifact must reference this work's persisted files")
		}
		if store == nil {
			return fmt.Errorf("artifact storage unavailable: %s", artifact.Key)
		}
		data, err := store.GetObject(ctx, artifact.Key)
		if err != nil {
			return fmt.Errorf("artifact is not durably readable: %s", artifact.Key)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != artifact.Size || hex.EncodeToString(sum[:]) != artifact.SHA256 {
			return fmt.Errorf("artifact digest mismatch: %s", artifact.Key)
		}
	}
	if cp.Delivery != "" && cp.Delivery != "pending" && cp.Delivery != "delivered" {
		return fmt.Errorf("invalid delivery state")
	}
	if cp.Delivery != "" && (cp.Result == nil || cp.ReplyTxn == "" || unresolvedOperation(cp)) {
		return fmt.Errorf("delivery requires a saved result, stable transaction and verified operations")
	}
	if cp.Delivery == "delivered" && cp.ReplyEvent == "" {
		return fmt.Errorf("delivered result requires a Matrix event")
	}
	cp.SavedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return nil
}
