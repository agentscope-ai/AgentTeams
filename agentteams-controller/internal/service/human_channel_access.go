package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
)

type humanChannelOwnership struct {
	Group []string `json:"group"`
	DM    []string `json:"dm"`
}

// Journal ownership before publishing the config and compact it afterwards.
// If either write fails, the next reconcile can still remove only the entries
// this controller added. Pre-existing manual entries are never claimed.
func (d *Deployer) reconcileHumanChannelAccess(ctx context.Context, req WorkerDeployRequest, generated, merged, existing []byte) ([]byte, func() error, error) {
	key := "agents/" + req.Name + "/.agentteams-human-channel-access.json"
	var previous humanChannelOwnership
	data, err := d.oss.GetObject(ctx, key)
	if err == nil {
		if err := json.Unmarshal(data, &previous); err != nil {
			return nil, nil, fmt.Errorf("decode Human allowlist ownership: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read Human allowlist ownership: %w", err)
	}
	var fresh, config, old map[string]any
	if err := json.Unmarshal(generated, &fresh); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(merged, &config); err != nil {
		return nil, nil, err
	}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &old); err != nil {
			return nil, nil, err
		}
	}
	matrix := matrixChannel(config)
	freshMatrix := matrixChannel(fresh)
	oldMatrix := matrixChannel(old)
	domain := ""
	if id, _ := freshMatrix["userId"].(string); strings.Contains(id, ":") {
		domain = strings.SplitN(id, ":", 2)[1]
	}
	var groupDeny, dmDeny []string
	if req.Spec.ChannelPolicy != nil {
		groupDeny = req.Spec.ChannelPolicy.GroupDenyExtra
		dmDeny = req.Spec.ChannelPolicy.DmDenyExtra
	}
	group, groupOwned := reconcileHumanIDs(channelIDs(oldMatrix, false), channelIDs(freshMatrix, false), previous.Group, req.HumanMatrixIDs, groupDeny, domain)
	dm, dmOwned := reconcileHumanIDs(channelIDs(oldMatrix, true), channelIDs(freshMatrix, true), previous.DM, req.HumanMatrixIDs, dmDeny, domain)
	matrix["groupAllowFrom"] = group
	matrix["dm"].(map[string]any)["allowFrom"] = dm
	next := humanChannelOwnership{Group: groupOwned, DM: dmOwned}
	pending := humanChannelOwnership{Group: sortedHumanIDs(append(append([]string{}, previous.Group...), groupOwned...)), DM: sortedHumanIDs(append(append([]string{}, previous.DM...), dmOwned...))}
	write := func(value humanChannelOwnership) error {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return d.oss.PutObject(ctx, key, payload)
	}
	if !reflect.DeepEqual(pending, previous) {
		if err := write(pending); err != nil {
			return nil, nil, fmt.Errorf("prepare Human allowlist ownership: %w", err)
		}
	}
	finish := func() error {
		if reflect.DeepEqual(next, pending) {
			return nil
		}
		return write(next)
	}
	result, err := json.MarshalIndent(config, "", "  ")
	return result, finish, err
}

func matrixChannel(config map[string]any) map[string]any {
	channels, _ := config["channels"].(map[string]any)
	matrix, _ := channels["matrix"].(map[string]any)
	return matrix
}

func channelIDs(matrix map[string]any, dm bool) []string {
	var values []any
	if dm {
		policy, _ := matrix["dm"].(map[string]any)
		values, _ = policy["allowFrom"].([]any)
	} else {
		values, _ = matrix["groupAllowFrom"].([]any)
	}
	var ids []string
	for _, value := range values {
		if id, ok := value.(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func reconcileHumanIDs(existing, generated, owned, desired, deny []string, domain string) ([]string, []string) {
	set := func(ids []string) map[string]bool {
		result := map[string]bool{}
		for _, id := range ids {
			result[id] = true
		}
		return result
	}
	wasOwned, want, fresh := set(owned), set(desired), set(generated)
	denied := map[string]bool{}
	for _, id := range deny {
		if !strings.HasPrefix(id, "@") {
			id = "@" + id + ":" + domain
		}
		denied[id] = true
	}
	result := make([]string, 0)
	seen := map[string]bool{}
	for _, id := range append(append([]string{}, existing...), generated...) {
		if seen[id] || denied[id] || (wasOwned[id] && !want[id] && !fresh[id]) {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	preexisting := set(existing)
	nextOwned := make([]string, 0)
	for _, id := range desired {
		if seen[id] && (wasOwned[id] || !preexisting[id]) {
			nextOwned = append(nextOwned, id)
		}
	}
	return result, sortedHumanIDs(nextOwned)
}

func sortedHumanIDs(ids []string) []string {
	sort.Strings(ids)
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if len(result) == 0 || result[len(result)-1] != id {
			result = append(result, id)
		}
	}
	return result
}
