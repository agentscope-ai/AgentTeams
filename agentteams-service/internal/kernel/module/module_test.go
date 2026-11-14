package module

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fakeModule records lifecycle calls and serves fixed errors/health.
type fakeModule struct {
	id       string
	calls    *[]string
	initErr  error
	startErr error
	stopErr  error
	health   Health
}

func (m *fakeModule) ID() string { return m.id }

func (m *fakeModule) Init(ctx context.Context) error {
	*m.calls = append(*m.calls, "init:"+m.id)
	return m.initErr
}

func (m *fakeModule) Start(ctx context.Context) error {
	*m.calls = append(*m.calls, "start:"+m.id)
	return m.startErr
}

func (m *fakeModule) Stop(ctx context.Context) error {
	*m.calls = append(*m.calls, "stop:"+m.id)
	return m.stopErr
}

func (m *fakeModule) Health(ctx context.Context) Health { return m.health }

func newFake(id string, calls *[]string) *fakeModule {
	return &fakeModule{id: id, calls: calls, health: Health{Status: StatusOK}}
}

func TestRegisterDuplicateID(t *testing.T) {
	r := NewRegistry()
	calls := []string{}
	a1 := newFake("a", &calls)
	a2 := newFake("a", &calls)
	if err := r.Register(a1); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(a2); err == nil {
		t.Fatal("duplicate ID accepted")
	} else if !strings.Contains(err.Error(), `duplicate ID "a"`) {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := r.Register(nil); err == nil {
		t.Fatal("nil module accepted")
	}
}

func TestInitStartOrder(t *testing.T) {
	r := NewRegistry()
	calls := []string{}
	for _, id := range []string{"a", "b", "c"} {
		if err := r.Register(newFake(id, &calls)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.InitAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"init:a", "init:b", "init:c", "start:a", "start:b", "start:c"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestInitAllStopsAtFirstError(t *testing.T) {
	r := NewRegistry()
	calls := []string{}
	if err := r.Register(newFake("a", &calls)); err != nil {
		t.Fatal(err)
	}
	b := newFake("b", &calls)
	b.initErr = errors.New("b init failed")
	if err := r.Register(b); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(newFake("c", &calls)); err != nil {
		t.Fatal(err)
	}
	err := r.InitAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "b init failed") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"init:a", "init:b"}) {
		t.Fatalf("calls = %v, want stop after b", calls)
	}
}

func TestStopAllReverseAndJoined(t *testing.T) {
	r := NewRegistry()
	calls := []string{}
	a := newFake("a", &calls)
	a.stopErr = errors.New("a stop failed")
	b := newFake("b", &calls)
	c := newFake("c", &calls)
	c.stopErr = errors.New("c stop failed")
	for _, m := range []*fakeModule{a, b, c} {
		if err := r.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	err := r.StopAll(context.Background())
	if !reflect.DeepEqual(calls, []string{"stop:c", "stop:b", "stop:a"}) {
		t.Fatalf("calls = %v, want reverse order", calls)
	}
	if err == nil {
		t.Fatal("expected joined stop errors")
	}
	for _, want := range []string{"a stop failed", "c stop failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q missing %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "module a") || !strings.Contains(err.Error(), "module c") {
		t.Fatalf("err %q missing module IDs", err)
	}
}

func TestStopAllNoErrorWhenClean(t *testing.T) {
	r := NewRegistry()
	calls := []string{}
	if err := r.Register(newFake("a", &calls)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(newFake("b", &calls)); err != nil {
		t.Fatal(err)
	}
	if err := r.StopAll(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestHealthAllWorst(t *testing.T) {
	cases := []struct {
		name     string
		statuses []Status
		want     Status
	}{
		{"all ok", []Status{StatusOK, StatusOK}, StatusOK},
		{"ok and degraded", []Status{StatusOK, StatusDegraded}, StatusDegraded},
		{"degraded and down", []Status{StatusDegraded, StatusDown}, StatusDown},
		{"single down", []Status{StatusDown}, StatusDown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewRegistry()
			for i, s := range c.statuses {
				m := &fakeModule{
					id:     fmt.Sprintf("m%d", i),
					health: Health{Status: s, Detail: "detail"},
				}
				if err := r.Register(m); err != nil {
					t.Fatal(err)
				}
			}
			got := r.HealthAll(context.Background())
			if got.Status != c.want {
				t.Fatalf("status = %s, want %s", got.Status, c.want)
			}
			if c.want == StatusOK && got.Detail != "" {
				t.Fatalf("detail = %q, want empty", got.Detail)
			}
		})
	}
}

func TestHealthEmptyRegistry(t *testing.T) {
	got := NewRegistry().HealthAll(context.Background())
	if got.Status != StatusOK {
		t.Fatalf("status = %s, want ok", got.Status)
	}
}
