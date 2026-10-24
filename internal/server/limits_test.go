package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/config"
)

func (c *client) withPW(method, path, pw string, hdr ...string) *http.Response {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, nil)
	req.SetBasicAuth(c.user, pw)
	req.Header.Set("Depth", "0")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestLoginRateLimit(t *testing.T) {
	// The test client connects from 127.0.0.1, which is a trusted proxy
	// here, so X-Forwarded-For decides the client IP.
	srv, _ := newServer(t, func(c *config.Config) { c.TrustedProxies = []string{"127.0.0.1"} })
	alice := &client{t, srv, "alice"}
	xff := func(ip string) []string { return []string{"X-Forwarded-For", ip} }

	for i := range 10 {
		if st := alice.withPW("PROPFIND", "/dav/", "wrong", xff("198.51.100.1")...).StatusCode; st != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, st)
		}
	}
	resp := alice.withPW("PROPFIND", "/dav/", "pw", xff("198.51.100.1")...)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d", resp.StatusCode)
	}
	// Another client IP (spoofing the header only works via a trusted
	// proxy) and requests without credentials are unaffected.
	if st := alice.withPW("PROPFIND", "/dav/", "pw", xff("198.51.100.2")...).StatusCode; st != http.StatusMultiStatus {
		t.Fatalf("other IP: %d", st)
	}
	anon := &client{t, srv, ""}
	anon.expect("PROPFIND", "/dav/", "", http.StatusUnauthorized, "X-Forwarded-For", "198.51.100.1")
}

func TestUntrustedForwardedFor(t *testing.T) {
	srv, _ := newServer(t, nil) // no trusted proxies: header ignored
	alice := &client{t, srv, "alice"}
	for range 10 {
		alice.withPW("PROPFIND", "/dav/", "wrong", "X-Forwarded-For", "203.0.113.5")
	}
	// All failures counted for the real peer, whatever the header says.
	if st := alice.withPW("PROPFIND", "/dav/", "pw", "X-Forwarded-For", "203.0.113.99").StatusCode; st != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For escaped the limiter: %d", st)
	}
}

func TestFreeBusyQuery(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	alice.expect("PUT", cal+"weekly.ics", weekly, http.StatusCreated)
	_, out := alice.expect("PROPFIND", cal, `<d:propfind xmlns:d="DAV:"><d:prop><d:supported-report-set/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<cal:free-busy-query/>")
	resp, out := alice.expect("REPORT", cal, `<c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav"><c:time-range start="20260112T000000Z" end="20260128T000000Z"/></c:free-busy-query>`, http.StatusOK, "Depth", "1")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/calendar") {
		t.Fatal(resp.Header.Get("Content-Type"))
	}
	// 12th 08-09Z, 19th excluded (EXDATE), 26th moved to 27th 13-14Z.
	out = strings.ReplaceAll(out, "\r\n ", "") // unfold
	contains(t, out, "BEGIN:VFREEBUSY", "FREEBUSY;FBTYPE=BUSY:20260112T080000Z/20260112T090000Z,20260127T130000Z/20260127T140000Z")
	notContains(t, out, "20260119", "20260126")
	alice.expect("REPORT", cal, `<c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav"/>`, http.StatusBadRequest)
	alice.expect("REPORT", "/dav/addressbooks/alice/contacts/", `<c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav"><c:time-range start="20260112T000000Z" end="20260128T000000Z"/></c:free-busy-query>`, http.StatusForbidden)
}

func TestRecover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("users: {}\n"), 0o600)
	cs, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	h := Recover(log, cs, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PROPFIND", "/dav/x/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatal(w.Code)
	}
	contains(t, logged.String(), "panic", "PROPFIND", "/dav/x/", "boom", "stack=")
}
