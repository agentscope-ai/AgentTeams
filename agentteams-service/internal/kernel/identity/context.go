package identity

import "context"

type ctxKey struct{}

// WithIdentity stores the resolved caller on the request context. The
// 2c auth middleware (token-review) sets it; the 2b2 handlers read it.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the caller identity, or nil when the middleware did
// not run (handlers must treat nil as unauthenticated).
func FromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(ctxKey{}).(*Identity)
	return id
}
