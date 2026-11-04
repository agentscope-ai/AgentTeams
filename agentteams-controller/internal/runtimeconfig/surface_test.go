package runtimeconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

type stubAdapter struct{ s Surface }

func (a stubAdapter) Surface() Surface { return a.s }

func TestRegistryRejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	r.Register(stubAdapter{s: Surface{Runtime: "x"}})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic")
		}
	}()
	r.Register(stubAdapter{s: Surface{Runtime: "x"}})
}

func TestSurfaceFieldAndKeys(t *testing.T) {
	s := Surface{Faces: []Face{{ID: "f", Fields: []Field{{Key: "a.b"}, {Key: "a.c"}}}}}
	if _, ok := s.Field("a.b"); !ok {
		t.Fatal("expected a.b")
	}
	if _, ok := s.Field("nope"); ok {
		t.Fatal("unexpected key")
	}
	if got := len(s.Keys()); got != 2 {
		t.Fatalf("keys = %d", got)
	}
}

func TestClassVocabulary(t *testing.T) {
	for _, c := range []Class{ClassSelection, ClassDefinition, ClassPolicy} {
		if !c.Valid() {
			t.Fatalf("%q should be valid", c)
		}
	}
	if Class("bogus").Valid() {
		t.Fatal("bogus class must be invalid")
	}
}

func TestFieldClassJSON(t *testing.T) {
	var f Field
	if err := json.Unmarshal([]byte(`{"key":"a","type":"string","applies":"hot","class":"selection"}`), &f); err != nil {
		t.Fatal(err)
	}
	if f.Class != ClassSelection {
		t.Fatalf("class = %q", f.Class)
	}
	b, err := json.Marshal(Field{Key: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"class"`) {
		t.Fatalf("unset class must be omitted: %s", b)
	}
}
