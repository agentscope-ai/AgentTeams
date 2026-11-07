package server

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CheckNativeWrite checks the actual merged/restored document, before snapshots
// or file writes. Unchanged native values do not need a fresh model grant.
func (h *MemberConfigHandler) CheckNativeWrite(ctx context.Context, caller *authpkg.CallerIdentity, name, runtime string, next map[string]any, read func(context.Context) (map[string]any, error)) error {
	if caller == nil || caller.Role != authpkg.RoleHuman {
		return nil
	}
	current, err := read(ctx)
	if err != nil {
		return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "current native configuration cannot be parsed"}
	}
	adapter, ok := h.reg.Lookup(runtime)
	if !ok {
		return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "unknown native runtime"}
	}
	for _, face := range adapter.Surface().Faces {
		for _, field := range face.Fields {
			if field.Class != runtimeconfig.ClassDefinition && field.Class != runtimeconfig.ClassPolicy {
				continue
			}
			if !reflect.DeepEqual(nativeGrantPath(current, field.Key), nativeGrantPath(next, field.Key)) {
				return &NativeConfigPolicyError{Code: "native_config_" + string(field.Class) + "_l1_only", Message: field.Key}
			}
		}
	}
	// These native containers can change credentials or introduce model routes
	// outside the main/spawn grant surfaces. Keep those overrides L1-owned.
	protected := []string{"env", "secrets", "auth"}
	if runtime == "openclaw" {
		protected = append(protected, "agents.entries", "agents.defaults.utilityModel", "agents.defaults.decisionModel", "agents.defaults.imageModel", "agents.defaults.mediaModels", "agents.defaults.voiceModel", "agents.defaults.pdfModel", "tools.media.models", "tools.links.models", "channels.modelByChannel", "tts.summaryModel")
	} else {
		protected = append(protected, "auxiliary", "moa", "cron.model", "cron.provider", "cron.model_provider", "model_catalog", "models_dev")
	}
	for _, key := range protected {
		if !reflect.DeepEqual(nativeGrantPath(current, key), nativeGrantPath(next, key)) {
			return &NativeConfigPolicyError{Code: "native_config_definition_l1_only", Message: key}
		}
	}
	if runtime == "openclaw" && reflect.DeepEqual(nativeGrantPath(current, "agents.defaults.model"), nativeGrantPath(next, "agents.defaults.model")) && reflect.DeepEqual(nativeGrantPath(current, "agents.defaults.subagents.model"), nativeGrantPath(next, "agents.defaults.subagents.model")) {
		return nil
	}
	if runtime == "hermes" && reflect.DeepEqual(nativeHermesPair(current, false), nativeHermesPair(next, false)) && reflect.DeepEqual(nativeHermesPair(current, true), nativeHermesPair(next, true)) {
		return nil
	}
	var worker v1beta1.Worker
	if err := h.client.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		return &NativeConfigPolicyError{Code: "native_config_grants_unavailable", Message: "cannot read worker grants", Status: http.StatusInternalServerError}
	}
	team, _, _, err := findTeamMember(ctx, h.client, h.namespace, name)
	if err != nil {
		return &NativeConfigPolicyError{Code: "native_config_grants_unavailable", Message: "cannot read worker team", Status: http.StatusInternalServerError}
	}
	teamName, role := "", "worker"
	if team != nil {
		teamName, role = team.Name, teamMemberRole(team, name)
	}
	set, err := resolveCallerGrant(ctx, h.client, h.namespace, caller, &worker, teamName, role)
	if err != nil {
		if errors.Is(err, errModelGrantOverflow) {
			return &NativeConfigPolicyError{Code: "model_grant_overflow", Message: err.Error(), Status: http.StatusConflict}
		}
		return &NativeConfigPolicyError{Code: "native_config_grants_unavailable", Message: "cannot resolve caller grants", Status: http.StatusInternalServerError}
	}
	if runtime == "openclaw" {
		for _, slot := range []struct {
			key     string
			grants  []v1beta1.ModelAccessEntry
			inherit bool
		}{
			{"agents.defaults.model", set.main, false},
			{"agents.defaults.subagents.model", set.spawn, set.spawnInherit},
		} {
			if err := checkNativeModelSelection(nativeGrantPath(current, slot.key), nativeGrantPath(next, slot.key), slot.grants, slot.inherit); err != nil {
				return err
			}
		}
		return nil
	}
	for _, slot := range []struct {
		spawn   bool
		grants  []v1beta1.ModelAccessEntry
		inherit bool
	}{
		{false, set.main, false}, {true, set.spawn, set.spawnInherit},
	} {
		before, after := nativeHermesPair(current, slot.spawn), nativeHermesPair(next, slot.spawn)
		// Hermes "custom" is a runtime route alias, not a gateway provider
		// identity. Without a verified binding it must not match a grant by name.
		if value, ok := after.(string); ok && !reflect.DeepEqual(before, after) && strings.HasPrefix(value, "custom/") {
			return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "native custom route needs a verified provider binding"}
		}
		if err := checkNativeModelSelection(before, after, slot.grants, slot.inherit); err != nil {
			return err
		}
	}
	return nil
}

func nativeHermesPair(doc map[string]any, spawn bool) any {
	key, modelKey := "model", "default"
	if spawn {
		key, modelKey = "delegation", "model"
	}
	value := nativeGrantPath(doc, key)
	if value == nil {
		return nil
	}
	if _, ok := value.(map[string]any); !ok {
		return value
	}
	model, _ := nativeGrantPath(value, modelKey).(string)
	provider, _ := nativeGrantPath(value, "provider").(string)
	if model == "" && provider == "" {
		return nil
	}
	if spawn {
		if model == "" {
			model, _ = nativeGrantPath(doc, "model.default").(string)
		}
		if provider == "" {
			provider, _ = nativeGrantPath(doc, "model.provider").(string)
		}
	}
	return provider + "/" + model
}

// nativeGrantPath preserves wildcard member names, so renaming a provider is
// a definition change even when its endpoint and model entries are identical.
func nativeGrantPath(doc any, key string) any {
	if key == "" {
		return doc
	}
	parts := strings.SplitN(key, ".", 2)
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	obj, _ := doc.(map[string]any)
	if parts[0] != "*" {
		return nativeGrantPath(obj[parts[0]], rest)
	}
	values := map[string]any{}
	for name, value := range obj {
		if found := nativeGrantPath(value, rest); found != nil {
			values[name] = found
		}
	}
	if array, ok := doc.([]any); ok {
		for index, value := range array {
			if found := nativeGrantPath(value, rest); found != nil {
				values[strconv.Itoa(index)] = found
			}
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func checkNativeModelSelection(current, next any, grants []v1beta1.ModelAccessEntry, inherit bool) error {
	if reflect.DeepEqual(current, next) {
		return nil
	}
	var selections []string
	hasPrimary := false
	switch value := next.(type) {
	case nil:
	case string:
		if value != "" {
			hasPrimary = true
			selections = append(selections, value)
		}
	case map[string]any:
		for key, value := range value {
			switch key {
			case "primary":
				text, ok := value.(string)
				if !ok {
					return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "model primary must be a provider/model string"}
				}
				if text != "" {
					hasPrimary = true
					selections = append(selections, text)
				}
			case "fallbacks":
				list, ok := value.([]any)
				if !ok {
					return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "model fallbacks must be a list"}
				}
				for _, item := range list {
					text, ok := item.(string)
					if !ok {
						return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "model fallback must be a provider/model string"}
					}
					selections = append(selections, text)
				}
			default:
				return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "unknown native model selection"}
			}
		}
	default:
		return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "unknown native model selection"}
	}
	if !hasPrimary && !inherit {
		return &NativeConfigPolicyError{Code: "inherit_not_granted", Message: "native model selection"}
	}
	for _, selection := range selections {
		if nativeSelectionContains(current, selection) {
			continue
		}
		provider, model, ok := strings.Cut(selection, "/")
		if !ok || provider == "" || provider == "auto" || model == "" {
			return &NativeConfigPolicyError{Code: "model_pair_unresolvable", Message: "native selection requires an explicit provider/model pair"}
		}
		if !grantAllows(grants, provider, model) {
			return &NativeConfigPolicyError{Code: "model_not_allowed", Message: provider + "/" + model}
		}
	}
	return nil
}

func nativeSelectionContains(value any, selection string) bool {
	if value == selection {
		return true
	}
	if object, ok := value.(map[string]any); ok {
		if object["primary"] == selection {
			return true
		}
		if list, ok := object["fallbacks"].([]any); ok {
			for _, fallback := range list {
				if fallback == selection {
					return true
				}
			}
		}
	}
	return false
}
