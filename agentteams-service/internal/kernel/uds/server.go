// Package uds serves the internal health face on a Unix domain socket
// and only the /healthz /readyz routes are ever mounted here
// (the caller passes httpapi.HealthHandler), the socket is 0600, a stale
// socket from a crashed instance is removed before listening, and the
// socket file is removed on shutdown.
package uds

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// Server is the UDS health face.
type Server struct {
	path    string
	handler http.Handler
	log     *slog.Logger
	ln      net.Listener
	srv     *http.Server
}

// New builds a UDS server for the socket at path. lg nil → slog.Default().
func New(path string, handler http.Handler, lg *slog.Logger) *Server {
	if lg == nil {
		lg = slog.Default()
	}
	return &Server{path: path, handler: handler, log: lg}
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Listen removes a stale socket, binds the unix listener, and sets the
// file to 0600. It does not serve; call Serve (blocking) or Shutdown.
func (s *Server) Listen() error {
	// A stale socket (crashed instance) is removed unconditionally; a
	// live one would fail to bind anyway, and removing a plain file is
	// harmless.
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("uds: remove stale socket %s: %w", s.path, err)
	}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("uds: listen %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		ln.Close()
		os.Remove(s.path)
		return fmt.Errorf("uds: chmod 0600 %s: %w", s.path, err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	return nil
}

// Serve blocks serving HTTP on the unix socket (call after Listen).
func (s *Server) Serve() error {
	if s.srv == nil {
		return fmt.Errorf("uds: Listen not called")
	}
	err := s.srv.Serve(s.ln)
	if err != nil && err != http.ErrServerClosed {
		s.log.Error("uds serve", "err", err)
	}
	return err
}

// Shutdown stops the server (bounded by ctx) and removes the socket
// file. Safe to call when Listen was never called.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		if err := s.srv.Shutdown(ctx); err != nil {
			s.log.Warn("uds shutdown", "err", err)
		}
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("uds: remove socket %s: %w", s.path, err)
	}
	return nil
}
