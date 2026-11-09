package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// humanFileRangeSegment bounds one ranged read window: the seeker opens the
// object in segments of at most this many bytes, so a streamed download of
// a large file costs one ranged open per window instead of one open per
// requested range.
const humanFileRangeSegment = 4 << 20

// humanFilePrimeRetries bounds how many times a pre-header ranged open is
// repeated against a fresh stat after the open reports the object moved
// (412) or shrank past the range (416). Exhaustion is 503
// snapshot_unstable, never a partial or mixed read.
const humanFilePrimeRetries = 2

// errHumanFileTombstone signals that the GET target is a soft-deleted
// path: the answer is the ordinary 404, identical to a missing file.
var errHumanFileTombstone = errors.New("human file is a tombstone")

// errRangeNotSatisfiable signals that the Range header cannot be satisfied
// by the current object size: the answer is 416 with
// "Content-Range: bytes */<size>" and no body.
var errRangeNotSatisfiable = errors.New("human file range not satisfiable")

// fileRepresentation is the GET side of one object, resolved from a single
// StatMeta: the size and validator that drive the 304/416/If-Range
// decisions, the extension-derived Content-Type, and the tombstone flag.
// etag is the BARE (unquoted) validator; the response header renders it
// through httputil.EtagQuoted.
type fileRepresentation struct {
	size        int64
	modTime     time.Time
	etag        string
	contentType string
	tombstone   bool
}

// statFileRepresentation resolves key's representation with exactly one
// StatMeta. rel is the path relative to the human-files root, used for the
// extension-based Content-Type (the object's stored content type is the
// upload metadata, not the delivery one).
func (h *humanFilesHandler) statFileRepresentation(ctx context.Context, key, rel string) (fileRepresentation, error) {
	meta, err := h.oss.StatMeta(ctx, key)
	if err != nil {
		return fileRepresentation{}, err
	}
	return fileRepresentation{
		size:        meta.Size,
		modTime:     meta.ModTime,
		etag:        httputil.BareETag(meta.ETag),
		contentType: contentTypeForName(rel),
		tombstone:   isHumanFileTombstone(meta),
	}, nil
}

// preflightRange validates a raw Range header against the representation
// size before any storage call, so a 416 needs only the one stat. It
// accepts exactly one satisfiable "bytes=a-b" / "bytes=a-" / "bytes=-n"
// spec, clamped to [0, size-1]:
//
//   - ok, !reject   : start..end inclusive and satisfiable;
//   - reject        : answer 416 with "Content-Range: bytes */<size>" —
//     the unit is not "bytes", the spec is multi-range, it is malformed
//     (a>b, "-0", an empty object with a Range), or it starts past the
//     end;
//   - !ok, !reject  : no Range header — serve the full body.
func preflightRange(raw string, size int64) (start, end int64, ok, reject bool) {
	if raw == "" {
		return 0, 0, false, false
	}
	spec, isBytes := strings.CutPrefix(raw, "bytes=")
	if !isBytes {
		return 0, 0, false, true
	}
	// Multi-range is out of scope: no multipart/byteranges answers here.
	if strings.Contains(spec, ",") {
		return 0, 0, false, true
	}
	if size == 0 {
		// An empty object cannot satisfy any byte range.
		return 0, 0, false, true
	}
	spec = strings.TrimSpace(spec)
	first, second, hasDash := strings.Cut(spec, "-")
	if !hasDash {
		return 0, 0, false, true
	}
	first = strings.TrimSpace(first)
	second = strings.TrimSpace(second)
	switch {
	case first == "" && second == "":
		// "bytes=-" is malformed, and "bytes=-0" asks for zero bytes.
		return 0, 0, false, true
	case first == "":
		// Suffix form "-n": the last n bytes.
		n, err := parseRangeBound(second)
		if err != nil || n == 0 {
			return 0, 0, false, true
		}
		start = size - n
		if start < 0 {
			start = 0
		}
		return start, size - 1, true, false
	case second == "":
		// "a-": from a to the end.
		a, err := parseRangeBound(first)
		if err != nil || a >= size {
			return 0, 0, false, true
		}
		return a, size - 1, true, false
	default:
		a, errA := parseRangeBound(first)
		b, errB := parseRangeBound(second)
		if errA != nil || errB != nil || a > b || a >= size {
			return 0, 0, false, true
		}
		if b >= size {
			b = size - 1
		}
		return a, b, true, false
	}
}

// parseRangeBound parses a non-negative decimal range bound. A value that
// overflows int64 reports math.MaxInt64 so the caller's size comparison
// rejects or clamps it without a second failure mode.
func parseRangeBound(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty range bound")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("range bound is not a non-negative decimal")
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return math.MaxInt64, nil
		}
		return 0, err
	}
	return n, nil
}

// openForRead resolves the GET representation before any response byte is
// committed, so every storage fault still answers with headers, never a
// broken body. Per attempt: one StatMeta, If-None-Match against the fresh
// validator (a hit is 304 — no further read, ever), the Range header
// prefetched against the fresh size, If-Range evaluated against the fresh
// validator, and — for every GET with a body — the pre-header prime open
// bound to the snapshot's ETag (the requested window for a ranged GET,
// the single [0,size) stream for a full-body GET, the full etag-bound
// snapshot on a store without RangeReader). A 412/416 from that open
// means the object moved under us: re-stat and re-decide, bounded by
// humanFilePrimeRetries; exhaustion is errHumanFileSnapshotUnstable (503
// snapshot_unstable).
//
// The notModified result tells the caller to answer 304 (no body) for the
// rep; a nil seeker with notModified=false means "seeker ready".
func (h *humanFilesHandler) openForRead(ctx context.Context, r *http.Request, key, rel string) (fileRepresentation, *objectSeeker, bool, error) {
	rangeHdr := r.Header.Get("Range")
	ifRange := r.Header.Get("If-Range")
	ifNoneMatch := r.Header.Get("If-None-Match")
	// HEAD prices at the stat only: no pre-header open, ever.
	serveRange := r.Method == http.MethodGet

	for attempt := 0; attempt <= humanFilePrimeRetries; attempt++ {
		rep, err := h.statFileRepresentation(ctx, key, rel)
		if err != nil {
			return fileRepresentation{}, nil, false, err
		}
		if rep.tombstone {
			return fileRepresentation{}, nil, false, errHumanFileTombstone
		}
		if httputil.IfNoneMatchMatches(ifNoneMatch, rep.etag) {
			// Revalidation hit: the 304 costs exactly this one stat —
			// no pre-header open, no body.
			return rep, nil, true, nil
		}

		start, end, hasRange, reject := preflightRange(rangeHdr, rep.size)
		if reject {
			return rep, nil, false, errRangeNotSatisfiable
		}
		// If-Range against the fresh validator: a mismatch answers the
		// full current body, so drop the range before the content is
		// served. On a match the range is kept, but If-Range itself must
		// still be removed: ServeContent sees our quoted response ETag
		// and would compare the If-Range value against that ETag as a
		// token, ignoring the HTTP-date form. The decision has already
		// been made here against the stored validator.
		if hasRange && ifRange != "" {
			if !httputil.IfRangeApplies(ifRange, rep.etag, rep.modTime) {
				r.Header.Del("Range")
				r.Header.Del("If-Range")
				hasRange = false
			} else {
				r.Header.Del("If-Range")
			}
		}
		// If-None-Match was evaluated against the fresh validator above;
		// never let ServeContent re-evaluate it against the quoted header.
		r.Header.Del("If-None-Match")

		seeker := newObjectSeeker(h, ctx, key, rep)
		// Every GET with a body primes its first stream before the 200
		// is committed, so a store fault or a version move answers 503
		// instead of a 200 that dies on its first byte: a ranged GET
		// primes the requested window, a full-body GET primes the
		// single [0,size) stream — and on a store without RangeReader
		// Prime delegates to the etag-bound snapshot load. An empty
		// object and a HEAD prime nothing.
		var perr error
		switch {
		case serveRange && hasRange:
			perr = seeker.Prime(start, end-start+1)
		case serveRange && rep.size > 0:
			perr = seeker.Prime(0, rep.size)
		}
		if perr == nil {
			return rep, seeker, false, nil
		}
		// Prime opened a stream on this attempt. Close it before the
		// error return or the next retry; the caller's defer only sees a
		// successfully returned seeker.
		_ = seeker.Close()
		if !errors.Is(perr, oss.ErrPreconditionFailed) && !errors.Is(perr, oss.ErrRangeNotSatisfiable) {
			return fileRepresentation{}, nil, false, perr
		}
		if attempt == humanFilePrimeRetries {
			return fileRepresentation{}, nil, false, errHumanFileSnapshotUnstable
		}
		// The object moved (412) or shrank (416): re-stat,
		// re-preflight, re-open.
	}
	// Unreachable: the loop returns on every path (exhaustion above).
	return fileRepresentation{}, nil, false, errHumanFileSnapshotUnstable
}

// writeReadError maps an openForRead failure to its HTTP form: a deleted
// path and a missing key are the ordinary 404 (storage faults that turn
// out to be absences stay 404 per the PR-0 classification); a range the
// current size cannot satisfy is 416 with the full-size Content-Range;
// everything else follows the snapshot error mapping (unstable and
// unavailable are 503, the rest 500).
func (h *humanFilesHandler) writeReadError(w http.ResponseWriter, rep fileRepresentation, err error) {
	if errors.Is(err, errRangeNotSatisfiable) {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(rep.size, 10))
		httputil.WriteError(w, http.StatusRequestedRangeNotSatisfiable, "range not satisfiable")
		return
	}
	if errors.Is(err, errHumanFileTombstone) || errors.Is(err, os.ErrNotExist) {
		httputil.WriteError(w, http.StatusNotFound, "file not found")
		return
	}
	writeHumanFileSnapshotError(w, err)
}

// objectSeeker serves one fileRepresentation through io.ReadSeeker to
// http.ServeContent. It prefers the store's RangeReader capability — one
// bounded segment per read window, each open bound to the snapshot's ETag
// so a concurrent writer is caught at the open, not after a mixed read —
// and falls back to a full etag-bound snapshot (readFileSnapshot) when the
// backend cannot open ranges.
type objectSeeker struct {
	h   *humanFilesHandler
	ctx context.Context
	key string
	rep fileRepresentation

	// rr is the store's ranged-open capability (nil: eager fallback).
	rr oss.RangeReader

	// Ranged mode: the open segment [segStart, segEnd) and its reader,
	// whose cursor always sits at (off - segStart).
	off      int64
	segStart int64
	segEnd   int64
	seg      io.ReadCloser

	// Eager mode: the full snapshot bytes and their reader.
	full []byte
	br   *bytes.Reader

	// pending holds the byte(s) Prime read to force the lazy SDK GET
	// before headers; Read hands them out first.
	pending []byte

	// Test-observable accounting: ranged opens performed, bytes handed
	// out through Read.
	opens    int
	bytesOut int64
}

func newObjectSeeker(h *humanFilesHandler, ctx context.Context, key string, rep fileRepresentation) *objectSeeker {
	s := &objectSeeker{h: h, ctx: ctx, key: key, rep: rep}
	s.rr, _ = h.oss.(oss.RangeReader)
	return s
}

// Prime prepares the seeker before the response headers are committed, so
// a storage fault surfaces as a 503 instead of a broken body.
//
// Ranged mode: Prime(start, length) opens exactly [start, start+length) —
// the primed window is a hint about what the client will read first, NOT
// a bound: reads past it continue by opening further segments.
// Prime(0, 0) is a no-op (an empty object has nothing to open).
// Eager mode: Prime loads and etag-binds the full snapshot so the served
// bytes and the served validator are one consistent pair.
func (s *objectSeeker) Prime(start, length int64) error {
	if s.rr == nil {
		return s.loadEager()
	}
	if start <= 0 && length <= 0 {
		return nil
	}
	if err := s.openSegment(start, start+length); err != nil {
		return err
	}
	// sdk.GetObject is lazy: OpenRange has not issued the GET yet. Read
	// one byte here, before ServeContent commits 200, so a 503/412 on the
	// actual GET reaches writeReadError instead of becoming an empty 200.
	buf := make([]byte, 1)
	n, err := s.seg.Read(buf)
	if n > 0 {
		s.pending = append(s.pending, buf[:n]...)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// loadEager loads the full snapshot and checks it against the
// representation's validator: the bytes must describe the same version
// the stat reported, otherwise the served ETag would authorize a write
// onto a base the client never saw (errHumanFileSnapshotUnstable).
func (s *objectSeeker) loadEager() error {
	if s.full != nil {
		return nil
	}
	data, meta, err := s.h.readFileSnapshot(s.ctx, s.key)
	if err != nil {
		return err
	}
	if s.rep.etag != "" && httputil.BareETag(meta.ETag) != s.rep.etag {
		return errHumanFileSnapshotUnstable
	}
	s.full = data
	s.br = bytes.NewReader(data)
	return nil
}

// openSegment opens [start, end) as one ranged read bound to the
// snapshot's validator. Invariant: 0 <= start < end <= rep.size.
func (s *objectSeeker) openSegment(start, end int64) error {
	if s.seg != nil {
		_ = s.seg.Close()
		s.seg = nil
	}
	rc, err := s.rr.OpenRange(s.ctx, s.key, start, end-start, s.rep.etag)
	if err != nil {
		return err
	}
	s.opens++
	s.segStart = start
	s.segEnd = end
	s.seg = rc
	s.off = start
	return nil
}

// Read implements io.Reader over the current segment (ranged mode) or the
// snapshot (eager mode). Reading past the open segment continues at the
// next window — the primed range is never a truncation point.
func (s *objectSeeker) Read(p []byte) (int, error) {
	if s.rr == nil {
		if s.br == nil {
			if err := s.loadEager(); err != nil {
				return 0, err
			}
		}
		n, err := s.br.Read(p)
		s.bytesOut += int64(n)
		return n, err
	}
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		s.off += int64(n)
		s.bytesOut += int64(n)
		return n, nil
	}
	if s.off >= s.rep.size {
		return 0, io.EOF
	}
	if s.seg == nil || s.off < s.segStart || s.off >= s.segEnd {
		end := s.off + humanFileRangeSegment
		if end > s.rep.size {
			end = s.rep.size
		}
		if err := s.openSegment(s.off, end); err != nil {
			return 0, err
		}
	}
	n, err := s.seg.Read(p)
	s.off += int64(n)
	s.bytesOut += int64(n)
	return n, err
}

// Seek implements io.Seeker. In ranged mode it is arithmetic only — no
// I/O: the next Read opens (or reuses) the segment containing the new
// position. ServeContent probes the size with Seek(0, io.SeekEnd) before
// reading anything, and both modes must answer that from the stat alone.
func (s *objectSeeker) Seek(offset int64, whence int) (int64, error) {
	if s.br != nil {
		pos, err := s.br.Seek(offset, whence)
		if err == nil {
			s.off = pos
		}
		return pos, err
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.off + offset
	case io.SeekEnd:
		abs = s.rep.size + offset
	default:
		return 0, errors.New("objectSeeker: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("objectSeeker: negative position")
	}
	s.off = abs
	return abs, nil
}

// Close releases the open segment. It is safe to call more than once
// (http.ServeContent defers it).
func (s *objectSeeker) Close() error {
	if s.seg == nil {
		return nil
	}
	err := s.seg.Close()
	s.seg = nil
	return err
}

// Opens reports how many ranged opens the seeker performed.
func (s *objectSeeker) Opens() int { return s.opens }

// BytesRead reports how many bytes Read handed out.
func (s *objectSeeker) BytesRead() int64 { return s.bytesOut }
