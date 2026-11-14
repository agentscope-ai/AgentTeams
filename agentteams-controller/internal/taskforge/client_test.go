package taskforge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef"

func testClaims() Claims {
	return Claims{
		ActorType: ActorWorker,
		ActorID:   "w-1",
		TeamID:    "sysdev",
		RequestID: "req-9",
		TraceID:   "tr-9",
		TokenJTI:  "jti-9",
		IssuedAt:  time.Now().Unix(),
	}
}

func TestDoSendsSignedClaimHeaders(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)

	cl := testClaims()
	c := NewClient(base, testSecret, time.Second)
	ctx := WithForwardedHeaders(context.Background(), "idem-1", `"v3"`)
	resp, err := c.Do(ctx, cl, http.MethodPost, "/internal/v1/tasks/t-1/claim", url.Values{"limit": []string{"10"}}, []byte(`{"note":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	wantHeaders := map[string]string{
		HeaderActorType:  "worker",
		HeaderActor:      "w-1",
		HeaderOnBehalfOf: "",
		HeaderTeam:       "sysdev",
		HeaderRequestID:  "req-9",
		HeaderTraceID:    "tr-9",
		HeaderTokenJTI:   "jti-9",
		HeaderIssuedAt:   strconv.FormatInt(cl.IssuedAt, 10),
	}
	for k, want := range wantHeaders {
		if gotv := got.Header.Get(k); gotv != want {
			t.Errorf("header %s = %q, want %q", k, gotv, want)
		}
	}
	sig := got.Header.Get(HeaderSignature)
	if sig == "" {
		t.Fatal("missing signature header")
	}
	if err := Verify(testSecret, cl, sig, time.Now()); err != nil {
		t.Fatalf("signature not verifiable with the same secret: %v", err)
	}

	// Idempotency-Key / If-Match forwarded unchanged.
	if got.Header.Get("Idempotency-Key") != "idem-1" || got.Header.Get("If-Match") != `"v3"` {
		t.Fatalf("forwarded headers = %q / %q", got.Header.Get("Idempotency-Key"), got.Header.Get("If-Match"))
	}

	// Fixed path rewrite + query pass-through.
	if got.URL.Path != "/internal/v1/tasks/t-1/claim" {
		t.Fatalf("upstream path = %q", got.URL.Path)
	}
	if got.URL.RawQuery != "limit=10" {
		t.Fatalf("upstream query = %q, want limit=10", got.URL.RawQuery)
	}
	if string(gotBody) != `{"note":"x"}` {
		t.Fatalf("body = %q, want the exact request bytes", gotBody)
	}
}

func TestDoRefusesNonInternalPath(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:1")
	c := NewClient(base, testSecret, time.Second)
	cl := Claims{ActorType: ActorSystem, IssuedAt: time.Now().Unix()}
	for _, p := range []string{"/api/v1/tasks", "//etc/passwd", "internal/v1/loops", "/internal/v2/loops", ""} {
		if _, err := c.Do(context.Background(), cl, http.MethodGet, p, nil, nil); err == nil {
			t.Fatalf("path %q accepted, want refusal", p)
		}
	}
}

func TestDoConnectionFailureIsUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := upstream.URL
	upstream.Close() // closed listener: connections are refused
	base, _ := url.Parse(addr)

	c := NewClient(base, testSecret, time.Second)
	cl := Claims{ActorType: ActorSystem, IssuedAt: time.Now().Unix()}
	if _, err := c.Do(context.Background(), cl, http.MethodGet, "/internal/v1/healthz", nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestDoTimeoutIsUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
		}
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)

	c := NewClient(base, testSecret, 100*time.Millisecond)
	cl := Claims{ActorType: ActorSystem, IssuedAt: time.Now().Unix()}
	if _, err := c.Do(context.Background(), cl, http.MethodGet, "/internal/v1/loops", nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	upstream.CloseClientConnections()
}

func TestHealthParsesBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/healthz" {
			t.Errorf("health path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true,"version":"1.2.7","protocol":"atf"}`))
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)

	c := NewClient(base, testSecret, time.Second)
	st, err := c.Health(context.Background(), Claims{ActorType: ActorSystem, IssuedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Healthy || st.Version != "1.2.7" || st.Protocol != "atf" {
		t.Fatalf("status = %+v", st)
	}
}

func TestHealthNon2xxIsUnhealthy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("down"))
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)

	c := NewClient(base, testSecret, time.Second)
	st, err := c.Health(context.Background(), Claims{ActorType: ActorSystem, IssuedAt: time.Now().Unix()})
	if err != nil {
		t.Fatalf("err = %v, want nil (unhealthy is not an error)", err)
	}
	if st.Healthy {
		t.Fatal("non-2xx healthz must report unhealthy")
	}
}

func TestHealthConnectionFailureIsUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := upstream.URL
	upstream.Close()
	base, _ := url.Parse(addr)

	c := NewClient(base, testSecret, time.Second)
	if _, err := c.Health(context.Background(), Claims{ActorType: ActorSystem, IssuedAt: time.Now().Unix()}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
