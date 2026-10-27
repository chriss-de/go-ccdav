// Package trace records DAV requests and responses as numbered text files,
// to capture what real clients send and turn it into test fixtures.
// Credentials are blanked; calendar and contact data is not, so traces are
// as private as the data directory.
package trace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	run  string // start time: files of earlier runs aren't overwritten
	next http.Handler
	log  *slog.Logger
	n    atomic.Uint64
}

// Handler wraps next and writes one file per request into dir.
func Handler(dir string, next http.Handler, log *slog.Logger) (http.Handler, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &tracer{dir: dir, run: time.Now().UTC().Format("20060102T150405.000"), next: next, log: log}, nil
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
	if err := t.write(sanitize(user), r.Method, b.Bytes()); err != nil {
		t.log.Warn("trace", "err", err)
	}
}

// write stores one trace in a new file; it never replaces an existing one
// (of an earlier run that started in the same millisecond, say).
func (t *tracer) write(user, method string, data []byte) error {
	for {
		name := fmt.Sprintf("%s-%05d-%s-%s.txt", t.run, t.n.Add(1), user, method)
		f, err := os.OpenFile(filepath.Join(t.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
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
