package resources

import "net/http"

// The kernel wiring builds the public /api/v1/workers face by wrapping each
// native route with the identity middleware. These accessors expose the
// handler funcs without making the unexported methods part of the package
// API; Register (used by tests) stays the unwrapped form.
//
// PUT / DELETE are served by Proxy (transparent controller proxy),
// not by the native handler set.

// CreateHTTP serves POST /api/v1/workers.
func (h *Handler) CreateHTTP() http.HandlerFunc { return h.createWorker }

// ListHTTP serves GET /api/v1/workers.
func (h *Handler) ListHTTP() http.HandlerFunc { return h.listWorkers }

// GetHTTP serves GET /api/v1/workers/{name}.
func (h *Handler) GetHTTP() http.HandlerFunc { return h.getWorker }

// UpdateHTTP serves PUT /api/v1/workers/{name} through the transparent
// proxy.
func (p *Proxy) UpdateHTTP() http.HandlerFunc { return p.handleUpdate }

// DeleteHTTP serves DELETE /api/v1/workers/{name} through the transparent
// proxy.
func (p *Proxy) DeleteHTTP() http.HandlerFunc { return p.handleDelete }

// ReceiveHTTP serves POST /internal/v1/workers/status (controller → service
// observation writeback); the kernel wraps it with httpapi.InternalAuth.
func (h *StatusHandler) ReceiveHTTP() http.HandlerFunc { return h.receive }
