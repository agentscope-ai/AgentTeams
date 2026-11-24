package matrix

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResponseBodyReadFailures(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(map[bool]string{false: "truncated", true: "timeout"}[stalled], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "1024")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("partial"))
				w.(http.Flusher).Flush()
				if stalled {
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			c := NewTuwunelClient(Config{ServerURL: server.URL}, &http.Client{Timeout: 100 * time.Millisecond})
			status, body, err := c.doJSON(context.Background(), http.MethodGet, "/body", "", nil, nil)
			if status != http.StatusOK || string(body) != "partial" {
				t.Fatalf("status=%d body=%q; want 200 and preserved partial bytes", status, body)
			}
			if stalled {
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("expected timeout, got %v", err)
				}
			} else if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("expected unexpected EOF, got %v", err)
			}
		})
	}
}

func TestSetDisplayNameDoesNotHideBodyTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	c := NewTuwunelClient(Config{ServerURL: server.URL}, &http.Client{Timeout: 100 * time.Millisecond})
	if err := c.SetDisplayName(context.Background(), "@test:local", "synthetic-token", "test"); err == nil {
		t.Fatal("unfinished HTTP 200 response reported operation success")
	}
}
