package runtimeconfig

import "testing"

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
