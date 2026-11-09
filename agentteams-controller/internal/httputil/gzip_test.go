package httputil

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func gunzipBody(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip read: %v", err)
	}
	return out
}

func doGzip(handler http.Handler, path string, hdrs map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// listBody is a JSON body large enough that gzip actually shrinks it
// (small bodies can be LARGER compressed; the equivalence assertions do
// not depend on the size, the reduction check does).
const listBody = `{"path":"files/","entries":[` + `"a000.txt","a001.txt","a002.txt","a003.txt","a004.txt",` +
	`"a005.txt","a006.txt","a007.txt","a008.txt","a009.txt",` +
	`"a010.txt","a011.txt","a012.txt","a013.txt","a014.txt",` +
	`"a015.txt","a016.txt","a017.txt","a018.txt","a019.txt",` +
	`"a020.txt","a021.txt","a022.txt","a023.txt","a024.txt",` +
	`"a025.txt","a026.txt","a027.txt","a028.txt","a029.txt",` +
	`"a030.txt","a031.txt","a032.txt","a033.txt","a034.txt",` +
	`"a035.txt","a036.txt","a037.txt","a038.txt","a039.txt",` +
	`"a040.txt","a041.txt","a042.txt","a043.txt","a044.txt",` +
	`"a045.txt","a046.txt","a047.txt","a048.txt","a049.txt"` + `]}`

// TestGzip_IdentityVsGzipEquivalence pins the core contract: without
// Accept-Encoding the body goes out byte-identical (no Content-Encoding,
// Content-Length = raw length); with Accept-Encoding: gzip the body is the
// gzip of the SAME bytes (gunzips back to equality), Content-Length
// answers the COMPRESSED length, and the encoding actually saves bytes.
func TestGzip_IdentityVsGzipEquivalence(t *testing.T) {
	handler := GzipMiddleware(jsonHandler(http.StatusOK, listBody))

	recID := doGzip(handler, "/x", nil)
	if recID.Code != http.StatusOK {
		t.Fatalf("identity status = %d, want 200", recID.Code)
	}
	if recID.Body.String() != listBody {
		t.Fatalf("identity body not byte-identical")
	}
	if ce := recID.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("identity carries Content-Encoding %q", ce)
	}
	if got, want := recID.Header().Get("Content-Length"), strconv.Itoa(len(listBody)); got != want {
		t.Errorf("identity Content-Length = %q, want %q", got, want)
	}

	recGZ := doGzip(handler, "/x", map[string]string{"Accept-Encoding": "gzip"})
	if recGZ.Code != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200", recGZ.Code)
	}
	if ce := recGZ.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q, want gzip", ce)
	}
	got := gunzipBody(t, recGZ.Body.Bytes())
	if string(got) != listBody {
		t.Fatalf("gunzipped body is not the identity body")
	}
	if gotCL := recGZ.Header().Get("Content-Length"); gotCL != strconv.Itoa(recGZ.Body.Len()) {
		t.Errorf("gzip Content-Length = %q, want the compressed length %d", gotCL, recGZ.Body.Len())
	}
	if recGZ.Body.Len() >= len(listBody) {
		t.Errorf("gzip did not save bytes: %d >= %d", recGZ.Body.Len(), len(listBody))
	}
}

// TestGzip_VaryMerged pins requirement 3: the Vary of a wrapped route
// keeps the token the inner layer declared (Authorization, via
// ETagMiddleware) and adds Accept-Encoding — merged, not overwritten.
func TestGzip_VaryMerged(t *testing.T) {
	handler := GzipMiddleware(ETagMiddleware(jsonHandler(http.StatusOK, listBody)))

	for _, hdrs := range []map[string]string{
		nil,
		{"Accept-Encoding": "gzip"},
	} {
		rec := doGzip(handler, "/x", hdrs)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (hdrs %v)", rec.Code, hdrs)
		}
		got := rec.Header().Get("Vary")
		if got != "Authorization, Accept-Encoding" {
			t.Errorf("Vary = %q, want %q (hdrs %v)", got, "Authorization, Accept-Encoding", hdrs)
		}
	}

	// Without an inner ETag layer (sync/status is gzip-only) the Vary is
	// just the encoding token, still declared.
	bare := GzipMiddleware(jsonHandler(http.StatusOK, listBody))
	if got := doGzip(bare, "/x", nil).Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("bare Vary = %q, want Accept-Encoding", got)
	}
}

// TestGzip_304NoGzipBody pins requirement 4: a revalidation hit (304)
// carries no body, no Content-Encoding, Content-Length 0, and the current
// ETag — even when the request offered gzip.
func TestGzip_304NoGzipBody(t *testing.T) {
	handler := GzipMiddleware(ETagMiddleware(jsonHandler(http.StatusOK, listBody)))

	rec1 := doGzip(handler, "/x", map[string]string{"Accept-Encoding": "gzip"})
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec1.Code)
	}
	etag := rec1.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("first response missing ETag")
	}

	rec2 := doGzip(handler, "/x", map[string]string{
		"Accept-Encoding": "gzip",
		"If-None-Match":   etag,
	})
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("replay status = %d, want 304 (body %s)", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", rec2.Body.String())
	}
	if ce := rec2.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("304 carries Content-Encoding %q", ce)
	}
	if got := rec2.Header().Get("Content-Length"); got != "0" {
		t.Errorf("304 Content-Length = %q, want 0", got)
	}
	if got := rec2.Header().Get("ETag"); got != etag {
		t.Errorf("304 ETag = %q, want the current validator %q", got, etag)
	}
	if got := rec2.Header().Get("Vary"); got != "Authorization, Accept-Encoding" {
		t.Errorf("304 Vary = %q, want the merged form", got)
	}
}

// TestGzip_SameETagAcrossEncodings pins the pipeline invariant: the
// validator is computed over the uncompressed JSON, so identity and gzip
// requests of the same representation share one ETag.
func TestGzip_SameETagAcrossEncodings(t *testing.T) {
	handler := GzipMiddleware(ETagMiddleware(jsonHandler(http.StatusOK, listBody)))
	etagID := doGzip(handler, "/x", nil).Header().Get("ETag")
	etagGZ := doGzip(handler, "/x", map[string]string{"Accept-Encoding": "gzip"}).Header().Get("ETag")
	if etagID == "" || etagGZ == "" {
		t.Fatalf("missing ETags: identity %q / gzip %q", etagID, etagGZ)
	}
	if etagID != etagGZ {
		t.Fatalf("ETag differs by encoding: identity %q vs gzip %q", etagID, etagGZ)
	}
}

// TestGzip_SkipsNon200AndNonJSON pins the activation boundary: error JSON
// and non-JSON 200s pass through verbatim, never compressed and never
// carrying Content-Encoding — even when the client offered gzip.
func TestGzip_SkipsNon200AndNonJSON(t *testing.T) {
	t.Run("404-json", func(t *testing.T) {
		const body = `{"message":"nope"}`
		handler := GzipMiddleware(jsonHandler(http.StatusNotFound, body))
		rec := doGzip(handler, "/x", map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if rec.Body.String() != body {
			t.Fatalf("404 body not verbatim: %q", rec.Body.String())
		}
		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("404 carries Content-Encoding %q", ce)
		}
		if got, want := rec.Header().Get("Content-Length"), strconv.Itoa(len(body)); got != want {
			t.Errorf("404 Content-Length = %q, want %q", got, want)
		}
	})

	t.Run("200-text-plain", func(t *testing.T) {
		const body = "plain bytes, not json"
		handler := GzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		}))
		rec := doGzip(handler, "/x", map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.String() != body {
			t.Fatalf("text/plain body not verbatim: %q", rec.Body.String())
		}
		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("text/plain carries Content-Encoding %q", ce)
		}
	})
}

// TestAcceptsGzipEncoding pins the header parser: only an explicit gzip
// token with a non-zero quality counts; "*" is not a gzip offer.
func TestAcceptsGzipEncoding(t *testing.T) {
	for _, tc := range []struct {
		hdr  string
		want bool
	}{
		{hdr: "", want: false},
		{hdr: "identity", want: false},
		{hdr: "*", want: false},
		{hdr: "deflate", want: false},
		{hdr: "x-gzip", want: false},
		{hdr: "gzip", want: true},
		{hdr: "GZIP", want: true},
		{hdr: "gzip;q=1", want: true},
		{hdr: "gzip;q=0.1", want: true},
		{hdr: "gzip;q=0", want: false},
		{hdr: "gzip;q=0.0", want: false},
		{hdr: "br, gzip", want: true},
		{hdr: " gzip , br ", want: true},
		{hdr: "gzip;q=0, br", want: false},
		{hdr: "br;q=0, gzip", want: true},
		{hdr: "deflate;q=0.5, gzip;q=0", want: false},
	} {
		if got := acceptsGzipEncoding(http.Header{"Accept-Encoding": []string{tc.hdr}}); got != tc.want {
			t.Errorf("acceptsGzipEncoding(%q) = %v, want %v", tc.hdr, got, tc.want)
		}
	}
}

// TestGzip_ContentLengthFinal pins that the Content-Length of every pass
// answers the final wire body: 200 identity = raw length, 200 gzip =
// compressed length (the inner uncompressed value replaced), 304 = 0.
func TestGzip_ContentLengthFinal(t *testing.T) {
	handler := GzipMiddleware(ETagMiddleware(jsonHandler(http.StatusOK, listBody)))

	recID := doGzip(handler, "/x", nil)
	if recID.Code != http.StatusOK {
		t.Fatalf("identity status = %d, want 200", recID.Code)
	}
	if got := recID.Header().Get("Content-Length"); got != strconv.Itoa(len(listBody)) {
		t.Errorf("identity Content-Length = %q, want the raw length %d", got, len(listBody))
	}

	recGZ := doGzip(handler, "/x", map[string]string{"Accept-Encoding": "gzip"})
	if recGZ.Code != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200", recGZ.Code)
	}
	// The ETag layer wrote the UNCOMPRESSED length; the final header must
	// answer the compressed wire body.
	if got := recGZ.Header().Get("Content-Length"); got != strconv.Itoa(recGZ.Body.Len()) {
		t.Errorf("gzip Content-Length = %q, want the compressed length %d (uncompressed was %d)",
			got, recGZ.Body.Len(), len(listBody))
	}

	rec304 := doGzip(handler, "/x", map[string]string{
		"Accept-Encoding": "gzip",
		"If-None-Match":   recGZ.Header().Get("ETag"),
	})
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("replay status = %d, want 304", rec304.Code)
	}
	if got := rec304.Header().Get("Content-Length"); got != "0" {
		t.Errorf("304 Content-Length = %q, want 0", got)
	}
}
