package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/auth"
	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/store"

	"go.yaml.in/yaml/v3"
)

type client struct {
	t    *testing.T
	srv  *httptest.Server
	user string
}

var testHash = sync.OnceValue(func() string {
	h, err := auth.Hash("pw")
	if err != nil {
		panic(err)
	}
	return h
})

// newServer starts a test server; mut may adjust the config (users alice,
// bob and carol, password "pw") before it is written.
func newServer(t *testing.T, mut func(*config.Config)) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{Users: map[string]config.User{
		"alice": {Password: testHash(), Email: "alice@example.com", Name: "Alice"},
		"bob":   {Password: testHash()},
		"carol": {Password: testHash()},
	}}
	if mut != nil {
		mut(&cfg)
	}
	b, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := config.NewStore(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(dir, UIDOf)
	srv := httptest.NewServer(New(cs, st, auth.NewAppPasswords(dir), auth.NewLimiter(), log))
	t.Cleanup(srv.Close)
	return srv, st
}

func newTestServer(t *testing.T) (*client, *client) {
	t.Helper()
	srv, _ := newServer(t, nil)
	return &client{t, srv, "alice"}, &client{t, srv, "bob"}
}

func (c *client) do(method, path, body string, hdr ...string) (*http.Response, string) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	if c.user != "" {
		req.SetBasicAuth(c.user, "pw")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (c *client) expect(method, path, body string, status int, hdr ...string) (*http.Response, string) {
	c.t.Helper()
	resp, out := c.do(method, path, body, hdr...)
	if resp.StatusCode != status {
		c.t.Fatalf("%s %s: status %d, want %d\n%s", method, path, resp.StatusCode, status, out)
	}
	return resp, out
}

func contains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in:\n%s", sub, s)
		}
	}
}

func notContains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("unexpected %q in:\n%s", sub, s)
		}
	}
}

const propfindPrincipal = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`

func TestDiscovery(t *testing.T) {
	alice, _ := newTestServer(t)
	resp, _ := alice.expect("GET", "/.well-known/caldav", "", http.StatusMovedPermanently)
	if resp.Header.Get("Location") != "/dav/" {
		t.Fatal(resp.Header.Get("Location"))
	}
	anon := &client{t, alice.srv, ""}
	anon.expect("PROPFIND", "/dav/", propfindPrincipal, http.StatusUnauthorized)
	resp, _ = anon.expect("OPTIONS", "/dav/", "", http.StatusOK)
	contains(t, resp.Header.Get("DAV"), "calendar-access")

	_, out := alice.expect("PROPFIND", "/dav/", propfindPrincipal, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:current-user-principal><d:href>/dav/principals/alice/</d:href>")

	_, out = alice.expect("PROPFIND", "/dav/principals/alice/", `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop>
		<c:calendar-home-set/><c:calendar-user-address-set/><d:displayname/><d:resourcetype/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<cal:calendar-home-set><d:href>/dav/calendars/alice/</d:href>",
		"mailto:alice@example.com", "<d:displayname>Alice</d:displayname>", "<d:principal/>")

	_, out = alice.expect("PROPFIND", "/dav/calendars/alice/", `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/"><d:prop>
		<d:resourcetype/><d:displayname/><c:supported-calendar-component-set/><cs:getctag/><x:unknown xmlns:x="urn:x"/></d:prop></d:propfind>`,
		http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/calendars/alice/personal/", `<cal:comp name="VEVENT"/>`, `<cal:comp name="VJOURNAL"/>`,
		`<cal:comp name="VTODO"/>`, "<cal:calendar/>", "<cs:getctag>0</cs:getctag>", "404 Not Found")

	// allprop
	_, out = alice.expect("PROPFIND", "/dav/calendars/alice/personal/", "", http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:displayname>Personal</d:displayname>", "calendar-multiget")
}

func TestAccessControl(t *testing.T) {
	alice, bob := newTestServer(t)
	bob.expect("PROPFIND", "/dav/calendars/alice/", "", http.StatusForbidden, "Depth", "0")
	bob.expect("PUT", "/dav/calendars/alice/personal/x.ics", event("x", "20260101T100000Z", ""), http.StatusForbidden)
	bob.expect("PROPFIND", "/dav/principals/alice/", "", http.StatusMultiStatus, "Depth", "0")
	alice.expect("PROPFIND", "/dav/calendars/nobody/", "", http.StatusNotFound, "Depth", "0")
	alice.expect("PROPFIND", "/dav/calendars/alice/../bob/", "", http.StatusNotFound, "Depth", "0")
}

func event(uid, start, extra string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:" + uid +
		"\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:" + start + "\r\nDURATION:PT1H\r\nSUMMARY:ev " + uid + "\r\n" + extra +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
}

func TestPutGetDelete(t *testing.T) {
	alice, _ := newTestServer(t)
	path := "/dav/calendars/alice/personal/one.ics"
	body := event("one", "20260105T090000Z", "")
	resp, _ := alice.expect("PUT", path, body, http.StatusCreated, "Content-Type", "text/calendar", "If-None-Match", "*")
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no etag")
	}
	alice.expect("PUT", path, body, http.StatusPreconditionFailed, "If-None-Match", "*")
	alice.expect("PUT", path, body, http.StatusPreconditionFailed, "If-Match", `"nope"`)
	resp, got := alice.expect("GET", path, "", http.StatusOK)
	if got != body || resp.Header.Get("ETag") != etag {
		t.Fatalf("GET mismatch: %q", got)
	}
	alice.expect("GET", path, "", http.StatusNotModified, "If-None-Match", etag)

	// UID conflict in another resource of the same calendar.
	_, out := alice.expect("PUT", "/dav/calendars/alice/personal/dup.ics", body, http.StatusConflict)
	contains(t, out, "no-uid-conflict", "one.ics")

	// Wrong component for the collection.
	alice.expect("MKCALENDAR", "/dav/calendars/alice/events/", `<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop>
		<c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set></d:prop></d:set></c:mkcalendar>`, http.StatusCreated)
	todo := strings.ReplaceAll(strings.ReplaceAll(body, "VEVENT", "VTODO"), "UID:one", "UID:t1")
	_, out = alice.expect("PUT", "/dav/calendars/alice/events/todo.ics", todo, http.StatusForbidden)
	contains(t, out, "supported-calendar-component")
	alice.expect("PUT", "/dav/calendars/alice/personal/todo.ics", todo, http.StatusCreated)

	// Garbage.
	_, out = alice.expect("PUT", "/dav/calendars/alice/personal/bad.ics", "hello", http.StatusForbidden)
	contains(t, out, "valid-calendar-data")
	alice.expect("PUT", "/dav/calendars/alice/nocal/x.ics", body, http.StatusConflict)

	// ctag moved.
	_, out = alice.expect("PROPFIND", "/dav/calendars/alice/personal/", `<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:getctag/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<cs:getctag>2</cs:getctag>")

	alice.expect("DELETE", path, "", http.StatusPreconditionFailed, "If-Match", `"nope"`)
	alice.expect("DELETE", path, "", http.StatusNoContent, "If-Match", etag)
	alice.expect("GET", path, "", http.StatusNotFound)
}

func TestMkcalendarProppatch(t *testing.T) {
	alice, _ := newTestServer(t)
	alice.expect("MKCALENDAR", "/dav/calendars/alice/work/", `<?xml version="1.0"?>
<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:a="http://apple.com/ns/ical/">
 <d:set><d:prop><d:displayname>Work</d:displayname><a:calendar-color>#FF0000FF</a:calendar-color>
 <c:supported-calendar-component-set><c:comp name="VEVENT"/><c:comp name="VTODO"/></c:supported-calendar-component-set></d:prop></d:set>
</c:mkcalendar>`, http.StatusCreated)
	alice.expect("MKCALENDAR", "/dav/calendars/alice/work/", "", http.StatusMethodNotAllowed)
	alice.expect("MKCOL", "/dav/calendars/alice/ext/", `<d:mkcol xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop>
		<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><d:displayname>Ext</d:displayname></d:prop></d:set></d:mkcol>`, http.StatusCreated)
	alice.expect("MKCOL", "/dav/calendars/alice/plain/", "", http.StatusForbidden)

	_, out := alice.expect("PROPPATCH", "/dav/calendars/alice/work/", `<d:propertyupdate xmlns:d="DAV:" xmlns:a="http://apple.com/ns/ical/" xmlns:x="urn:x">
		<d:set><d:prop><a:calendar-order>3</a:calendar-order><x:foo>bar</x:foo></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	contains(t, out, "200 OK")
	_, out = alice.expect("PROPPATCH", "/dav/calendars/alice/work/", `<d:propertyupdate xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/">
		<d:set><d:prop><d:displayname>New</d:displayname><cs:getctag>9</cs:getctag></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	contains(t, out, "403 Forbidden", "424 Failed Dependency")

	_, out = alice.expect("PROPFIND", "/dav/calendars/alice/work/", "", http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:displayname>Work</d:displayname>", "<ical:calendar-color>#FF0000FF</ical:calendar-color>",
		"<ical:calendar-order>3</ical:calendar-order>", `<x:foo xmlns:x="urn:x">bar</x:foo>`)
	notContains(t, out, `name="VJOURNAL"`)

	alice.expect("DELETE", "/dav/calendars/alice/work/", "", http.StatusNoContent)
	alice.expect("PROPFIND", "/dav/calendars/alice/work/", "", http.StatusNotFound, "Depth", "0")
}

const berlinTZ = "BEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\nBEGIN:STANDARD\r\nDTSTART:19701025T030000\r\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\nBEGIN:DAYLIGHT\r\nDTSTART:19700329T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\n"

// A weekly series in Berlin with one moved and one cancelled instance.
const weekly = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" + berlinTZ +
	"BEGIN:VEVENT\r\nUID:weekly\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;TZID=Europe/Berlin:20260105T090000\r\nDTEND;TZID=Europe/Berlin:20260105T100000\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=13\r\nEXDATE;TZID=Europe/Berlin:20260119T090000\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:weekly\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID;TZID=Europe/Berlin:20260126T090000\r\n" +
	"DTSTART;TZID=Europe/Berlin:20260127T140000\r\nDTEND;TZID=Europe/Berlin:20260127T150000\r\nSUMMARY:Standup moved\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func query(start, end string) string {
	return `<?xml version="1.0"?><c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
<d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">
<c:time-range start="` + start + `" end="` + end + `"/></c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
}

func TestCalendarQueryRecurring(t *testing.T) {
	alice, _ := newTestServer(t)
	alice.expect("PUT", "/dav/calendars/alice/personal/weekly.ics", weekly, http.StatusCreated)
	alice.expect("PUT", "/dav/calendars/alice/personal/single.ics", event("single", "20260301T120000Z", ""), http.StatusCreated)

	cases := []struct {
		start, end string
		weekly     bool
		single     bool
	}{
		{"20260112T000000Z", "20260113T000000Z", true, false},  // 2nd instance
		{"20260119T000000Z", "20260120T000000Z", false, false}, // EXDATE
		{"20260126T000000Z", "20260127T000000Z", false, false}, // moved away from here
		{"20260127T120000Z", "20260127T140000Z", true, false},  // moved to 14:00 CET = 13:00Z
		{"20260309T075900Z", "20260309T080001Z", true, false},  // 09:00 CET = 08:00Z
		{"20260330T065900Z", "20260330T070001Z", true, false},  // after DST: 09:00 CEST = 07:00Z
		{"20260406T000000Z", "20260501T000000Z", false, false}, // after COUNT=13 (last is 2026-03-30)
		{"20260301T000000Z", "20260302T000000Z", false, true},
		{"20250101T000000Z", "20270101T000000Z", true, true},
	}
	for _, c := range cases {
		_, out := alice.expect("REPORT", "/dav/calendars/alice/personal/", query(c.start, c.end), http.StatusMultiStatus, "Depth", "1")
		if strings.Contains(out, "weekly.ics") != c.weekly || strings.Contains(out, "single.ics") != c.single {
			t.Errorf("range %s-%s: weekly=%v single=%v, want %v %v\n%s", c.start, c.end,
				strings.Contains(out, "weekly.ics"), strings.Contains(out, "single.ics"), c.weekly, c.single, out)
		}
	}
}

func TestMultigetAndExpand(t *testing.T) {
	alice, _ := newTestServer(t)
	alice.expect("PUT", "/dav/calendars/alice/personal/weekly.ics", weekly, http.StatusCreated)
	_, out := alice.expect("REPORT", "/dav/calendars/alice/personal/", `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
<d:prop><d:getetag/><c:calendar-data/></d:prop>
<d:href>/dav/calendars/alice/personal/weekly.ics</d:href><d:href>/dav/calendars/alice/personal/missing.ics</d:href>
</c:calendar-multiget>`, http.StatusMultiStatus)
	contains(t, out, "RRULE:FREQ=WEEKLY;COUNT=13", "missing.ics</d:href><d:status>HTTP/1.1 404 Not Found")

	_, out = alice.expect("REPORT", "/dav/calendars/alice/personal/", `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
<d:prop><c:calendar-data><c:expand start="20260112T000000Z" end="20260202T000000Z"/></c:calendar-data></d:prop>
<c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="20260112T000000Z" end="20260202T000000Z"/></c:comp-filter></c:comp-filter></c:filter>
</c:calendar-query>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out,
		"RECURRENCE-ID:20260112T080000Z", "DTSTART:20260112T080000Z",
		"RECURRENCE-ID:20260126T080000Z", "DTSTART:20260127T130000Z", "Standup moved")
	// 01-19 is excluded; 02-02 08:00Z starts after the (exclusive) range end.
	notContains(t, out, "RRULE", "VTIMEZONE", "20260119T080000Z", "20260202T080000Z", "TZID")
}

func TestQueryTextMatchAndTodo(t *testing.T) {
	alice, _ := newTestServer(t)
	todo := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\nBEGIN:VTODO\r\nUID:t1\r\nDTSTAMP:20260101T000000Z\r\nDUE:20260210T120000Z\r\nSUMMARY:Buy Milk\r\nSTATUS:NEEDS-ACTION\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	alice.expect("PUT", "/dav/calendars/alice/personal/t1.ics", todo, http.StatusCreated)
	q := func(inner string) string {
		return `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop>
<c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VTODO">` + inner + `</c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
	}
	for _, c := range []struct {
		filter string
		match  bool
	}{
		{``, true},
		{`<c:prop-filter name="SUMMARY"><c:text-match>milk</c:text-match></c:prop-filter>`, true},
		{`<c:prop-filter name="SUMMARY"><c:text-match collation="i;octet">milk</c:text-match></c:prop-filter>`, false},
		{`<c:prop-filter name="STATUS"><c:text-match negate-condition="yes">COMPLETED</c:text-match></c:prop-filter>`, true},
		{`<c:prop-filter name="COMPLETED"><c:is-not-defined/></c:prop-filter>`, true},
		{`<c:time-range start="20260201T000000Z" end="20260301T000000Z"/>`, true},
		{`<c:time-range start="20260301T000000Z" end="20260401T000000Z"/>`, false},
	} {
		_, out := alice.expect("REPORT", "/dav/calendars/alice/personal/", q(c.filter), http.StatusMultiStatus, "Depth", "1")
		if strings.Contains(out, "t1.ics") != c.match {
			t.Errorf("filter %s: match=%v, want %v", c.filter, !c.match, c.match)
		}
	}
}

func syncReport(token string, limit int) string {
	l := ""
	if limit > 0 {
		l = fmt.Sprintf("<d:limit><d:nresults>%d</d:nresults></d:limit>", limit)
	}
	return `<d:sync-collection xmlns:d="DAV:"><d:sync-token>` + token + `</d:sync-token><d:sync-level>1</d:sync-level>` + l +
		`<d:prop><d:getetag/></d:prop></d:sync-collection>`
}

var syncTokenRe = regexp.MustCompile(`<d:sync-token>([^<]*)</d:sync-token>`)

func tokenOf(t *testing.T, out string) string {
	t.Helper()
	m := syncTokenRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no sync-token in\n%s", out)
	}
	return m[1]
}

func TestSyncCollection(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	_, out := alice.expect("PROPFIND", cal, `<d:propfind xmlns:d="DAV:"><d:prop><d:sync-token/><d:supported-report-set/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:sync-collection/>", "<d:sync-token>http://ccdav/ns/sync/")
	_, out = alice.expect("PROPFIND", cal, "", http.StatusMultiStatus, "Depth", "0")
	notContains(t, out, "<d:sync-token>") // not in allprop

	for _, n := range []string{"a", "b", "c"} {
		alice.expect("PUT", cal+n+".ics", event(n, "20260105T090000Z", ""), http.StatusCreated)
	}
	_, out = alice.expect("REPORT", cal, syncReport("", 0), http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "a.ics", "b.ics", "c.ics", "<d:getetag>")
	tok1 := tokenOf(t, out)

	// No changes.
	_, out = alice.expect("REPORT", cal, syncReport(tok1, 0), http.StatusMultiStatus)
	notContains(t, out, ".ics")
	if tokenOf(t, out) != tok1 {
		t.Fatal("token moved without changes")
	}

	alice.expect("PUT", cal+"a.ics", event("a", "20260106T090000Z", ""), http.StatusNoContent)
	alice.expect("DELETE", cal+"b.ics", "", http.StatusNoContent)
	alice.expect("PUT", cal+"d.ics", event("d", "20260105T090000Z", ""), http.StatusCreated)
	_, out = alice.expect("REPORT", cal, syncReport(tok1, 0), http.StatusMultiStatus)
	contains(t, out, "a.ics</d:href><d:propstat>", "b.ics</d:href><d:status>HTTP/1.1 404 Not Found", "d.ics</d:href><d:propstat>")
	notContains(t, out, "c.ics")
	tok2 := tokenOf(t, out)

	// Property change: new token, no object changes.
	alice.expect("PROPPATCH", cal, `<d:propertyupdate xmlns:d="DAV:" xmlns:a="http://apple.com/ns/ical/"><d:set><d:prop><a:calendar-color>#123456</a:calendar-color></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	_, out = alice.expect("REPORT", cal, syncReport(tok2, 0), http.StatusMultiStatus)
	notContains(t, out, ".ics")
	tok3 := tokenOf(t, out)
	if tok3 == tok2 {
		t.Fatal("PROPPATCH did not bump the token")
	}

	// limit: truncated with 507 and a resumable token.
	for _, n := range []string{"e", "f", "g"} {
		alice.expect("PUT", cal+n+".ics", event(n, "20260105T090000Z", ""), http.StatusCreated)
	}
	_, out = alice.expect("REPORT", cal, syncReport(tok3, 2), http.StatusMultiStatus)
	contains(t, out, "e.ics", "f.ics", "507 Insufficient Storage", "number-of-matches-within-limits")
	notContains(t, out, "g.ics")
	_, out = alice.expect("REPORT", cal, syncReport(tokenOf(t, out), 2), http.StatusMultiStatus)
	contains(t, out, "g.ics")
	notContains(t, out, "e.ics", "507")

	// Tokens from before the rename still sync incrementally.
	_, out = alice.expect("REPORT", cal, syncReport(strings.Replace(tok3, "http://ccdav/", "http://davd/", 1), 0), http.StatusMultiStatus)
	contains(t, out, "e.ics", "<d:sync-token>http://ccdav/ns/sync/")

	// Invalid tokens.
	for _, bad := range []string{"garbage", "http://ccdav/ns/sync/otherid-1", tok3 + "999" /* future counter */} {
		_, out = alice.expect("REPORT", cal, syncReport(bad, 0), http.StatusForbidden)
		contains(t, out, "valid-sync-token")
	}
	// Token of a deleted and recreated calendar.
	alice.expect("DELETE", cal, "", http.StatusNoContent)
	alice.expect("MKCALENDAR", cal, "", http.StatusCreated)
	alice.expect("REPORT", cal, syncReport(tok3, 0), http.StatusForbidden)

	// Not on the home.
	alice.expect("REPORT", "/dav/calendars/alice/", syncReport("", 0), http.StatusForbidden)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCardDAVDiscovery(t *testing.T) {
	alice, _ := newTestServer(t)
	resp, _ := alice.expect("GET", "/.well-known/carddav", "", http.StatusMovedPermanently)
	if resp.Header.Get("Location") != "/dav/" {
		t.Fatal(resp.Header.Get("Location"))
	}
	resp, _ = alice.expect("OPTIONS", "/dav/", "", http.StatusOK)
	contains(t, resp.Header.Get("DAV"), "addressbook")

	_, out := alice.expect("PROPFIND", "/dav/principals/alice/", `<d:propfind xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><card:addressbook-home-set/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<card:addressbook-home-set><d:href>/dav/addressbooks/alice/</d:href>")

	_, out = alice.expect("PROPFIND", "/dav/addressbooks/alice/", `<d:propfind xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav" xmlns:cal="urn:ietf:params:xml:ns:caldav"><d:prop>
		<d:resourcetype/><d:displayname/><card:supported-address-data/><card:max-resource-size/><d:supported-report-set/><cal:supported-calendar-component-set/></d:prop></d:propfind>`,
		http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/addressbooks/alice/contacts/", "<card:addressbook/>", "<d:displayname>Contacts</d:displayname>",
		`version="3.0"`, `version="4.0"`, "<card:addressbook-query/>", "<card:addressbook-multiget/>", "<d:sync-collection/>")
	notContains(t, out, "<cal:calendar/>", "<cal:comp")

	// The root lists both homes.
	_, out = alice.expect("PROPFIND", "/dav/", "", http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/calendars/", "/dav/addressbooks/")
}

func TestCardDAVObjects(t *testing.T) {
	alice, bob := newTestServer(t)
	book := "/dav/addressbooks/alice/contacts/"
	for _, f := range []string{"apple-contact.vcf", "apple-group.vcf", "davx5-contact.vcf", "thunderbird-contact.vcf"} {
		resp, _ := alice.expect("PUT", book+f, fixture(t, f), http.StatusCreated, "Content-Type", "text/vcard; charset=utf-8", "If-None-Match", "*")
		if resp.Header.Get("ETag") == "" {
			t.Fatal("no etag")
		}
		resp, got := alice.expect("GET", book+f, "", http.StatusOK)
		if got != fixture(t, f) || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/vcard") {
			t.Fatalf("%s: round trip mismatch (%s)", f, resp.Header.Get("Content-Type"))
		}
	}

	// Large inline photo (~400 KB).
	photo := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff, 0xd8, 0x42}, 100_000))
	big := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:photo-1\r\nFN:With Photo\r\nPHOTO;ENCODING=b;TYPE=JPEG:" + photo + "\r\nEND:VCARD\r\n"
	alice.expect("PUT", book+"photo.vcf", big, http.StatusCreated)
	if _, got := alice.expect("GET", book+"photo.vcf", "", http.StatusOK); got != big {
		t.Fatal("photo round trip")
	}

	// Rejections.
	apple := fixture(t, "apple-contact.vcf")
	_, out := alice.expect("PUT", book+"dup.vcf", apple, http.StatusConflict)
	contains(t, out, "no-uid-conflict", "apple-contact.vcf", "urn:ietf:params:xml:ns:carddav")
	_, out = alice.expect("PUT", book+"v21.vcf", strings.Replace(strings.Replace(apple, "VERSION:3.0", "VERSION:2.1", 1), "F1A2", "X1A2", 1), http.StatusForbidden)
	contains(t, out, "supported-address-data")
	_, out = alice.expect("PUT", book+"nouid.vcf", "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:x\r\nEND:VCARD\r\n", http.StatusForbidden)
	contains(t, out, "valid-address-data")
	alice.expect("PUT", book+"x.vcf", apple, http.StatusUnsupportedMediaType, "Content-Type", "text/calendar")
	alice.expect("PUT", "/dav/calendars/alice/personal/x.ics", apple, http.StatusForbidden) // vCard into a calendar
	alice.expect("PUT", book+"x.vcf", event("x", "20260101T100000Z", ""), http.StatusForbidden)
	bob.expect("GET", book+"apple-contact.vcf", "", http.StatusForbidden)

	// multiget: only address objects; calendar hrefs are not served here.
	alice.expect("PUT", "/dav/calendars/alice/personal/ev.ics", event("ev", "20260101T100000Z", ""), http.StatusCreated)
	_, out = alice.expect("REPORT", book, `<card:addressbook-multiget xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
<d:prop><d:getetag/><card:address-data/></d:prop>
<d:href>`+book+`davx5-contact.vcf</d:href><d:href>/dav/calendars/alice/personal/ev.ics</d:href></card:addressbook-multiget>`, http.StatusMultiStatus)
	contains(t, out, "FN:Max Mustermann", "ev.ics</d:href><d:status>HTTP/1.1 404")

	// Collection management: extended MKCOL, PROPPATCH, DELETE.
	alice.expect("MKCOL", "/dav/addressbooks/alice/work/", `<d:mkcol xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:set><d:prop>
		<d:resourcetype><d:collection/><card:addressbook/></d:resourcetype><d:displayname>Work</d:displayname>
		<card:addressbook-description>Colleagues</card:addressbook-description></d:prop></d:set></d:mkcol>`, http.StatusCreated)
	alice.expect("MKCOL", "/dav/addressbooks/alice/cal/", `<d:mkcol xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop>
		<d:resourcetype><d:collection/><c:calendar/></d:resourcetype></d:prop></d:set></d:mkcol>`, http.StatusForbidden)
	alice.expect("MKCALENDAR", "/dav/addressbooks/alice/cal/", "", http.StatusForbidden)
	_, out = alice.expect("PROPPATCH", "/dav/addressbooks/alice/work/", `<d:propertyupdate xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
		<d:set><d:prop><d:displayname>Job</d:displayname><c:calendar-timezone>x</c:calendar-timezone></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	contains(t, out, "403 Forbidden", "424 Failed Dependency")
	_, out = alice.expect("PROPFIND", "/dav/addressbooks/alice/work/", "", http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:displayname>Work</d:displayname>", "<card:addressbook-description>Colleagues</card:addressbook-description>")
	alice.expect("DELETE", "/dav/addressbooks/alice/work/", "", http.StatusNoContent)

	// sync-collection works on address books too.
	_, out = alice.expect("REPORT", book, syncReport("", 0), http.StatusMultiStatus)
	contains(t, out, "apple-group.vcf", "photo.vcf")
	tok := tokenOf(t, out)
	alice.expect("DELETE", book+"apple-group.vcf", "", http.StatusNoContent)
	_, out = alice.expect("REPORT", book, syncReport(tok, 0), http.StatusMultiStatus)
	contains(t, out, "apple-group.vcf</d:href><d:status>HTTP/1.1 404")
}

func TestAddressbookQuery(t *testing.T) {
	alice, _ := newTestServer(t)
	book := "/dav/addressbooks/alice/contacts/"
	for _, f := range []string{"apple-contact.vcf", "apple-group.vcf", "davx5-contact.vcf", "thunderbird-contact.vcf"} {
		alice.expect("PUT", book+f, fixture(t, f), http.StatusCreated)
	}
	q := func(filter, extra string) string {
		return `<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><d:getetag/>` + extra + `</d:prop>` + filter + `</card:addressbook-query>`
	}
	for _, c := range []struct {
		filter string
		want   []string
	}{
		{`<card:filter/>`, []string{"apple-contact", "apple-group", "davx5-contact", "thunderbird-contact"}},
		{`<card:filter><card:prop-filter name="EMAIL"><card:text-match match-type="ends-with">example.org</card:text-match></card:prop-filter></card:filter>`, []string{"davx5-contact"}},
		{`<card:filter><card:prop-filter name="FN"><card:text-match>muster</card:text-match></card:prop-filter></card:filter>`, []string{"davx5-contact", "thunderbird-contact"}},
		{`<card:filter><card:prop-filter name="X-ADDRESSBOOKSERVER-KIND"><card:text-match match-type="equals">group</card:text-match></card:prop-filter></card:filter>`, []string{"apple-group"}},
		{`<card:filter test="allof"><card:prop-filter name="FN"><card:text-match>muster</card:text-match></card:prop-filter><card:prop-filter name="CATEGORIES"><card:text-match>sport</card:text-match></card:prop-filter></card:filter>`, []string{"davx5-contact"}},
		{`<card:filter><card:prop-filter name="TEL"><card:param-filter name="TYPE"><card:text-match match-type="equals">cell</card:text-match></card:param-filter></card:prop-filter></card:filter>`, []string{"apple-contact"}},
	} {
		_, out := alice.expect("REPORT", book, q(c.filter, ""), http.StatusMultiStatus, "Depth", "1")
		if got := strings.Count(out, "<d:response>"); got != len(c.want) {
			t.Errorf("%s: %d results, want %v\n%s", c.filter, got, c.want, out)
		}
		contains(t, out, c.want...)
	}

	// Partial address-data.
	_, out := alice.expect("REPORT", book, q(`<card:filter><card:prop-filter name="UID"><card:text-match match-type="starts-with">3b8e</card:text-match></card:prop-filter></card:filter>`,
		`<card:address-data><card:prop name="FN"/></card:address-data>`), http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "FN:Max Mustermann", "UID:3b8e1c1e")
	notContains(t, out, "EMAIL", "CATEGORIES")

	// limit → 507 marker.
	_, out = alice.expect("REPORT", book, q(`<card:filter/><card:limit><card:nresults>2</card:nresults></card:limit>`, ""), http.StatusMultiStatus, "Depth", "1")
	if strings.Count(out, ".vcf</d:href>") != 2 {
		t.Errorf("limit not applied:\n%s", out)
	}
	contains(t, out, "507 Insufficient Storage")

	// Bad filter, and a calendar-query on an address book.
	_, out = alice.expect("REPORT", book, q(`<card:filter><card:prop-filter name="FN"><card:text-match match-type="regex">x</card:text-match></card:prop-filter></card:filter>`, ""), http.StatusForbidden, "Depth", "1")
	contains(t, out, "valid-filter")
	alice.expect("REPORT", book, query("20260101T000000Z", "20270101T000000Z"), http.StatusForbidden, "Depth", "1")
}
