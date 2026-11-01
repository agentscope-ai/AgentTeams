package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ManagerConfigStore serializes embedded-mode updates to the Manager Agent's
// openclaw.json.
//
// All state is persisted to OSS (MinIO) so that the Manager Agent container
// can access it via mc mirror — this avoids cross-container filesystem issues.
//
// In incluster mode, construct with nil OSS — Enabled() will return false
// and all methods become no-ops.
type ManagerConfigStore struct {
	OSS          oss.StorageClient
	MatrixDomain string
	ManagerName  string // Manager agent name, default "manager"
	AgentFSDir   string // local filesystem root for agent workspaces (embedded: shared mount with manager)

	mu sync.Mutex // serializes openclaw.json read-modify-write cycles
}

// ManagerConfigStoreConfig holds configuration for constructing a ManagerConfigStore.
type ManagerConfigStoreConfig struct {
	OSS          oss.StorageClient
	MatrixDomain string
	ManagerName  string
	AgentFSDir   string
}

func NewManagerConfigStore(cfg ManagerConfigStoreConfig) *ManagerConfigStore {
	managerName := cfg.ManagerName
	if managerName == "" {
		managerName = "manager"
	}
	return &ManagerConfigStore{
		OSS:          cfg.OSS,
		MatrixDomain: cfg.MatrixDomain,
		ManagerName:  managerName,
		AgentFSDir:   cfg.AgentFSDir,
	}
}

// Enabled reports whether Manager config storage is configured.
func (l *ManagerConfigStore) Enabled() bool {
	return l != nil && l.OSS != nil
}

// MatrixUserID builds a full Matrix user ID from a localpart username.
func (l *ManagerConfigStore) MatrixUserID(name string) string {
	return fmt.Sprintf("@%s:%s", name, l.MatrixDomain)
}

func (l *ManagerConfigStore) managerAgentPrefix() string {
	return fmt.Sprintf("agents/%s", l.ManagerName)
}

// managerLocalConfigPath returns the local filesystem path for the manager's openclaw.json.
// In embedded mode, this is a shared mount with the manager container.
func (l *ManagerConfigStore) managerLocalConfigPath() string {
	if l.AgentFSDir == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/openclaw.json", l.AgentFSDir, l.ManagerName)
}

// writeManagerLocalConfig writes openclaw.json to the local filesystem (shared
// mount) only when no local config exists yet. The Manager's start script owns
// the live file after first boot: replacing it from the Controller on every
// reconcile changes the authored revision under OpenClaw's startup config
// guard and can trap the Manager in a restart loop.
func (l *ManagerConfigStore) writeManagerLocalConfig(data []byte) {
	path := l.managerLocalConfigPath()
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return
	} else if !os.IsNotExist(err) {
		fmt.Printf("warning: cannot inspect manager local config %s: %v\n", path, err)
		return
	}
	if err := writeManagerLocalConfigAtomic(path, data); err != nil {
		// Non-fatal: MinIO is the source of truth
		fmt.Printf("warning: failed to write manager local config %s: %v\n", path, err)
	}
}

// writeManagerLocalConfigAtomic replaces the shared Manager config without
// exposing a truncated/invalid file to OpenClaw's config reader.
func writeManagerLocalConfigAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".openclaw.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// --- Manager Config ---

// PutManagerConfig writes the Manager's openclaw.json to OSS, merging the
// new config with any existing groupAllowFrom entries to avoid overwriting
// additions made by UpdateManagerGroupAllowFrom (e.g. team leader IDs).
func (l *ManagerConfigStore) PutManagerConfig(configJSON []byte) error {
	if !l.Enabled() {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ctx := context.Background()
	key := l.managerAgentPrefix() + "/openclaw.json"

	// Read existing config to preserve user customizations on top of the
	// generated defaults: groupAllowFrom additions plus user-modified plugin
	// entries (e.g. memory-core dreaming schedule, extra load paths).
	existingData, err := l.OSS.GetObject(ctx, key)
	if err == nil && len(existingData) > 0 {
		var existingCfg map[string]interface{}
		var newCfg map[string]interface{}
		if json.Unmarshal(existingData, &existingCfg) == nil && json.Unmarshal(configJSON, &newCfg) == nil {
			mergeGroupAllowFrom(existingCfg, newCfg)
			if merged, mErr := json.MarshalIndent(newCfg, "", "  "); mErr == nil {
				configJSON = merged
			}
		}
		if pluginMerged, pErr := mergeUserPluginConfig(configJSON, existingData); pErr != nil {
			log.Log.WithName("manager-config").Error(pErr, "plugin config merge failed, using generated config", "key", key)
		} else {
			configJSON = pluginMerged
		}
	}

	if err := l.OSS.PutObject(ctx, key, configJSON); err != nil {
		return err
	}
	// First boot may need the generated file on the shared workspace, but an
	// existing local file is authoritative and must not be replaced here.
	l.writeManagerLocalConfig(configJSON)
	return nil
}

// mergeGroupAllowFrom copies any extra groupAllowFrom entries from old config
// into new config, preserving IDs added by UpdateManagerGroupAllowFrom.
func mergeGroupAllowFrom(oldCfg, newCfg map[string]interface{}) {
	oldChannels, _ := oldCfg["channels"].(map[string]interface{})
	newChannels, _ := newCfg["channels"].(map[string]interface{})
	if oldChannels == nil || newChannels == nil {
		return
	}
	oldMatrix, _ := oldChannels["matrix"].(map[string]interface{})
	newMatrix, _ := newChannels["matrix"].(map[string]interface{})
	if oldMatrix == nil || newMatrix == nil {
		return
	}

	oldAllow := extractStringSlice(oldMatrix["groupAllowFrom"])
	newAllow := extractStringSlice(newMatrix["groupAllowFrom"])

	// Add any entries from old that are missing in new
	for _, id := range oldAllow {
		found := false
		for _, nid := range newAllow {
			if nid == id {
				found = true
				break
			}
		}
		if !found {
			newAllow = append(newAllow, id)
		}
	}
	newMatrix["groupAllowFrom"] = newAllow
}

// UpdateManagerGroupAllowFrom adds or removes a worker Matrix ID from the
// Manager's openclaw.json groupAllowFrom list via OSS.
func (l *ManagerConfigStore) UpdateManagerGroupAllowFrom(workerMatrixID string, add bool) error {
	if !l.Enabled() {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ctx := context.Background()
	key := l.managerAgentPrefix() + "/openclaw.json"

	data, err := l.OSS.GetObject(ctx, key)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read manager config from OSS: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse manager config: %w", err)
	}

	channels, _ := config["channels"].(map[string]interface{})
	if channels == nil {
		return nil
	}
	matrixCfg, _ := channels["matrix"].(map[string]interface{})
	if matrixCfg == nil {
		return nil
	}

	if updateGroupAllowFrom(matrixCfg, workerMatrixID, add) {
		out, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal manager config: %w", err)
		}
		if err := l.OSS.PutObject(ctx, key, out); err != nil {
			return err
		}
	}
	// Keep an already-running Manager's local allowlist in sync without
	// replacing its entire local config (which may contain Manager-owned
	// fields and customizations).
	l.updateManagerLocalGroupAllowFrom(workerMatrixID, add)
	return nil
}

// updateGroupAllowFrom applies an allowlist mutation and reports whether it
// changed the slice. Keeping the local and OSS paths on one mutator prevents
// the two copies from drifting.
func updateGroupAllowFrom(matrixCfg map[string]interface{}, workerMatrixID string, add bool) bool {
	allowList := extractStringSlice(matrixCfg["groupAllowFrom"])
	if add {
		for _, id := range allowList {
			if id == workerMatrixID {
				return false
			}
		}
		allowList = append(allowList, workerMatrixID)
	} else {
		filtered := make([]string, 0, len(allowList))
		for _, id := range allowList {
			if id != workerMatrixID {
				filtered = append(filtered, id)
			}
		}
		if len(filtered) == len(allowList) {
			return false
		}
		allowList = filtered
	}
	matrixCfg["groupAllowFrom"] = allowList
	return true
}

// updateManagerLocalGroupAllowFrom mutates only the allowlist in the live
// local Manager config, preserving Manager-owned fields and customizations.
func (l *ManagerConfigStore) updateManagerLocalGroupAllowFrom(workerMatrixID string, add bool) {
	path := l.managerLocalConfigPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return
	}
	channels, _ := config["channels"].(map[string]interface{})
	if channels == nil {
		return
	}
	matrixCfg, _ := channels["matrix"].(map[string]interface{})
	if matrixCfg == nil || !updateGroupAllowFrom(matrixCfg, workerMatrixID, add) {
		return
	}
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return
	}
	if err := writeManagerLocalConfigAtomic(path, out); err != nil {
		fmt.Printf("warning: failed to update manager local allowlist %s: %v\n", path, err)
	}
}

func extractStringSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch arr := v.(type) {
	case []interface{}:
		result := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return arr
	}
	return nil
}
