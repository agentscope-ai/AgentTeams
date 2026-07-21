package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	migrationMarker = "agents/manager/.migration-v1beta1-done"
)

// Migrator converts v1.0.9 registry JSON files into CR resources on controller startup.
// CRs are created with empty status so that reconcilers run handleCreate normally,
// re-provisioning infrastructure (idempotent via Ensure* methods) and starting containers.
//
// This logic is version-independent: it reconciles registry state with CR state on every
// startup. Workers/teams/humans in registry files that have no corresponding CR get created.
// The per-CR Get-before-Create check ensures idempotency — existing CRs are never touched.
type Migrator struct {
	OSS          oss.StorageClient
	RestCfg      *rest.Config
	Namespace    string
	DefaultModel string
	ManagerName  string // default "manager"
	AgentFSDir   string // local filesystem root for agent workspaces (e.g. /root/agentteams-fs/agents)
}

func (m *Migrator) managerName() string {
	if m.ManagerName != "" {
		return m.ManagerName
	}
	return "manager"
}

func (m *Migrator) Run(ctx context.Context) error {
	logger := ctrl.Log.WithName("migration")

	dynClient, err := dynamic.NewForConfig(m.RestCfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	workersReg, err := m.loadWorkersRegistry(ctx)
	if err != nil {
		return fmt.Errorf("load workers registry: %w", err)
	}
	teamsReg, err := m.loadTeamsRegistry(ctx)
	if err != nil {
		return fmt.Errorf("load teams registry: %w", err)
	}
	humansReg, err := m.loadHumansRegistry(ctx)
	if err != nil {
		return fmt.Errorf("load humans registry: %w", err)
	}

	if len(workersReg) == 0 && len(teamsReg) == 0 && len(humansReg) == 0 {
		logger.Info("no registry data to reconcile")
		return nil
	}

	// Build team lookup: workerName -> teamName
	teamByWorker := make(map[string]string)
	for teamName, entry := range teamsReg {
		teamByWorker[entry.Leader] = teamName
		for _, w := range entry.Workers {
			teamByWorker[w] = teamName
		}
	}

	// Step 1: Create standalone Worker CRs (workers not belonging to any team)
	workerRes := dynClient.Resource(workerGVR).Namespace(m.Namespace)
	for name, entry := range workersReg {
		if _, inTeam := teamByWorker[name]; inTeam {
			continue
		}
		if err := m.createStandaloneWorkerCR(ctx, workerRes, name, entry); err != nil {
			logger.Error(err, "failed to migrate standalone worker (non-fatal)", "worker", name)
		} else {
			logger.Info("migrated standalone worker", "name", name)
		}
	}

	// Step 2: Create Team CRs — TeamReconciler will create Worker CRs for team members
	for teamName, teamEntry := range teamsReg {
		if err := m.createTeamCR(ctx, dynClient, teamName, teamEntry, workersReg); err != nil {
			logger.Error(err, "failed to migrate team (non-fatal)", "team", teamName)
		} else {
			logger.Info("migrated team", "name", teamName)
		}
	}

	// Step 3: Create Human CRs
	for name, entry := range humansReg {
		if err := m.createHumanCR(ctx, dynClient, name, entry); err != nil {
			logger.Error(err, "failed to migrate human (non-fatal)", "human", name)
		} else {
			logger.Info("migrated human", "name", name)
		}
	}

	return nil
}

// --- Registry types ---

type workersRegistry struct {
	Version   int                       `json:"version"`
	UpdatedAt string                    `json:"updated_at"`
	Workers   map[string]workerRegEntry `json:"workers"`
}

type workerRegEntry struct {
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

type teamsRegistry struct {
	Version   int                     `json:"version"`
	UpdatedAt string                  `json:"updated_at"`
	Teams     map[string]teamRegEntry `json:"teams"`
}

type teamRegEntry struct {
	Leader         string        `json:"leader"`
	Workers        []string      `json:"workers"`
	TeamRoomID     string        `json:"team_room_id"`
	LeaderDMRoomID string        `json:"leader_dm_room_id,omitempty"`
	Admin          *teamAdminReg `json:"admin,omitempty"`
	CreatedAt      string        `json:"created_at,omitempty"`
}

type teamAdminReg struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrix_user_id"`
}

type humansRegistry struct {
	Version   int                      `json:"version"`
	UpdatedAt string                   `json:"updated_at"`
	Humans    map[string]humanRegEntry `json:"humans"`
}

type humanRegEntry struct {
	MatrixUserID    string   `json:"matrix_user_id"`
	DisplayName     string   `json:"display_name"`
	PermissionLevel int      `json:"permission_level"`
	AccessibleTeams []string `json:"accessible_teams,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
}

// --- Registry loading (local FS first, fallback to OSS) ---

func (m *Migrator) loadWorkersRegistry(ctx context.Context) (map[string]workerRegEntry, error) {
	data, err := m.readRegistryFile("workers-registry.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var reg workersRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse workers-registry.json: %w", err)
	}
	return reg.Workers, nil
}

func (m *Migrator) loadTeamsRegistry(ctx context.Context) (map[string]teamRegEntry, error) {
	data, err := m.readRegistryFile("teams-registry.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var reg teamsRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse teams-registry.json: %w", err)
	}
	return reg.Teams, nil
}

func (m *Migrator) loadHumansRegistry(ctx context.Context) (map[string]humanRegEntry, error) {
	data, err := m.readRegistryFile("humans-registry.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var reg humansRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse humans-registry.json: %w", err)
	}
	return reg.Humans, nil
}

func (m *Migrator) readRegistryFile(filename string) ([]byte, error) {
	if m.AgentFSDir != "" {
		localPath := filepath.Join(m.AgentFSDir, m.managerName(), filename)
		data, err := os.ReadFile(localPath)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	key := fmt.Sprintf("agents/%s/%s", m.managerName(), filename)
	return m.OSS.GetObject(context.Background(), key)
}

// --- Workspace data extraction ---

func (m *Migrator) extractModel(ctx context.Context, workerName string) string {
	data := m.readAgentFile(ctx, workerName, "openclaw.json")
	if data == nil {
		return m.DefaultModel
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return m.DefaultModel
	}
	models, _ := cfg["models"].(map[string]interface{})
	if models == nil {
		return m.DefaultModel
	}
	defaultModel, _ := models["default"].(string)
	if defaultModel == "" {
		return m.DefaultModel
	}
	return strings.TrimPrefix(defaultModel, "agentteams-gateway/")
}

func (m *Migrator) extractMCPServers(ctx context.Context, workerName string) []map[string]interface{} {
	data := m.readAgentFile(ctx, workerName, "mcporter-servers.json")
	if data == nil {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	servers := raw
	if wrapped, ok := raw["mcpServers"].(map[string]interface{}); ok {
		servers = wrapped
	}

	out := make([]map[string]interface{}, 0, len(servers))
	for name, v := range servers {
		entry := map[string]interface{}{
			"name": name,
		}
		if m, ok := v.(map[string]interface{}); ok {
			if urlStr, ok := m["url"].(string); ok && urlStr != "" {
				entry["url"] = urlStr
			}
			if transportStr, ok := m["transport"].(string); ok && transportStr != "" {
				entry["transport"] = transportStr
			} else {
				entry["transport"] = "http"
			}
		} else {
			entry["transport"] = "http"
		}
		out = append(out, entry)
	}
	return out
}

func (m *Migrator) readAgentFile(ctx context.Context, workerName, filename string) []byte {
	if m.AgentFSDir != "" {
		localPath := filepath.Join(m.AgentFSDir, workerName, filename)
		data, err := os.ReadFile(localPath)
		if err == nil {
			return data
		}
	}
	key := fmt.Sprintf("agents/%s/%s", workerName, filename)
	data, err := m.OSS.GetObject(ctx, key)
	if err != nil {
		return nil
	}
	return data
}

// --- CR creation ---

var (
	workerGVR = schema.GroupVersionResource{Group: v1beta1.GroupName, Version: v1beta1.Version, Resource: "workers"}
	teamGVR   = schema.GroupVersionResource{Group: v1beta1.GroupName, Version: v1beta1.Version, Resource: "teams"}
	humanGVR  = schema.GroupVersionResource{Group: v1beta1.GroupName, Version: v1beta1.Version, Resource: "humans"}
)

func (m *Migrator) createStandaloneWorkerCR(ctx context.Context, res dynamic.ResourceInterface, name string, entry workerRegEntry) error {
	if _, err := res.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	}

	model := m.extractModel(ctx, name)
	mcpServers := m.extractMCPServers(ctx, name)

	spec := map[string]interface{}{
		"model":   model,
		"runtime": entry.Runtime,
	}
	if entry.Image != nil && *entry.Image != "" {
		spec["image"] = *entry.Image
	}
	if len(entry.Skills) > 0 {
		spec["skills"] = toInterfaceSlice(entry.Skills)
	}
	if len(mcpServers) > 0 {
		spec["mcpServers"] = mcpServersToInterfaceSlice(mcpServers)
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": v1beta1.GroupName + "/" + v1beta1.Version,
			"kind":       "Worker",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": m.Namespace,
			},
			"spec": spec,
		},
	}

	_, err := res.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create Worker CR %s: %w", name, err)
	}
	return nil
}

func (m *Migrator) createTeamCR(ctx context.Context, dynClient dynamic.Interface, teamName string, entry teamRegEntry, workersReg map[string]workerRegEntry) error {
	res := dynClient.Resource(teamGVR).Namespace(m.Namespace)

	if _, err := res.Get(ctx, teamName, metav1.GetOptions{}); err == nil {
		return nil
	}

	logger := ctrl.Log.WithName("migration")

	leaderModel := m.extractModel(ctx, entry.Leader)
	leader := map[string]interface{}{
		"name":  entry.Leader,
		"model": leaderModel,
	}

	workers := make([]interface{}, 0, len(entry.Workers))
	for _, wName := range entry.Workers {
		wModel := m.extractModel(ctx, wName)
		wSpec := map[string]interface{}{
			"name":  wName,
			"model": wModel,
		}
		if wEntry, ok := workersReg[wName]; ok {
			if wEntry.Runtime != "" {
				wSpec["runtime"] = wEntry.Runtime
			}
			if len(wEntry.Skills) > 0 {
				wSpec["skills"] = toInterfaceSlice(wEntry.Skills)
			}
			mcpServers := m.extractMCPServers(ctx, wName)
			if len(mcpServers) > 0 {
				wSpec["mcpServers"] = mcpServersToInterfaceSlice(mcpServers)
			}
			if wEntry.Image != nil && *wEntry.Image != "" {
				wSpec["image"] = *wEntry.Image
			}
		} else {
			logger.Info("team worker not found in workers-registry", "team", teamName, "worker", wName)
		}
		workers = append(workers, wSpec)
	}

	spec := map[string]interface{}{
		"leader":  leader,
		"workers": workers,
	}
	if entry.Admin != nil {
		admin := map[string]interface{}{
			"name": entry.Admin.Name,
		}
		if entry.Admin.MatrixUserID != "" {
			admin["matrixUserId"] = entry.Admin.MatrixUserID
		}
		spec["admin"] = admin
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": v1beta1.GroupName + "/" + v1beta1.Version,
			"kind":       "Team",
			"metadata": map[string]interface{}{
				"name":      teamName,
				"namespace": m.Namespace,
			},
			"spec": spec,
		},
	}

	_, err := res.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create Team CR %s: %w", teamName, err)
	}
	return nil
}

func (m *Migrator) createHumanCR(ctx context.Context, dynClient dynamic.Interface, name string, entry humanRegEntry) error {
	res := dynClient.Resource(humanGVR).Namespace(m.Namespace)

	if _, err := res.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	}

	spec := map[string]interface{}{
		"displayName":     entry.DisplayName,
		"permissionLevel": int64(entry.PermissionLevel),
	}
	if len(entry.AccessibleTeams) > 0 {
		spec["accessibleTeams"] = toInterfaceSlice(entry.AccessibleTeams)
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": v1beta1.GroupName + "/" + v1beta1.Version,
			"kind":       "Human",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": m.Namespace,
			},
			"spec": spec,
		},
	}

	_, err := res.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create Human CR %s: %w", name, err)
	}
	return nil
}

func toInterfaceSlice(ss []string) []interface{} {
	result := make([]interface{}, len(ss))
	for i, s := range ss {
		result[i] = s
	}
	return result
}

func mcpServersToInterfaceSlice(entries []map[string]interface{}) []interface{} {
	result := make([]interface{}, len(entries))
	for i, e := range entries {
		result[i] = e
	}
	return result
}
