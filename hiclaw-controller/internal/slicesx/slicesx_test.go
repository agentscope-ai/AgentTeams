package slicesx

import (
	"reflect"
	"testing"
)

func TestContains(t *testing.T) {
	values := []string{"a", "b", "c"}
	if !Contains(values, "b") {
		t.Fatal("expected b to be present")
	}
	if Contains(values, "z") {
		t.Fatal("expected z to be absent")
	}
	if Contains(nil, "a") {
		t.Fatal("expected nil slice to be absent")
	}
}

func TestUniqueNonEmpty(t *testing.T) {
	got := UniqueNonEmpty([]string{"", "a", "b", "a", "", "c", "b"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UniqueNonEmpty() = %v, want %v", got, want)
	}
}

func TestAppendUnique(t *testing.T) {
	base := []string{"a", "", "b"}
	got := AppendUnique(base, "", "b", "c", "a")
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AppendUnique() = %v, want %v", got, want)
	}
}
