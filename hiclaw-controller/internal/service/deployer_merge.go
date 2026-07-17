package service

import "encoding/json"

// mergeUserPluginConfig preserves user-customized plugin entries from an
// existing openclaw.json when regenerating config on update. The generated
// config provides defaults for any new entries; existing user-modified
// entries override generated values so that customizations (e.g. memory-core
// dreaming schedule) survive controller reconciles.
//
// It also preserves channels.matrix.groupAllowFrom and channels.matrix.dm.allowFrom
// from the existing config, because TeamReconciler in the decoupled path
// overrides these to [leader, admin] for team members. WorkerReconciler is
// team-agnostic and would otherwise revert them to standalone [manager, admin]
// on every reconcile, breaking team-scoped task delivery.
func mergeUserPluginConfig(generatedJSON, existingJSON []byte) ([]byte, error) {
	var generated, existing map[string]interface{}
	if err := json.Unmarshal(generatedJSON, &generated); err != nil {
		return generatedJSON, err
	}
	if err := json.Unmarshal(existingJSON, &existing); err != nil {
		return generatedJSON, err
	}

	preserveChannelMatrixAllowFrom(generated, existing)

	genPlugins, _ := generated["plugins"].(map[string]interface{})
	existPlugins, _ := existing["plugins"].(map[string]interface{})
	if genPlugins == nil || existPlugins == nil {
		return json.MarshalIndent(generated, "", "  ")
	}

	genEntries, _ := genPlugins["entries"].(map[string]interface{})
	existEntries, _ := existPlugins["entries"].(map[string]interface{})
	if existEntries != nil && genEntries != nil {
		merged := make(map[string]interface{})
		for k, v := range genEntries {
			merged[k] = v
		}
		for k, v := range existEntries {
			if genV, has := merged[k]; has {
				merged[k] = deepMergeMap(toMap(genV), toMap(v))
			} else {
				merged[k] = v
			}
		}
		genPlugins["entries"] = merged
	}

	genLoad, _ := genPlugins["load"].(map[string]interface{})
	existLoad, _ := existPlugins["load"].(map[string]interface{})
	if genLoad != nil && existLoad != nil {
		genPaths := toStringSliceCompat(genLoad["paths"])
		existPaths := toStringSliceCompat(existLoad["paths"])
		seen := make(map[string]bool, len(genPaths)+len(existPaths))
		var unionPaths []string
		for _, p := range genPaths {
			if !seen[p] {
				seen[p] = true
				unionPaths = append(unionPaths, p)
			}
		}
		for _, p := range existPaths {
			if !seen[p] {
				seen[p] = true
				unionPaths = append(unionPaths, p)
			}
		}
		genLoad["paths"] = unionPaths
	}

	return json.MarshalIndent(generated, "", "  ")
}

func toMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// preserveChannelMatrixAllowFrom copies channels.matrix.groupAllowFrom and
// channels.matrix.dm.allowFrom from existing into generated when the existing
// values are non-empty.
func preserveChannelMatrixAllowFrom(generated, existing map[string]interface{}) {
	existChannels, _ := existing["channels"].(map[string]interface{})
	if existChannels == nil {
		return
	}
	existMatrix, _ := existChannels["matrix"].(map[string]interface{})
	if existMatrix == nil {
		return
	}

	genChannels, _ := generated["channels"].(map[string]interface{})
	if genChannels == nil {
		genChannels = make(map[string]interface{})
		generated["channels"] = genChannels
	}
	genMatrix, _ := genChannels["matrix"].(map[string]interface{})
	if genMatrix == nil {
		genMatrix = make(map[string]interface{})
		genChannels["matrix"] = genMatrix
	}

	if existAllow, ok := existMatrix["groupAllowFrom"].([]interface{}); ok && len(existAllow) > 0 {
		genMatrix["groupAllowFrom"] = existAllow
	}
	if existDM, ok := existMatrix["dm"].(map[string]interface{}); ok {
		genDM, _ := genMatrix["dm"].(map[string]interface{})
		if genDM == nil {
			genDM = make(map[string]interface{})
			genMatrix["dm"] = genDM
		}
		if existDMAllow, ok := existDM["allowFrom"].([]interface{}); ok && len(existDMAllow) > 0 {
			genDM["allowFrom"] = existDMAllow
		}
	}
}

// deepMergeMap recursively merges override into base; override wins on
// leaf-level conflicts. Both inputs must be non-nil (caller guards).
func deepMergeMap(base, override map[string]interface{}) map[string]interface{} {
	if base == nil {
		return override
	}
	if override == nil {
		return base
	}
	result := make(map[string]interface{}, len(base)+len(override))
	for k, v := range base {
		result[k] = v
	}
	for k, ov := range override {
		bv, exists := result[k]
		if !exists {
			result[k] = ov
			continue
		}
		bMap, bIsMap := bv.(map[string]interface{})
		oMap, oIsMap := ov.(map[string]interface{})
		if bIsMap && oIsMap {
			result[k] = deepMergeMap(bMap, oMap)
		} else {
			result[k] = ov
		}
	}
	return result
}

func toStringSliceCompat(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch arr := v.(type) {
	case []interface{}:
		var result []string
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
