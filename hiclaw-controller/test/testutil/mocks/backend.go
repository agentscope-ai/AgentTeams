package mocks

import (
	"context"
	"sync"

	"github.com/hiclaw/hiclaw-controller/internal/backend"
)

// MockWorkerBackend implements backend.WorkerBackend for testing.
//
// It tracks container lifecycle state automatically: Create sets a container
// to Running, Delete removes it, Stop sets it to Stopped, Start sets it back
// to Running. Status queries return the tracked state (or ErrNotFound if
// no container exists). Fn overrides take precedence over tracked state.
type MockWorkerBackend struct {
	mu sync.Mutex

	NameOverride string

	CreateFn func(ctx context.Context, req backend.CreateRequest) (*backend.WorkerResult, error)
	DeleteFn func(ctx context.Context, name string) error
	StartFn  func(ctx context.Context, name string) error
	StopFn   func(ctx context.Context, name string) error
	StatusFn func(ctx context.Context, name string) (*backend.WorkerResult, error)
	ListFn   func(ctx context.Context) ([]backend.WorkerResult, error)

	containerState map[string]backend.WorkerStatus

	Calls struct {
		Create     []string
		CreateReqs []backend.CreateRequest
		Delete     []string
		Start      []string
		Stop       []string
		Status     []string
		List       int
	}
}

func NewMockWorkerBackend() *MockWorkerBackend {
	return &MockWorkerBackend{
		containerState: map[string]backend.WorkerStatus{},
	}
}

// Reset clears all Fn overrides, call records, and tracked container state.
func (m *MockWorkerBackend) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
	m.containerState = map[string]backend.WorkerStatus{}
	m.NameOverride = ""
	m.CreateFn = nil
	m.DeleteFn = nil
	m.StartFn = nil
	m.StopFn = nil
	m.StatusFn = nil
	m.ListFn = nil
}

// ClearCalls resets call records only, preserving Fn overrides and container state.
func (m *MockWorkerBackend) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockWorkerBackend) clearCallsLocked() {
	m.Calls = struct {
		Create     []string
		CreateReqs []backend.CreateRequest
		Delete     []string
		Start      []string
		Stop       []string
		Status     []string
		List       int
	}{}
}

// SimulatePodDeletion removes a container from tracked state, simulating
// an external deletion (e.g. kubectl delete pod). Subsequent Status calls
// will return ErrNotFound (unless StatusFn is set).
func (m *MockWorkerBackend) SimulatePodDeletion(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.containerState, name)
}

func (m *MockWorkerBackend) Name() string {
	if m.NameOverride != "" {
		return m.NameOverride
	}
	return "mock"
}
func (m *MockWorkerBackend) DeploymentMode() string           { return backend.DeployLocal }
func (m *MockWorkerBackend) Available(_ context.Context) bool { return true }
func (m *MockWorkerBackend) NeedsCredentialInjection() bool   { return false }

func (m *MockWorkerBackend) Create(ctx context.Context, req backend.CreateRequest) (*backend.WorkerResult, error) {
	m.mu.Lock()
	m.Calls.Create = append(m.Calls.Create, req.Name)
	m.Calls.CreateReqs = append(m.Calls.CreateReqs, req)
	fn := m.CreateFn
	m.mu.Unlock()

	setRunning := func() {
		m.mu.Lock()
		m.containerState[req.Name] = backend.StatusRunning
		if req.ContainerName != "" && req.ContainerName != req.Name {
			m.containerState[req.ContainerName] = backend.StatusRunning
		}
		m.mu.Unlock()
	}

	if fn != nil {
		result, err := fn(ctx, req)
		if err == nil {
			setRunning()
		}
		return result, err
	}

	setRunning()
	return &backend.WorkerResult{
		Name:    req.Name,
		Backend: m.Name(),
		Status:  backend.StatusStarting,
	}, nil
}

func (m *MockWorkerBackend) Delete(ctx context.Context, name string) error {
	m.mu.Lock()
	m.Calls.Delete = append(m.Calls.Delete, name)
	fn := m.DeleteFn
	m.mu.Unlock()

	if fn != nil {
		err := fn(ctx, name)
		if err == nil {
			m.mu.Lock()
			delete(m.containerState, name)
			m.mu.Unlock()
		}
		return err
	}

	m.mu.Lock()
	delete(m.containerState, name)
	m.mu.Unlock()
	return nil
}

func (m *MockWorkerBackend) Start(ctx context.Context, name string) error {
	m.mu.Lock()
	m.Calls.Start = append(m.Calls.Start, name)
	fn := m.StartFn
	m.mu.Unlock()

	if fn != nil {
		err := fn(ctx, name)
		if err == nil {
			m.mu.Lock()
			m.containerState[name] = backend.StatusRunning
			m.mu.Unlock()
		}
		return err
	}

	m.mu.Lock()
	m.containerState[name] = backend.StatusRunning
	m.mu.Unlock()
	return nil
}

func (m *MockWorkerBackend) Stop(ctx context.Context, name string) error {
	m.mu.Lock()
	m.Calls.Stop = append(m.Calls.Stop, name)
	fn := m.StopFn
	m.mu.Unlock()

	if fn != nil {
		err := fn(ctx, name)
		if err == nil {
			m.mu.Lock()
			m.containerState[name] = backend.StatusStopped
			m.mu.Unlock()
		}
		return err
	}

	m.mu.Lock()
	m.containerState[name] = backend.StatusStopped
	m.mu.Unlock()
	return nil
}

func (m *MockWorkerBackend) Status(ctx context.Context, name string) (*backend.WorkerResult, error) {
	m.mu.Lock()
	m.Calls.Status = append(m.Calls.Status, name)
	fn := m.StatusFn
	state, tracked := m.containerState[name]
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, name)
	}
	if tracked {
		return &backend.WorkerResult{
			Name:    name,
			Backend: m.Name(),
			Status:  state,
		}, nil
	}
	return &backend.WorkerResult{
		Name:    name,
		Backend: m.Name(),
		Status:  backend.StatusNotFound,
	}, nil
}

func (m *MockWorkerBackend) List(ctx context.Context) ([]backend.WorkerResult, error) {
	m.mu.Lock()
	m.Calls.List++
	fn := m.ListFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil, nil
}

// LastCreateReq returns the most recent CreateRequest recorded by Create,
// or (zero, false) if Create has not been invoked.
func (m *MockWorkerBackend) LastCreateReq() (backend.CreateRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Calls.CreateReqs) == 0 {
		return backend.CreateRequest{}, false
	}
	return m.Calls.CreateReqs[len(m.Calls.CreateReqs)-1], true
}

// FindCreateReq returns the most recent CreateRequest whose Name matches the
// given value, or (zero, false) if no matching request was recorded.
func (m *MockWorkerBackend) FindCreateReq(name string) (backend.CreateRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.Calls.CreateReqs) - 1; i >= 0; i-- {
		if m.Calls.CreateReqs[i].Name == name {
			return m.Calls.CreateReqs[i], true
		}
	}
	return backend.CreateRequest{}, false
}

// CallSnapshot returns a snapshot of call records safe for concurrent use.
func (m *MockWorkerBackend) CallSnapshot() (creates, deletes, starts, stops, statuses []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	creates = append([]string{}, m.Calls.Create...)
	deletes = append([]string{}, m.Calls.Delete...)
	starts = append([]string{}, m.Calls.Start...)
	stops = append([]string{}, m.Calls.Stop...)
	statuses = append([]string{}, m.Calls.Status...)
	return
}

var _ backend.WorkerBackend = (*MockWorkerBackend)(nil)
