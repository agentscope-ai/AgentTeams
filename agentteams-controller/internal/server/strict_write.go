package server

import (
	"context"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// strictConditionalWriter is probed, not required: a backend that cannot offer
// atomic conditional writes simply does not implement it, and the caller fails
// closed. Declaring the method on oss.StorageClient would make every
// implementation "support" it, which is exactly what the 503 path must detect.
type strictConditionalWriter interface {
	PutObjectIfMatchStrict(ctx context.Context, key string, data []byte, matchETag string) error
}

// strictPut performs the conditional write, or reports the capability missing.
func strictPut(ctx context.Context, store oss.StorageClient, key string, payload []byte, etag string) error {
	w, ok := store.(strictConditionalWriter)
	if !ok {
		return oss.ErrConditionalUnsupported
	}
	return w.PutObjectIfMatchStrict(ctx, key, payload, etag)
}
