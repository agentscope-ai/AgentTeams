package httpapi

import "net/http"

// Mount registers a business handler subtree on the service face.
//
// The kernel owns /healthz and /readyz (registered in New); modules and the
// HTTP layer mount their public prefixes here during wiring (2b2:
// /api/v1/workers). Everything mounted here is wrapped by the kernel
// middleware chain in Handler (request id → panic recovery → access log),
// so business handlers get observability for free.
//
// A duplicate pattern panics by design: a route registered twice is a wiring
// bug that must fail loudly at startup, not silently shadow a handler.
func (s *Server) Mount(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// MountFunc is the http.HandlerFunc form of Mount.
func (s *Server) MountFunc(pattern string, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, h)
}
