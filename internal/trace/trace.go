// Package trace records DAV requests and responses as numbered text files,
// to capture what real clients send and turn it into test fixtures.
// Credentials are blanked; calendar and contact data is not, so traces are
// as private as the data directory.
package trace

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// maxBody caps how much of each body is recorded.
const maxBody = 1 << 20

var redacted = []string{"Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization"}

type tracer struct {
	dir  string
	next http.Handler
	log  *slog.Logger
	n    atomic.Uint64
}

// Handler wraps next and writes one file per request into dir.
func Handler(dir string, next http.Handler, log *slog.Logger) (http.Handler, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &tracer{dir: dir, next: next, log: log}, nil
}

func (t *tracer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req capped
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.TeeReader(r.Body, &req), r.Body}
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	t.next.ServeHTTP(rec, r)

	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s %s\n", r.Method, r.URL.RequestURI(), r.Proto)
	fmt.Fprintf(&b, "Host: %s\n", r.Host)
	writeHeader(&b, r.Header)
	fmt.Fprintf(&b, "\n%s\n\n", req.String())
	fmt.Fprintf(&b, "--- %d %s (%s)\n", rec.status, http.StatusText(rec.status), time.Since(start).Round(time.Millisecond))
	writeHeader(&b, rec.Header())
	fmt.Fprintf(&b, "\n%s\n", rec.body.String())

	user, _, _ := r.BasicAuth()
	name := fmt.Sprintf("%05d-%s-%s.txt", t.n.Add(1), sanitize(user), r.Method)
	if err := os.WriteFile(filepath.Join(t.dir, name), b.Bytes(), 0o600); err != nil {
		t.log.Warn("trace", "err", err)
	}
}

func writeHeader(b *bytes.Buffer, h http.Header) {
	for _, k := range slices.Sorted(maps.Keys(h)) {
		for _, v := range h[k] {
			if slices.Contains(redacted, k) {
				v = "(redacted)"
			}
			fmt.Fprintf(b, "%s: %s\n", k, v)
		}
	}
}

func sanitize(s string) string {
	if s == "" {
		return "anon"
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

// capped is a buffer that silently stops growing at maxBody.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxBody - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

type recorder struct {
	http.ResponseWriter
	status int
	body   capped
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	r.body.Write(p)
	return r.ResponseWriter.Write(p)
}
