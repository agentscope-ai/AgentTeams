package sandbox

import "fmt"

// PluginRegistry manages sandbox plugin registrations keyed by type name.
type PluginRegistry struct {
	plugins map[string]SandboxPlugin
}

// NewPluginRegistry creates an empty plugin registry.
func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{plugins: make(map[string]SandboxPlugin)}
}

// Register adds a plugin under the given type name. Panics on duplicate registration.
func (r *PluginRegistry) Register(typeName string, p SandboxPlugin) {
	if _, exists := r.plugins[typeName]; exists {
		panic(fmt.Sprintf("sandbox plugin %q already registered", typeName))
	}
	r.plugins[typeName] = p
}

// Get returns the plugin for the given type, or an error if not found.
func (r *PluginRegistry) Get(typeName string) (SandboxPlugin, error) {
	p, ok := r.plugins[typeName]
	if !ok {
		return nil, fmt.Errorf("sandbox plugin %q not registered", typeName)
	}
	return p, nil
}
