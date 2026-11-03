// Package runtimeconfig exposes each runtime's own configuration surface.
//
// The platform does not define a cross-runtime field contract: a runtime
// declares what it has, and the console renders exactly that. Nothing in this
// package translates one runtime's field into another's.
package runtimeconfig

// Applies says when a written value takes effect.
type Applies string

const (
	AppliesHot      Applies = "hot"       // applied without restarting the runtime
	AppliesNextTurn Applies = "next-turn" // read again on the next turn/session
	AppliesRestart  Applies = "restart"   // needs a process restart
)

// Class tells a policy layer how a field participates in model selection.
// Empty means the field is not part of the model-selection plane.
type Class string

const (
	// ClassSelection marks a key that selects which provider/model is active.
	ClassSelection Class = "selection"
	// ClassDefinition marks a key that defines a provider, its endpoint,
	// its credentials, or the model catalog itself.
	ClassDefinition Class = "definition"
	// ClassPolicy marks a key that constrains model selection (a policy
	// applied on top of a policy).
	ClassPolicy Class = "policy"
)

// Valid reports whether c is a known class; the empty string is valid and
// means "not classified".
func (c Class) Valid() bool {
	switch c {
	case "", ClassSelection, ClassDefinition, ClassPolicy:
		return true
	}
	return false
}

// Field describes one configurable key of a runtime.
type Field struct {
	Key       string   `json:"key"`
	Label     string   `json:"label,omitempty"`
	Type      string   `json:"type"` // string|int|float|bool|enum|string-list|object
	Default   any      `json:"default,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Applies   Applies  `json:"applies"`
	Class     Class    `json:"class,omitempty"`
	Sensitive bool     `json:"sensitive,omitempty"`
	Group     string   `json:"group,omitempty"`
	Enum      []string `json:"enum,omitempty"`
}

// Face groups fields the way the runtime's own UI/documentation does.
type Face struct {
	ID     string  `json:"id"`
	Label  string  `json:"label,omitempty"`
	Fields []Field `json:"fields"`
}

// Surface is the whole configurable surface of one runtime version.
type Surface struct {
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
	SurfaceVersion string `json:"surfaceVersion"`
	Faces          []Face `json:"faces"`
}

// Field looks up a field by key; ok is false when the key is not declared.
func (s Surface) Field(key string) (Field, bool) {
	for _, f := range s.Faces {
		for _, fld := range f.Fields {
			if fld.Key == key {
				return fld, true
			}
		}
	}
	return Field{}, false
}

// Keys returns every declared key, for coverage assertions.
func (s Surface) Keys() []string {
	out := make([]string, 0, len(s.Faces))
	for _, f := range s.Faces {
		for _, fld := range f.Fields {
			out = append(out, fld.Key)
		}
	}
	return out
}
