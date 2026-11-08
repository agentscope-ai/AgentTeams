package openclaw

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

// Adapter implements runtimeconfig.Adapter for OpenClaw.
type Adapter struct {
	surface runtimeconfig.Surface
}

// New builds the adapter from the embedded surface. A load failure is a
// build defect (the embedded files are version-pinned), so it panics.
func New() *Adapter {
	s, err := LoadSurface()
	if err != nil {
		panic(err)
	}
	return &Adapter{surface: s}
}

// Surface implements runtimeconfig.Adapter.
func (a *Adapter) Surface() runtimeconfig.Surface { return a.surface }

// FieldForKey resolves a concrete config key against the declared fields.
// A declared key may contain `*` segments; each `*` matches exactly one
// non-empty segment (e.g. `models.providers.*.baseUrl` matches
// `models.providers.openai.baseUrl`, but not `models.providers.baseUrl`).
func (a *Adapter) FieldForKey(key string) (runtimeconfig.Field, bool) {
	for _, face := range a.surface.Faces {
		for _, f := range face.Fields {
			if keyMatchesPattern(f.Key, key) {
				return f, true
			}
		}
	}
	return runtimeconfig.Field{}, false
}

func keyMatchesPattern(pattern, key string) bool {
	p := strings.Split(pattern, ".")
	k := strings.Split(key, ".")
	if len(p) != len(k) {
		return false
	}
	for i, seg := range p {
		if seg == "*" {
			if k[i] == "" {
				return false
			}
			continue
		}
		if seg != k[i] {
			return false
		}
	}
	return true
}

// KeyMode classifies how a concrete config key matched the declared
// surface.
type KeyMode int

const (
	// KeyExact: the key itself matched a declared field (pattern `*`
	// segments allowed). The value is type/enum/secret validated.
	KeyExact KeyMode = iota
	// KeyOpaque: a dot-ancestor of the key matched a declared
	// object/record field. The surface does not decompose that container,
	// so the value passes through to the file as written; OpenClaw's own
	// zod strict validation is the final arbiter (an illegal document is
	// rejected by the gateway, which keeps its last-good config).
	KeyOpaque
)

// ResolveKey classifies a concrete key against the declared surface: an
// exact field match wins, then the longest dot-ancestor declared as an
// object/record field. ok is false when neither applies (422
// unknown_field on write).
func (a *Adapter) ResolveKey(key string) (runtimeconfig.Field, KeyMode, bool) {
	if f, ok := a.FieldForKey(key); ok {
		return f, KeyExact, true
	}
	for i := strings.LastIndex(key, "."); i >= 0; i = strings.LastIndex(key[:i], ".") {
		ancestor := key[:i]
		if f, ok := a.FieldForKey(ancestor); ok && (f.Type == "object" || f.Type == "record") {
			return f, KeyOpaque, true
		}
	}
	return runtimeconfig.Field{}, 0, false
}

// SecretRef is the only shape a sensitive value may take in a write
// (OpenClaw SecretRefSchema: strict {source, provider, id}). The runtime
// resolves the reference; the controller never sees or stores the value.
type SecretRef struct {
	Source   string `json:"source"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

var (
	// Mirrors SECRET_PROVIDER_ALIAS_PATTERN in OpenClaw ref-contract.ts.
	secretProviderPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	// Mirrors ENV_SECRET_REF_ID_RE (env and store sources).
	secretEnvIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

// ValidateSecretRef checks v against the OpenClaw SecretRefSchema and
// returns the parsed reference. Every failure is a 422-class error: a
// sensitive key never accepts a plaintext value (credential invariants §8.2).
func ValidateSecretRef(v any) (*SecretRef, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("credential must be a secret ref object {source, provider, id}, never a plaintext value")
	}
	for k := range obj {
		if k != "source" && k != "provider" && k != "id" {
			return nil, fmt.Errorf("secret ref has unknown field %q (OpenClaw accepts exactly source/provider/id)", k)
		}
	}
	ref := &SecretRef{}
	var err error
	if ref.Source, err = stringField(obj, "source"); err != nil {
		return nil, err
	}
	if ref.Provider, err = stringField(obj, "provider"); err != nil {
		return nil, err
	}
	if ref.ID, err = stringField(obj, "id"); err != nil {
		return nil, err
	}
	if !secretProviderPattern.MatchString(ref.Provider) {
		return nil, fmt.Errorf("secret ref provider %q must match %s", ref.Provider, secretProviderPattern)
	}
	switch ref.Source {
	case "env", "store":
		if !secretEnvIDPattern.MatchString(ref.ID) {
			return nil, fmt.Errorf("secret ref id %q for source %q must match %s", ref.ID, ref.Source, secretEnvIDPattern)
		}
	case "file":
		if ref.ID != "value" && !strings.HasPrefix(ref.ID, "/") {
			return nil, fmt.Errorf("secret ref id %q for source file must be an absolute JSON pointer or \"value\"", ref.ID)
		}
	case "exec":
		if ref.ID == "" {
			return nil, errors.New("secret ref id for source exec must be non-empty")
		}
	default:
		return nil, fmt.Errorf("secret ref source %q must be one of env|file|exec|store", ref.Source)
	}
	return ref, nil
}

func stringField(obj map[string]any, k string) (string, error) {
	v, ok := obj[k].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("secret ref field %q must be a non-empty string", k)
	}
	return v, nil
}

// IsSecret reports whether a declared field carries credentials.
func IsSecret(f runtimeconfig.Field) bool {
	return f.Type == "secret" || f.Sensitive
}

// ValidateValue checks one concrete key/value pair against the declared
// field: the key must be declared, the value must match the field type,
// and enum fields must hold an allowed value.
func (a *Adapter) ValidateValue(key string, value any) error {
	f, ok := a.FieldForKey(key)
	if !ok {
		return fmt.Errorf("key %q is not declared in the openclaw surface", key)
	}
	if err := checkType(key, f, value); err != nil {
		return err
	}
	if len(f.Enum) > 0 {
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("key %q must be one of %v", key, f.Enum)
		}
		for _, e := range f.Enum {
			if s == e {
				return nil
			}
		}
		return fmt.Errorf("key %q value %q must be one of %v", key, s, f.Enum)
	}
	return nil
}

func checkType(key string, f runtimeconfig.Field, value any) error {
	switch f.Type {
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("key %q must be a boolean", key)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("key %q must be a number", key)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("key %q must be a string", key)
		}
	case "secret":
		if _, err := ValidateSecretRef(value); err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("key %q must be an array", key)
		}
	case "record", "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("key %q must be an object", key)
		}
	default:
		return fmt.Errorf("key %q has undeclared type %q", key, f.Type)
	}
	return nil
}
