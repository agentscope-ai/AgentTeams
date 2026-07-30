package server

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestHTTPServer_StartSwallowsErrServerClosed verifies the production
// invariant that Start returns nil (not http.ErrServerClosed) once Shutdown
// has closed the server. Without this, the goroutine that watches Start's
// return value in app.Start would log a spurious error on every shutdown.
func TestHTTPServer_StartSwallowsErrServerClosed(t *testing.T) {
	s := &HTTPServer{
		Addr:   "127.0.0.1:0",
		Mux:    http.NewServeMux(),
		server: &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux()},
	}
	// Pre-close so the subsequent ListenAndServe immediately returns
	// ErrServerClosed without binding a port.
	_ = s.server.Close()

	if err := s.Start(); err != nil {
		t.Fatalf("Start should swallow ErrServerClosed, got %v", err)
	}
}

// TestHTTPServer_ShutdownStopsStart exercises the full lifecycle: Start
// runs in a goroutine, Shutdown is called with a deadlined ctx, and Start
// must return cleanly within the deadline.
func TestHTTPServer_ShutdownStopsStart(t *testing.T) {
	s := &HTTPServer{
		Addr: "127.0.0.1:0",
		Mux:  http.NewServeMux(),
	}
	s.server = &http.Server{Addr: s.Addr, Handler: s.Mux}

	done := make(chan error, 1)
	go func() { done <- s.Start() }()

	time.Sleep(50 * time.Millisecond) // let ListenAndServe bind

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return within 2s of Shutdown")
	}
}
