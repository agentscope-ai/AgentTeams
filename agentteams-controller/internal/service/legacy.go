package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// LegacyCompat handles backward-compatible operations that only apply in
// embedded mode: Manager Agent openclaw.json manipulation, workers/teams/humans
// registry JSON files.
//
// All state is persisted to OSS (MinIO) so that the Manager Agent container
// can access it via mc mirror — this avoids cross-container filesystem issues.
//
// In incluster mode, construct with nil OSS — Enabled() will return false
// and all methods become no-ops.
type LegacyCompat struct {
	OSS          oss.StorageClient
	MatrixDomain string
	ManagerName  string // Manager agent name, default "manager"
	AgentFSDir   string // local filesystem root for agent workspaces (embedded: shared mount with manager)

	mu sync.Mutex // serializes read-modify-write cycles on registry files
}

// LegacyConfig holds configuration for constructing a LegacyCompat.
type LegacyConfig struct {
	OSS          oss.StorageClient
	MatrixDomain string
	ManagerName  string
	AgentFSDir   string
}

func NewLegacyCompat(cfg LegacyConfig) *LegacyCompat {
	managerName := cfg.ManagerName
	if managerName == "" {
		managerName = "manager"
	}
	return &LegacyCompat{
		OSS:          cfg.OSS,
		MatrixDomain: cfg.MatrixDomain,
		ManagerName:  managerName,
		AgentFSDir:   cfg.AgentFSDir,
	}
}

// Enabled reports whether legacy operations are configured.
func (l *LegacyCompat) Enabled() bool {
	return l != nil && l.OSS != nil
}

// MatrixUserID builds a full Matrix user ID from a localpart username.
func (l *LegacyCompat) MatrixUserID(name string) string {
	return fmt.Sprintf("@%s:%s", name, l.MatrixDomain)
}

func (l *LegacyCompat) managerAgentPrefix() string {
	return fmt.Sprintf("agents/%s", l.ManagerName)
}

// managerLocalConfigPath returns the local filesystem path for the manager's openclaw.json.
// In embedded mode, this is a shared mount with the manager container.
func (l *LegacyCompat) managerLocalConfigPath() string {
	if l.AgentFSDir == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/openclaw.json", l.AgentFSDir, l.ManagerName)
}

// writeManagerLocalConfig writes openclaw.json to the local filesystem (shared mount).
// This ensures the manager container sees changes immediately without MinIO sync.
func (l *LegacyCompat) writeManagerLocalConfig(data []byte) {
	path := l.managerLocalConfigPath()
	if path == "" {
		return
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		// Non-fatal: MinIO is the source of truth
		fmt.Printf("warning: failed to write manager local config %s: %v\n", path, err)
	}
}

// --- Manager Config ---

// PutManagerConfig writes the Manager's openclaw.json to OSS, merging the
// new config with any existing groupAllowFrom entries to avoid overwriting
// additions made by UpdateManagerGroupAllowFrom (e.g. team leader IDs).
func (l *LegacyCompat) PutManagerConfig(configJSON []byte) error {
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
			log.Log.WithName("legacy").Error(pErr, "plugin config merge failed, using generated config", "key", key)
		} else {
			configJSON = pluginMerged
		}
	}

	if err := l.OSS.PutObject(ctx, key, configJSON); err != nil {
		return err
	}
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
func (l *LegacyCompat) UpdateManagerGroupAllowFrom(workerMatrixID string, add bool) error {
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

	allowList := extractStringSlice(matrixCfg["groupAllowFrom"])

	if add {
		for _, id := range allowList {
			if id == workerMatrixID {
				return nil
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
		allowList = filtered
	}

	matrixCfg["groupAllowFrom"] = allowList

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manager config: %w", err)
	}
	if err := l.OSS.PutObject(ctx, key, out); err != nil {
		return err
	}
	l.writeManagerLocalConfig(out)
	return nil
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

// --- Workers Registry ---

// WorkerRegistryEntry describes a worker entry in workers-registry.json.
type WorkerRegistryEntry struct {
	Name            string   `json:"-"`
	MatrixUserID    string   `json:"matrix_user_id"`
	RoomID          string   `json:"room_id"`
	Runtime         string   `json:"runtime"`
	Deployment      string   `json:"deployment"`
	Skills          []string `json:"skills"`
	Role            string   `json:"role"`
	TeamID          *string  `json:"team_id"`
	Image           *string  `json:"image"`
	CreatedAt       string   `json:"created_at,omitempty"`
	SkillsUpdatedAt string   `json:"skills_updated_at"`
}

type workersRegistry struct {
	Version   int                            `json:"version"`
	UpdatedAt string                         `json:"updated_at"`
	Workers   map[string]WorkerRegistryEntry `json:"workers"`
}

// UpdateWorkersRegistry upserts a worker entry in workers-registry.json via OSS.
func (l *LegacyCompat) UpdateWorkersRegistry(entry WorkerRegistryEntry) error {
	if !l.Enabled() {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ctx := context.Background()
	key := l.managerAgentPrefix() + "/workers-registry.json"

	reg, err := l.loadRegistry(ctx, key, func() interface{} {
		return &workersRegistry{Version: 1, Workers: make(map[string]WorkerRegistryEntry)}
	})
	if err != nil {
		return err
	}
	wr := reg.(*workersRegistry)

	now := time.Now().UTC().Format(time.RFC3339)
	existing, exists := wr.Workers[entry.Name]
	if exists && existing.CreatedAt != "" {
		entry.CreatedAt = existing.CreatedAt
	} else {
		entry.CreatedAt = now
	}
	entry.SkillsUpdatedAt = now
	wr.Workers[entry.Name] = entry
	wr.UpdatedAt = now

	return l.saveRegistry(ctx, key, wr)
}

// RemoveFromWorkersRegistry removes a worker entry from workers-registry.json via OSS.
func (l *LegacyCompat) RemoveFromWorkersRegistry(workerName string) error {
	if !l.Enabled() {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ctx := context.Background()
	key := l.managerAgentPrefix() + "/workers-registry.json"

	reg, err := l.loadRegistry(ctx, key, func() interface{} {
		return &workersRegistry{Version: 1, Workers: make(map[string]WorkerRegistryEntry)}
	})
	if err != nil {
		return err
	}
	wr := reg.(*workersRegistry)

	delete(wr.Workers, workerName)
	wr.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	return l.saveRegistry(ctx, key, wr)
}

// --- Teams Registry ---

// TeamRegistryEntry describes a team entry in teams-registry.json.
type TeamRegistryEntry struct {
	Name           string            `json:"-"`
	Leader         string            `json:"leader"`
	Workers        []string          `json:"workers"`
	TeamRoomID     string            `json:"team_room_id"`
	LeaderDMRoomID string            `json:"leader_dm_room_id,omitempty"`
	Admin          *TeamAdminEntry   `json:"admin,omitempty"`
	Members        []TeamMemberEntry `json:"members,omitempty"`
	CreatedAt      string            `json:"created_at,omitempty"`
}

type TeamAdminEntry struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrix_user_id"`
}

type TeamMemberEntry struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrix_user_id,omitempty"`
	Role         string `json:"role,omitempty"`
}

type teamsRegistry struct {
	Version   int                          `json:"version"`
	UpdatedAt string                       `json:"updated_at"`
	Teams     map[string]TeamRegistryEntry `json:"teams"`
}

// UpdateTeamsRegistry upserts a team entry in teams-registry.json via OSS.
func (l *LegacyCompat) UpdateTeamsRegistry(entry TeamRegistryEntry) error {
	if !l.Enabled() {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ctx := context.Background()
	key := l.managerAgentPrefix() + "/teams-registry.json"

	reg, err := l.loadRegistry(ctx, key, func() interface{} {
		return &teamsRegistry{Version: 1, Teams: make(map[string]TeamRegistryEntry)}
	})
	if err != nil {
		return err
	}
	tr := reg.(*teamsRegistry)

	now := time.Now().UTC().Format(time.RFC3339)
	existing, exists := tr.Teams[entry.Name]
	if exists && existing.CreatedAt != "" {
		entry.CreatedAt = existing.CreatedAt
	} else {
		entry.CreatedAt = now
	}
	tr.Teams[entry.Name] = entry
	tr.UpdatedAt = now

	return l.saveRegistry(ctx, key, tr)
}

// RemoveFromTeamsRegistry removes a team from teams-registry.json via OSS.
func (l *LegacyCompat) RemoveFromTeamsRegistry(ctx context.Context, teamName string) error {
	if !l.Enabled() {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	key := l.managerAgentPrefix() + "/teams-registry.json"

	reg, err := l.loadRegistry(ctx, key, func() interface{} {
		return &teamsRegistry{Version: 1, Teams: make(map[string]TeamRegistryEntry)}
	})
	if err != nil {
		return err
	}
	tr := reg.(*teamsRegistry)

	delete(tr.Teams, teamName)
	tr.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	return l.saveRegistry(ctx, key, tr)
}

// --- Humans Registry ---

// HumanRegistryEntry describes a human entry in humans-registry.json.
type HumanRegistryEntry struct {
	Name            string   `json:"-"`
	MatrixUserID    string   `json:"matrix_user_id"`
	DisplayName     string   `json:"display_name"`
	PermissionLevel int      `json:"permission_level"`
	AccessibleTeams []string `json:"accessible_teams,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
}

type humansRegistry struct {
	Version   int                           `json:"version"`
	UpdatedAt string                        `json:"updated_at"`
	Humans    map[string]HumanRegistryEntry `json:"humans"`
}

// UpdateHumansRegistry upserts a human entry in humans-registry.json via OSS.
func (l *LegacyCompat) UpdateHumansRegistry(entry HumanRegistryEntry) error {
	if !l.Enabled() {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ctx := context.Background()
	key := l.managerAgentPrefix() + "/humans-registry.json"

	reg, err := l.loadRegistry(ctx, key, func() interface{} {
		return &humansRegistry{Version: 1, Humans: make(map[string]HumanRegistryEntry)}
	})
	if err != nil {
		return err
	}
	hr := reg.(*humansRegistry)

	now := time.Now().UTC().Format(time.RFC3339)
	existing, exists := hr.Humans[entry.Name]
	if exists && existing.CreatedAt != "" {
		entry.CreatedAt = existing.CreatedAt
	} else {
		entry.CreatedAt = now
	}
	hr.Humans[entry.Name] = entry
	hr.UpdatedAt = now

	return l.saveRegistry(ctx, key, hr)
}

// RemoveFromHumansRegistry removes a human from humans-registry.json via OSS.
func (l *LegacyCompat) RemoveFromHumansRegistry(ctx context.Context, humanName string) error {
	if !l.Enabled() {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	key := l.managerAgentPrefix() + "/humans-registry.json"

	reg, err := l.loadRegistry(ctx, key, func() interface{} {
		return &humansRegistry{Version: 1, Humans: make(map[string]HumanRegistryEntry)}
	})
	if err != nil {
		return err
	}
	hr := reg.(*humansRegistry)

	delete(hr.Humans, humanName)
	hr.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	return l.saveRegistry(ctx, key, hr)
}

// --- Generic OSS registry helpers ---

func (l *LegacyCompat) loadRegistry(ctx context.Context, key string, empty func() interface{}) (interface{}, error) {
	data, err := l.OSS.GetObject(ctx, key)
	if err != nil {
		if os.IsNotExist(err) {
			return empty(), nil
		}
		return nil, fmt.Errorf("read registry %s: %w", key, err)
	}

	result := empty()
	if err := json.Unmarshal(data, result); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", key, err)
	}
	return result, nil
}

func (l *LegacyCompat) saveRegistry(ctx context.Context, key string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	return l.OSS.PutObject(ctx, key, data)
}
