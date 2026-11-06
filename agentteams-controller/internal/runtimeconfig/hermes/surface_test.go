package hermes

import (
	"sort"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

func mustSurface(t *testing.T) runtimeconfig.Surface {
	t.Helper()
	s, err := Surface()
	if err != nil {
		t.Fatalf("Surface(): %v", err)
	}
	return s
}

func faceByKey(s runtimeconfig.Surface, id string) (runtimeconfig.Face, bool) {
	for _, f := range s.Faces {
		if f.ID == id {
			return f, true
		}
	}
	return runtimeconfig.Face{}, false
}

func TestSurfaceParses(t *testing.T) {
	s := mustSurface(t)
	if s.Runtime != "hermes" {
		t.Fatalf("runtime = %q", s.Runtime)
	}
	if s.SurfaceVersion == "" {
		t.Fatal("empty surfaceVersion")
	}
	if len(s.Faces) < 100 {
		t.Fatalf("want >=100 faces, got %d", len(s.Faces))
	}
	if len(s.Keys()) == 0 {
		t.Fatal("no keys")
	}
}

func TestRequiredFacesPresent(t *testing.T) {
	s := mustSurface(t)
	for _, id := range []string{
		"model", "providers", "fallback_providers", "fallback",
		"mcp", "mcp_servers", "matrix", "platforms.matrix",
		"delegation", "compression", "streaming", "sessions", "agent",
	} {
		if _, ok := faceByKey(s, id); !ok {
			t.Errorf("required face %q missing", id)
		}
	}
}

// mcp (runtime behavior) and mcp_servers (server definitions) are two
// separate faces; a concrete key must resolve to exactly one of them.
func TestMcpAndMcpServersAreSeparateFaces(t *testing.T) {
	s := mustSurface(t)
	mcp, ok := faceByKey(s, "mcp")
	if !ok {
		t.Fatal("mcp face missing")
	}
	servers, ok := faceByKey(s, "mcp_servers")
	if !ok {
		t.Fatal("mcp_servers face missing")
	}
	for _, fld := range mcp.Fields {
		f, ok := FieldFor(s, fld.Key)
		if !ok || f.Key != fld.Key {
			t.Fatalf("mcp face key %q resolves to %q", fld.Key, f.Key)
		}
	}
	for _, fld := range servers.Fields {
		entryKey := "mcp_servers.github." + fld.Key[len("mcp_servers.*."):]
		f, ok := FieldFor(s, entryKey)
		if !ok || f.Key != fld.Key {
			t.Fatalf("mcp_servers entry key %q resolves to %q", entryKey, f.Key)
		}
	}
}

// The providers face must declare exactly the entry keys Hermes actually
// reads (config_providers._KNOWN_PROVIDER_KEYS + is_provider_enabled's
// "enabled"); nothing guessed.
func TestProviderEntryKeySet(t *testing.T) {
	s := mustSurface(t)
	face, _ := faceByKey(s, "providers")
	want := map[string]bool{
		"enabled": true, "name": true, "provider": true, "api": true,
		"url": true, "base_url": true, "api_key": true, "key_env": true,
		"api_key_env": true, "key_cmd": true, "api_mode": true,
		"transport": true, "model": true, "default_model": true,
		"models": true, "models_discovered": true, "context_length": true,
		"rate_limit_delay": true, "request_timeout_seconds": true,
		"stale_timeout_seconds": true, "discover_models": true,
		"extra_body": true, "extra_headers": true, "capabilities": true,
		"ssl_ca_cert": true, "ssl_verify": true, "catalog_provider": true,
		"session_affinity_header": true,
	}
	if len(face.Fields) != len(want) {
		t.Fatalf("providers face: %d fields, want %d", len(face.Fields), len(want))
	}
	for _, fld := range face.Fields {
		suffix := fld.Key[len("providers.*."):]
		if !want[suffix] {
			t.Errorf("providers face declares undeclared entry key %q", suffix)
		}
		delete(want, suffix)
	}
	if len(want) != 0 {
		t.Errorf("providers face missing entry keys: %v", want)
	}
}

// Exactly seven restart-class keys: the matrix connection six plus
// streaming.enabled ("restart after enabling" in the source).
func TestRestartKeySetIsExact(t *testing.T) {
	s := mustSurface(t)
	want := map[string]bool{
		"platforms.matrix.enabled":    true,
		"platforms.matrix.token":      true,
		"platforms.matrix.api_key":    true,
		"platforms.matrix.homeserver": true,
		"platforms.matrix.user_id":    true,
		"platforms.matrix.password":   true,
		"streaming.enabled":           true,
	}
	var got []string
	for _, k := range s.Keys() {
		fld, _ := s.Field(k)
		if fld.Applies == runtimeconfig.AppliesRestart {
			if _, ok := want[k]; ok {
				delete(want, k)
			} else {
				got = append(got, k)
			}
		}
	}
	sort.Strings(got)
	if len(want) != 0 {
		t.Errorf("restart keys missing from surface: %v", want)
	}
	if len(got) != 0 {
		t.Errorf("unexpected restart keys: %v", got)
	}
}

// Hermes has no config watcher: everything is re-read next turn except the
// restart class. "hot" must not appear anywhere.
func TestNoHotApplies(t *testing.T) {
	s := mustSurface(t)
	for _, k := range s.Keys() {
		fld, _ := s.Field(k)
		if fld.Applies == runtimeconfig.AppliesHot {
			t.Errorf("key %q is hot; Hermes has no watcher", k)
		}
	}
}

func TestDelegationModelDefaults(t *testing.T) {
	s := mustSurface(t)
	for _, key := range []string{"delegation.model", "delegation.provider"} {
		fld, ok := s.Field(key)
		if !ok {
			t.Fatalf("field %q missing", key)
		}
		if fld.Default != "" {
			t.Errorf("%s default = %v, want empty (inherit parent)", key, fld.Default)
		}
	}
	fld, ok := s.Field("delegation.fallback_providers")
	if !ok {
		t.Fatal("delegation.fallback_providers missing")
	}
	if fld.Default != nil {
		t.Errorf("delegation.fallback_providers default = %v, want nil (inherit parent chain)", fld.Default)
	}
}

func TestSensitiveSpotChecks(t *testing.T) {
	s := mustSurface(t)
	sensitive := map[string]bool{
		"providers.*.api_key":           true,
		"providers.*.key_cmd":           true,
		"providers.*.extra_headers":     true,
		"delegation.api_key":            true,
		"auxiliary.vision.api_key":      true,
		"dashboard.basic_auth.password": true,
		"platforms.matrix.token":        true,
		"platforms.matrix.password":     true,
		"mcp_servers.*.env":             true,
		"terminal.docker_env":           true,
		"secrets.onepassword.env":       true,
	}
	for key, want := range sensitive {
		fld, ok := FieldFor(s, key)
		if !ok {
			t.Fatalf("sensitive key %q not resolvable", key)
		}
		if fld.Sensitive != want {
			t.Errorf("%s sensitive = %v, want %v", key, fld.Sensitive, want)
		}
	}
	for _, key := range []string{"model", "providers.*.name", "streaming.enabled", "delegation.model"} {
		fld, _ := FieldFor(s, key)
		if fld.Sensitive {
			t.Errorf("%s should not be sensitive", key)
		}
	}
}

func TestMatchKey(t *testing.T) {
	cases := []struct {
		pattern, key string
		want         bool
	}{
		{"providers.*.api_key", "providers.openrouter.api_key", true},
		{"providers.*.api_key", "providers.openrouter.base_url", false},
		{"providers.*.api_key", "providers.api_key", false},
		{"providers.*.api_key", "providers.openrouter.sub.api_key", false},
		{"mcp_servers.*.command", "mcp_servers.github.command", true},
		{"mcp_servers.*.command", "mcp.command", false},
		{"a.b", "a.b", true},
		{"a.b", "a.c", false},
		{"a.*.b", "a.x.b", true},
		{"a.*.b", "a..b", false},      // wildcard needs a non-empty segment
		{"a.*.b.*", "a.x.b.y", false}, // at most one wildcard
		{"*", "a", true},
		{"*", "a.b", false},
	}
	for _, c := range cases {
		if got := MatchKey(c.pattern, c.key); got != c.want {
			t.Errorf("MatchKey(%q, %q) = %v, want %v", c.pattern, c.key, got, c.want)
		}
	}
}

func TestFieldForWildcardAndExact(t *testing.T) {
	s := mustSurface(t)
	if _, ok := s.Field("providers.openrouter.api_key"); ok {
		t.Fatal("frozen exact-match lookup must not see wildcard keys")
	}
	fld, ok := FieldFor(s, "providers.openrouter.api_key")
	if !ok || fld.Key != "providers.*.api_key" {
		t.Fatalf("FieldFor wildcard: %+v %v", fld, ok)
	}
	if !fld.Sensitive {
		t.Error("providers.*.api_key should be sensitive")
	}
	fld, ok = FieldFor(s, "delegation.model")
	if !ok || fld.Key != "delegation.model" {
		t.Fatalf("FieldFor exact: %+v %v", fld, ok)
	}
	if _, ok := FieldFor(s, "totally.unknown.key"); ok {
		t.Fatal("FieldFor must reject undeclared keys")
	}
	// mcp.* must not swallow mcp_servers.*: no mcp face wildcard exists,
	// and mcp_servers.*.command must not match through any other pattern.
	if _, ok := FieldFor(s, "mcp_servers.github.command"); !ok {
		t.Fatal("mcp_servers entry key unresolved")
	}
}

func TestEnumFieldsCarryEnum(t *testing.T) {
	s := mustSurface(t)
	for _, key := range []string{"approvals.mode", "delegation.api_mode", "streaming.transport"} {
		fld, ok := s.Field(key)
		if !ok {
			t.Fatalf("field %q missing", key)
		}
		if fld.Type != "enum" || len(fld.Enum) == 0 {
			t.Errorf("%s: type=%q enum=%v, want enum with values", key, fld.Type, fld.Enum)
		}
	}
}

// The model face is an object container in hermes config.yaml, and its
// subkeys are classified: default/provider are the selection plane,
// base_url is provider definition.
func TestModelIsObjectAndClassed(t *testing.T) {
	s := mustSurface(t)
	fld, ok := s.Field("model")
	if !ok {
		t.Fatal("model field missing")
	}
	if fld.Type != "object" {
		t.Errorf("model type = %q, want object", fld.Type)
	}
	for key, want := range map[string]runtimeconfig.Class{
		"model.default":  runtimeconfig.ClassSelection,
		"model.provider": runtimeconfig.ClassSelection,
		"model.base_url": runtimeconfig.ClassDefinition,
	} {
		fld, ok := s.Field(key)
		if !ok {
			t.Fatalf("field %q missing", key)
		}
		if fld.Class != want {
			t.Errorf("%s class = %q, want %q", key, fld.Class, want)
		}
	}
}
