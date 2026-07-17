package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type createTeamOptions struct {
	name                     string
	teamName                 string
	leaderName               string
	leaderModel              string
	leaderHeartbeatEvery     string
	workerIdleTimeout        string
	workers                  string
	description              string
	teamAdmin                string
	teamAdminMatrixID        string
	peerMentions             string
	teamChannelPolicy        string
	teamChannelPolicyFile    string
	leaderChannelPolicy      string
	leaderChannelPolicyFile  string
	workerModels             string
	workerRuntimes           string
	workerSkills             string
	workerMcpServers         string
	workerChannelPolicies    string
	leaderMcpServers         string
	modelProvider            string
}

func buildCreateTeamRequest(opts createTeamOptions) (map[string]interface{}, error) {
	req := map[string]interface{}{
		"name": opts.name,
		"leader": map[string]interface{}{
			"name": opts.leaderName,
		},
	}
	leader := req["leader"].(map[string]interface{})
	if opts.leaderModel != "" {
		leader["model"] = opts.leaderModel
	}
	if opts.leaderHeartbeatEvery != "" {
		leader["heartbeat"] = map[string]interface{}{
			"enabled": true,
			"every":   opts.leaderHeartbeatEvery,
		}
	}
	setIfNotEmpty(leader, "workerIdleTimeout", opts.workerIdleTimeout)
	if opts.leaderMcpServers != "" {
		leader["mcpServers"] = parseMcpServerNames(opts.leaderMcpServers)
	}
	if policy, err := readChannelPolicy(opts.leaderChannelPolicy, opts.leaderChannelPolicyFile); err != nil {
		return nil, fmt.Errorf("leader channel policy: %w", err)
	} else if policy != nil {
		leader["channelPolicy"] = policy
	}

	setIfNotEmpty(req, "teamName", opts.teamName)
	setIfNotEmpty(req, "description", opts.description)
	setIfNotEmpty(req, "modelProvider", opts.modelProvider)

	if opts.teamAdmin != "" || opts.teamAdminMatrixID != "" {
		admin := map[string]interface{}{}
		if opts.teamAdmin != "" {
			admin["name"] = opts.teamAdmin
		}
		if opts.teamAdminMatrixID != "" {
			admin["matrixUserId"] = opts.teamAdminMatrixID
		}
		req["admin"] = admin
	}
	if opts.peerMentions != "" {
		val := strings.EqualFold(opts.peerMentions, "true") || opts.peerMentions == "1"
		req["peerMentions"] = val
	}
	if policy, err := readChannelPolicy(opts.teamChannelPolicy, opts.teamChannelPolicyFile); err != nil {
		return nil, fmt.Errorf("team channel policy: %w", err)
	} else if policy != nil {
		req["channelPolicy"] = policy
	}

	workerNames := splitCSV(opts.workers)
	if len(workerNames) > 0 {
		models := splitCSV(opts.workerModels)
		runtimes := splitCSV(opts.workerRuntimes)
		skillsParts := splitBySep(opts.workerSkills, ":")
		mcpParts := splitBySep(opts.workerMcpServers, ":")
		policyParts := splitBySep(opts.workerChannelPolicies, "|")
		workers := make([]interface{}, 0, len(workerNames))
		for i, name := range workerNames {
			w := map[string]interface{}{"name": name}
			if i < len(models) && models[i] != "" {
				w["model"] = models[i]
			}
			if i < len(runtimes) && runtimes[i] != "" {
				w["runtime"] = runtimes[i]
			}
			if i < len(skillsParts) && strings.TrimSpace(skillsParts[i]) != "" {
				w["skills"] = splitCSV(skillsParts[i])
			}
			if i < len(mcpParts) && strings.TrimSpace(mcpParts[i]) != "" {
				w["mcpServers"] = parseMcpServerNames(mcpParts[i])
			}
			if i < len(policyParts) && strings.TrimSpace(policyParts[i]) != "" {
				var workerPolicy map[string]interface{}
				if err := json.Unmarshal([]byte(policyParts[i]), &workerPolicy); err != nil {
					return nil, fmt.Errorf("worker channel policy for %s: %w", name, err)
				}
				w["channelPolicy"] = workerPolicy
			}
			workers = append(workers, w)
		}
		req["workers"] = workers
	} else {
		req["workers"] = []interface{}{}
	}
	return req, nil
}

func splitBySep(s, sep string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, sep) {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

func readChannelPolicy(inline, filePath string) (map[string]interface{}, error) {
	raw := strings.TrimSpace(inline)
	if raw == "" && strings.TrimSpace(filePath) != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(string(data))
	}
	if raw == "" {
		return nil, nil
	}
	var policy map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return nil, err
	}
	return policy, nil
}

func parseMcpServerNames(csv string) []map[string]interface{} {
	var servers []map[string]interface{}
	for _, name := range splitCSV(csv) {
		servers = append(servers, map[string]interface{}{"name": name})
	}
	return servers
}
