package server

// Review tests for the DAV layer: each test demonstrates one protocol or
// interop issue (or pins behaviour that was checked and found correct).
// Tests named Test* fail on the current code.

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// msResp is a parsed DAV:response.
type msResp struct {
	Href     string `xml:"DAV: href"`
	Status   string `xml:"DAV: status"`
	Propstat []struct {
		Status string `xml:"DAV: status"`
		Prop   struct {
			Inner string `xml:",innerxml"`
		} `xml:"DAV: prop"`
	} `xml:"DAV: propstat"`
}

type msBody struct {
	XMLName   xml.Name `xml:"DAV: multistatus"`
	Responses []msResp `xml:"DAV: response"`
	SyncToken string   `xml:"DAV: sync-token"`
}

// parseMS checks that out is a well-formed DAV:multistatus and parses it.
func parseMS(t *testing.T, out string) msBody {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(out))
	for {
		_, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("malformed XML: %v\n%s", err, out)
		}
	}
	var ms msBody
	if err := xml.Unmarshal([]byte(out), &ms); err != nil {
		t.Fatalf("not a multistatus: %v\n%s", err, out)
	}
	return ms
}

// tryDo is client.do without t.Fatal on transport errors, so a handler
// panic (connection dropped) is reported instead of aborting.
func (c *client) tryDo(method, path, body string, hdr ...string) (int, string, error) {
	req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	req.SetBasicAuth(c.user, "pw")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// --- hrefs ---------------------------------------------------------------

// Resource names with characters that need (or may need) percent-encoding
// round-trip through PUT, PROPFIND, multiget, sync-collection and DELETE.
func TestHrefRoundTrip(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	names := []string{"a b.ics", "a+b.ics", "50%.ics", "x@y.ics", "a;b.ics", "ü ñ.ics", "q?x.ics", "h#x.ics", "a,b.ics", "a&b.ics", "a:b.ics", "a'b\".ics", "<x>.ics"}
	for i, n := range names {
		alice.expect("PUT", cal+url.PathEscape(n), event("u"+string(rune('a'+i)), "20260105T090000Z", ""), http.StatusCreated)
	}
	_, out := alice.expect("PROPFIND", cal, `<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "1")
	ms := parseMS(t, out)
	got := map[string]string{} // decoded name -> href as sent
	for _, r := range ms.Responses {
		p, err := url.PathUnescape(r.Href)
		if err != nil {
			t.Fatalf("href %q: %v", r.Href, err)
		}
		if name, ok := strings.CutPrefix(p, cal); ok && name != "" {
			got[name] = r.Href
		}
	}
	var hrefs strings.Builder
	for _, n := range names {
		h, ok := got[n]
		if !ok {
			t.Fatalf("%q missing from PROPFIND: %v", n, got)
		}
		hrefs.WriteString("<d:href>" + dav.Esc(h) + "</d:href>")
	}
	_, out = alice.expect("REPORT", cal, `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop>`+hrefs.String()+`</c:calendar-multiget>`, http.StatusMultiStatus)
	for _, r := range parseMS(t, out).Responses {
		if r.Status != "" || len(r.Propstat) == 0 {
			t.Errorf("multiget %s: %s", r.Href, r.Status)
		}
		if p, _ := url.PathUnescape(r.Href); !strings.HasPrefix(p, cal) || got[strings.TrimPrefix(p, cal)] != r.Href {
			t.Errorf("multiget echoed %q, PROPFIND said %q", r.Href, got[strings.TrimPrefix(p, cal)])
		}
	}
	_, out = alice.expect("REPORT", cal, syncReport("", 0), http.StatusMultiStatus)
	for _, r := range parseMS(t, out).Responses {
		p, _ := url.PathUnescape(r.Href)
		if got[strings.TrimPrefix(p, cal)] != r.Href {
			t.Errorf("sync href %q differs from PROPFIND", r.Href)
		}
	}
	for _, n := range names {
		alice.expect("DELETE", got[n], "", http.StatusNoContent)
	}
}

// Absolute-URI hrefs in multiget (RFC 4918 allows them) work.
func TestMultigetAbsoluteURI(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	alice.expect("PUT", cal+"a%20b.ics", event("ab", "20260105T090000Z", ""), http.StatusCreated)
	_, out := alice.expect("REPORT", cal, `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop><d:href>`+alice.srv.URL+cal+`a%20b.ics</d:href></c:calendar-multiget>`, http.StatusMultiStatus)
	ms := parseMS(t, out)
	if len(ms.Responses) != 1 || len(ms.Responses[0].Propstat) == 0 {
		t.Fatalf("absolute href not served:\n%s", out)
	}
}

// --- sync-collection -----------------------------------------------------

// RFC 6578 §6.1 makes sync-level mandatory, but clients built on the
// pre-RFC drafts (and some current libraries) omit it. A missing element
// must not crash the handler: today it dereferences nil and the
// connection is dropped (500 behind Recover).
func TestSyncWithoutSyncLevel(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	alice.expect("PUT", cal+"a.ics", event("a", "20260105T090000Z", ""), http.StatusCreated)
	for name, body := range map[string]string{
		"no sync-level":      `<d:sync-collection xmlns:d="DAV:"><d:sync-token/><d:prop><d:getetag/></d:prop></d:sync-collection>`,
		"no sync-token":      `<d:sync-collection xmlns:d="DAV:"><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`,
		"limit w/o nresults": `<d:sync-collection xmlns:d="DAV:"><d:sync-token/><d:sync-level>1</d:sync-level><d:limit/><d:prop><d:getetag/></d:prop></d:sync-collection>`,
	} {
		st, out, err := alice.tryDo("REPORT", cal, body, "Depth", "0")
		if err != nil || st >= 500 {
			t.Errorf("%s: status %d err %v (want 207, or 400 for a malformed body)\n%s", name, st, err, out)
		}
	}
}

func TestAddressbookQueryLimitWithoutNresults(t *testing.T) {
	alice, _ := newTestServer(t)
	st, out, err := alice.tryDo("REPORT", "/dav/addressbooks/alice/contacts/",
		`<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><d:getetag/></d:prop><card:filter/><card:limit/></card:addressbook-query>`, "Depth", "1")
	if err != nil || st >= 500 {
		t.Errorf("status %d err %v (want 400)\n%s", st, err, out)
	}
}

// --- per-user data (caldav_per_user) ---------------------------------------

// Once carol (read-write sharee) has alarms of her own, her ETag is
// "<file>-u<n>". Clients send that ETag back in If-Match on DELETE
// (Thunderbird, DAVx5 and Apple all do), but DELETE compares it with the
// file's ETag only, so the delete fails with 412 forever.
func TestPerUserDeleteIfMatch(t *testing.T) {
	_, _, carol, _ := perUserServer(t, true)
	path := "/dav/calendars/carol/bob~team/ev.ics"
	_, body := etagOf(t, carol, path)
	carol.expect("PUT", path, strings.Replace(body, "-PT15M", "-PT5M", 1), http.StatusNoContent, "Content-Type", "text/calendar")
	tag, _ := etagOf(t, carol, path)
	if !strings.Contains(tag, "-u") {
		t.Fatalf("expected a per-user ETag, got %s", tag)
	}
	// The same ETag is what PROPFIND / sync report.
	_, out := carol.expect("PROPFIND", path, `<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, strings.Trim(tag, `"`))
	st, out, _ := carol.tryDo("DELETE", path, "", "If-Match", tag)
	if st != http.StatusNoContent {
		t.Errorf("DELETE with the ETag the server handed out: %d, want 204\n%s", st, out)
	}
}

// --- shared collections ----------------------------------------------------

// A sharee's DELETE of an alias returns 204 but the alias keeps working.
// RFC 4918 §9.6.1: after a successful DELETE "a subsequent GET/HEAD/
// PROPFIND request to the target Request-URI MUST return 404".
func TestDeletedAliasStillServed(t *testing.T) {
	srv, st := newServer(t, nil)
	alice, bob := &client{t, srv, "alice"}, &client{t, srv, "bob"}
	alice.expect("PUT", "/dav/calendars/alice/personal/a.ics", event("a", "20260105T090000Z", ""), http.StatusCreated)
	col, _ := st.Collection("alice", store.Calendars, "personal")
	col.SetShares([]store.Share{{User: "bob", Access: store.AccessWrite}})
	alias := "/dav/calendars/bob/alice~personal/"
	bob.expect("DELETE", alias, "", http.StatusNoContent)
	_, out := bob.expect("PROPFIND", "/dav/calendars/bob/", "", http.StatusMultiStatus, "Depth", "1")
	notContains(t, out, "alice~personal") // hidden from the listing: fine
	if s, _, _ := bob.tryDo("PROPFIND", alias, "", "Depth", "0"); s != http.StatusNotFound {
		t.Errorf("PROPFIND on the deleted alias: %d, want 404", s)
	}
	if s, _, _ := bob.tryDo("GET", alias+"a.ics", ""); s != http.StatusNotFound {
		t.Errorf("GET in the deleted alias: %d, want 404", s)
	}
	if s, _, _ := bob.tryDo("PUT", alias+"b.ics", event("b", "20260105T090000Z", "")); s/100 == 2 {
		t.Errorf("PUT into the deleted alias succeeded: %d", s)
	}
}

// --- collection creation ---------------------------------------------------

// RFC 5689 §3: when a property of an extended MKCOL can't be set, the
// request fails (typically 403) with a DAV:mkcol-response body. ccdav
// answers 207 Multi-Status, a 2xx that clients treating any 2xx as
// "created" (dav4jvm/DAVx5) take as success although nothing was created.
// (MKCALENDAR's 207 is correct per RFC 4791 §5.3.1.)
func TestExtendedMkcolFailureIs207(t *testing.T) {
	alice, _ := newTestServer(t)
	st, out, _ := alice.tryDo("MKCOL", "/dav/calendars/alice/new/", `<d:mkcol xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop>
<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><d:displayname>New</d:displayname>
<c:supported-calendar-component-set><c:comp name="VAVAILABILITY"/></c:supported-calendar-component-set></d:prop></d:set></d:mkcol>`)
	alice.expect("PROPFIND", "/dav/calendars/alice/new/", "", http.StatusNotFound, "Depth", "0") // not created: right
	if st/100 == 2 || !strings.Contains(out, "mkcol-response") {
		t.Errorf("failed extended MKCOL: status %d, want 403/424 with DAV:mkcol-response\n%s", st, out)
	}
}

// --- PROPFIND --------------------------------------------------------------

// RFC 4918 §9.1 / §14.8: allprop with DAV:include must also return the
// included properties. sync-token (not in allprop, RFC 6578 §4) is the
// typical candidate.
func TestAllpropInclude(t *testing.T) {
	alice, _ := newTestServer(t)
	_, out := alice.expect("PROPFIND", "/dav/calendars/alice/personal/", `<d:propfind xmlns:d="DAV:"><d:allprop/><d:include><d:sync-token/></d:include></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	parseMS(t, out)
	contains(t, out, "<d:sync-token>http://ccdav/")
}

// Every multistatus a typical client session produces is well-formed,
// including dead properties in odd namespaces, display names with markup
// characters, and unknown properties (404 propstat).
func TestWellFormed(t *testing.T) {
	srv, st := newServer(t, nil)
	alice, bob := &client{t, srv, "alice"}, &client{t, srv, "bob"}
	cal := "/dav/calendars/alice/personal/"
	alice.expect("PROPPATCH", cal, `<d:propertyupdate xmlns:d="DAV:" xmlns:x="urn:x&amp;y" xmlns:a="http://apple.com/ns/ical/"><d:set><d:prop>
<d:displayname>&lt;b&gt;Me &amp; you]]&gt;</d:displayname><x:foo xmlns:z="urn:z" z:attr="1&quot;"><z:bar>t&lt;</z:bar></x:foo>
<a:calendar-color symbolic-color="custom">#FF0000FF</a:calendar-color><nons xmlns="">v</nons></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	alice.expect("PUT", cal+"e.ics", event("e", "20260105T090000Z", "DESCRIPTION:a <b> & ]]> c\r\n"), http.StatusCreated)
	col, _ := st.Collection("alice", store.Calendars, "personal")
	col.SetShares([]store.Share{{User: "bob", Access: store.AccessRead}})
	for _, req := range []struct {
		c          *client
		method, p  string
		body, dpth string
	}{
		{alice, "PROPFIND", cal, "", "1"},
		{alice, "PROPFIND", cal, `<d:propfind xmlns:d="DAV:"><d:propname/></d:propfind>`, "1"},
		{alice, "PROPFIND", cal, `<d:propfind xmlns:d="DAV:" xmlns:x="urn:x&amp;y" xmlns:q="urn:q"><d:prop><x:foo/><q:unknown/><nons xmlns=""/></d:prop></d:propfind>`, "0"},
		{bob, "PROPFIND", "/dav/calendars/bob/", "", "1"},
		{bob, "PROPFIND", "/dav/calendars/bob/alice~personal/", `<d:propfind xmlns:d="DAV:" xmlns:x="urn:x&amp;y"><d:prop><x:foo/><d:displayname/></d:prop></d:propfind>`, "0"},
		{alice, "PROPFIND", "/dav/principals/alice/", "", "0"},
		{alice, "PROPFIND", "/dav/", "", "1"},
		{alice, "REPORT", cal, `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop><c:filter><c:comp-filter name="VCALENDAR"/></c:filter></c:calendar-query>`, "1"},
	} {
		_, out := req.c.expect(req.method, req.p, req.body, http.StatusMultiStatus, "Depth", req.dpth)
		parseMS(t, out)
	}
	// The calendar data inside the XML is the stored object, unchanged.
	_, out := alice.expect("REPORT", cal, `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-data/></d:prop><d:href>`+cal+`e.ics</d:href></c:calendar-multiget>`, http.StatusMultiStatus)
	var v struct {
		Data string `xml:"response>propstat>prop>calendar-data"`
	}
	if err := xml.Unmarshal([]byte(out), &v); err != nil || v.Data != event("e", "20260105T090000Z", "DESCRIPTION:a <b> & ]]> c\r\n") {
		t.Errorf("calendar-data not byte-exact (%v): %q", err, v.Data)
	}
}

// --- conditional requests --------------------------------------------------

func TestConditionals(t *testing.T) {
	alice, _ := newTestServer(t)
	p := "/dav/calendars/alice/personal/c.ics"
	// If-Match on a missing resource: 412, not a create.
	alice.expect("PUT", p, event("c", "20260105T090000Z", ""), http.StatusPreconditionFailed, "If-Match", `"x"`)
	resp, _ := alice.expect("PUT", p, event("c", "20260105T090000Z", ""), http.StatusCreated, "If-None-Match", "*")
	tag := resp.Header.Get("ETag")
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
		t.Fatalf("ETag not quoted: %s", tag)
	}
	alice.expect("GET", p, "", http.StatusNotModified, "If-None-Match", "W/"+tag)
	alice.expect("GET", p, "", http.StatusNotModified, "If-None-Match", `"zz", `+tag)
	resp, _ = alice.expect("PUT", p, event("c", "20260106T090000Z", ""), http.StatusNoContent, "If-Match", tag)
	if resp.Header.Get("ETag") == tag || resp.Header.Get("ETag") == "" {
		t.Fatal("ETag did not change")
	}
	alice.expect("PUT", p, event("c", "20260107T090000Z", ""), http.StatusPreconditionFailed, "If-Match", tag)
	alice.expect("DELETE", p, "", http.StatusPreconditionFailed, "If-Match", tag)
	alice.expect("HEAD", p, "", http.StatusOK)
}

// --- calendar-query semantics ---------------------------------------------

func TestCalendarQueryComponents(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	put := func(name, body string) { alice.expect("PUT", cal+name, body, http.StatusCreated) }
	wrap := func(s string) string {
		return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\n" + s + "END:VCALENDAR\r\n"
	}
	put("alarm.ics", event("alarm", "20260310T120000Z", "BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:x\r\nTRIGGER:-PT1H\r\nEND:VALARM\r\n"))
	put("journal.ics", wrap("BEGIN:VJOURNAL\r\nUID:j1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;VALUE=DATE:20260315\r\nSUMMARY:diary\r\nEND:VJOURNAL\r\n"))
	put("allday.ics", wrap("BEGIN:VEVENT\r\nUID:ad\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;VALUE=DATE:20260320\r\nDTEND;VALUE=DATE:20260321\r\nSUMMARY:holiday\r\nEND:VEVENT\r\n"))
	put("todo.ics", wrap("BEGIN:VTODO\r\nUID:td\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260401T100000Z\r\nDUE:20260405T100000Z\r\nSUMMARY:task\r\nEND:VTODO\r\n"))
	put("attendee.ics", event("att", "20260501T100000Z", "ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=Bob:mailto:bob@example.com\r\n"))
	q := func(inner string) string {
		return `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR">` + inner + `</c:comp-filter></c:filter></c:calendar-query>`
	}
	for _, c := range []struct {
		filter string
		want   []string
	}{
		// Alarm fires at 11:00Z.
		{`<c:comp-filter name="VEVENT"><c:comp-filter name="VALARM"><c:time-range start="20260310T105900Z" end="20260310T110100Z"/></c:comp-filter></c:comp-filter>`, []string{"alarm.ics"}},
		{`<c:comp-filter name="VEVENT"><c:comp-filter name="VALARM"><c:time-range start="20260310T115900Z" end="20260310T120100Z"/></c:comp-filter></c:comp-filter>`, nil},
		{`<c:comp-filter name="VJOURNAL"><c:time-range start="20260315T000000Z" end="20260316T000000Z"/></c:comp-filter>`, []string{"journal.ics"}},
		// All-day in UTC (no calendar-timezone).
		{`<c:comp-filter name="VEVENT"><c:time-range start="20260320T230000Z" end="20260321T000000Z"/></c:comp-filter>`, []string{"allday.ics"}},
		{`<c:comp-filter name="VEVENT"><c:time-range start="20260321T000000Z" end="20260322T000000Z"/></c:comp-filter>`, nil},
		{`<c:comp-filter name="VTODO"><c:time-range start="20260403T000000Z" end="20260404T000000Z"/></c:comp-filter>`, []string{"todo.ics"}},
		{`<c:comp-filter name="VTODO"><c:time-range start="20260406T000000Z" end="20260407T000000Z"/></c:comp-filter>`, nil},
		{`<c:comp-filter name="VEVENT"><c:prop-filter name="ATTENDEE"><c:param-filter name="PARTSTAT"><c:text-match>needs-action</c:text-match></c:param-filter></c:prop-filter></c:comp-filter>`, []string{"attendee.ics"}},
		{`<c:comp-filter name="VEVENT"><c:prop-filter name="ATTENDEE"><c:param-filter name="ROLE"><c:is-not-defined/></c:param-filter></c:prop-filter></c:comp-filter>`, []string{"attendee.ics"}},
		{`<c:comp-filter name="VTODO"><c:is-not-defined/></c:comp-filter>`, []string{"alarm.ics", "journal.ics", "allday.ics", "attendee.ics"}},
	} {
		_, out := alice.expect("REPORT", cal, q(c.filter), http.StatusMultiStatus, "Depth", "1")
		ms := parseMS(t, out)
		if len(ms.Responses) != len(c.want) {
			t.Errorf("%s: %d results, want %v\n%s", c.filter, len(ms.Responses), c.want, out)
		}
		contains(t, out, c.want...)
	}
}

// Floating times are evaluated in the calendar's calendar-timezone, or in
// the CALDAV:timezone of the query (RFC 4791 §7.3, §9.8).
func TestFloatingTimes(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	tz := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\n" + berlinTZ + "END:VCALENDAR\r\n"
	// 09:00 floating = 08:00Z in Berlin (winter), 09:00Z in UTC.
	alice.expect("PUT", cal+"f.ics", "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\nBEGIN:VEVENT\r\nUID:f\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260105T090000\r\nDURATION:PT30M\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", http.StatusCreated)
	berlin := query("20260105T080000Z", "20260105T081500Z")
	_, out := alice.expect("REPORT", cal, berlin, http.StatusMultiStatus, "Depth", "1")
	notContains(t, out, "f.ics") // UTC without a calendar-timezone
	withTZ := strings.Replace(berlin, "</c:filter>", "</c:filter><c:timezone>"+dav.Esc(tz)+"</c:timezone>", 1)
	_, out = alice.expect("REPORT", cal, withTZ, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "f.ics")
	alice.expect("PROPPATCH", cal, `<d:propertyupdate xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop><c:calendar-timezone>`+dav.Esc(tz)+`</c:calendar-timezone></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	_, out = alice.expect("REPORT", cal, berlin, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "f.ics")
}

// Expand of an all-day weekly series with EXDATE and a moved override:
// DATE values stay DATE, RECURRENCE-IDs identify the original instances.
func TestExpandAllDay(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	alice.expect("PUT", cal+"ad.ics", "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\n"+
		"BEGIN:VEVENT\r\nUID:ad\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;VALUE=DATE:20260105\r\nDTEND;VALUE=DATE:20260106\r\nRRULE:FREQ=WEEKLY;COUNT=4\r\nEXDATE;VALUE=DATE:20260112\r\nSUMMARY:x\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:ad\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID;VALUE=DATE:20260119\r\nDTSTART;VALUE=DATE:20260120\r\nDTEND;VALUE=DATE:20260121\r\nSUMMARY:moved\r\nEND:VEVENT\r\n"+
		"END:VCALENDAR\r\n", http.StatusCreated)
	_, out := alice.expect("REPORT", cal, `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-data><c:expand start="20260101T000000Z" end="20260201T000000Z"/></c:calendar-data></d:prop><d:href>`+cal+`ad.ics</d:href></c:calendar-multiget>`, http.StatusMultiStatus)
	out = dataOf(t, out)
	contains(t, out, "RECURRENCE-ID;VALUE=DATE:20260105", "RECURRENCE-ID;VALUE=DATE:20260119", "DTSTART;VALUE=DATE:20260120", "RECURRENCE-ID;VALUE=DATE:20260126")
	notContains(t, out, "20260112", "RRULE", "EXDATE")
}

// Partial calendar-data and address-data (RFC 4791 §9.6, RFC 6352 §10.4).
func TestPartialData(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	alice.expect("PUT", cal+"p.ics", event("p", "20260105T090000Z", "DESCRIPTION:long text\r\n"), http.StatusCreated)
	_, out := alice.expect("REPORT", cal, `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-data><c:comp name="VCALENDAR"><c:prop name="VERSION"/><c:comp name="VEVENT"><c:prop name="UID"/><c:prop name="SUMMARY"/><c:prop name="DESCRIPTION" novalue="yes"/></c:comp></c:comp></c:calendar-data></d:prop><d:href>`+cal+`p.ics</d:href></c:calendar-multiget>`, http.StatusMultiStatus)
	out = dataOf(t, out)
	contains(t, out, "VERSION:2.0", "UID:p", "SUMMARY:ev p", "DESCRIPTION:\r\n")
	notContains(t, out, "PRODID", "DTSTART", "long text")

	book := "/dav/addressbooks/alice/contacts/"
	alice.expect("PUT", book+"g.vcf", "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:g1\r\nFN:Grouped\r\nN:Grouped;;;;\r\nitem1.EMAIL;type=INTERNET:g@example.com\r\nitem1.X-ABLabel:_$!<Other>!$_\r\nEND:VCARD\r\n", http.StatusCreated)
	_, out = alice.expect("REPORT", book, `<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><card:address-data><card:prop name="EMAIL"/></card:address-data></d:prop><card:filter><card:prop-filter name="EMAIL"><card:param-filter name="TYPE"><card:text-match match-type="equals">internet</card:text-match></card:param-filter></card:prop-filter></card:filter></card:addressbook-query>`, http.StatusMultiStatus, "Depth", "1")
	out = dataOf(t, out)
	contains(t, out, "item1.EMAIL;type=INTERNET:g@example.com", "UID:g1", "VERSION:3.0")
}

// dataOf returns the calendar-data / address-data of all responses.
func dataOf(t *testing.T, out string) string {
	t.Helper()
	parseMS(t, out)
	var v struct {
		Cal  []string `xml:"response>propstat>prop>calendar-data"`
		Card []string `xml:"response>propstat>prop>address-data"`
	}
	if err := xml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	return strings.Join(append(v.Cal, v.Card...), "")
}

// RFC 9110 §15.5.6: a 405 "MUST generate an Allow header field".
func Test405WithoutAllow(t *testing.T) {
	alice, _ := newTestServer(t)
	for _, c := range []struct{ method, path string }{
		{"PUT", "/dav/calendars/alice/personal/"},
		{"PUT", "/dav/calendars/alice/"},
		{"MOVE", "/dav/calendars/alice/personal/x.ics"},
		{"GET", "/dav/calendars/alice/personal/"},
	} {
		resp, _ := alice.do(c.method, c.path, "")
		if resp.StatusCode == http.StatusMethodNotAllowed && resp.Header.Get("Allow") == "" {
			t.Errorf("%s %s: 405 without Allow", c.method, c.path)
		}
	}
}

// Collection names with spaces/unicode, own and as a sharee's alias.
func TestCollectionNameEscaping(t *testing.T) {
	srv, st := newServer(t, nil)
	alice, bob := &client{t, srv, "alice"}, &client{t, srv, "bob"}
	alice.expect("MKCALENDAR", "/dav/calendars/alice/my%20c%C3%A4l/", "", http.StatusCreated)
	_, out := alice.expect("PROPFIND", "/dav/calendars/alice/", `<d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "<d:href>/dav/calendars/alice/my%20c%C3%A4l/</d:href>", "<d:displayname>my cäl</d:displayname>")
	col, _ := st.Collection("alice", store.Calendars, "my cäl")
	col.SetShares([]store.Share{{User: "bob", Access: store.AccessWrite}})
	_, out = bob.expect("PROPFIND", "/dav/calendars/bob/", `<d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "<d:href>/dav/calendars/bob/alice~my%20c%C3%A4l/</d:href>")
	bob.expect("PUT", "/dav/calendars/bob/alice~my%20c%C3%A4l/e.ics", event("e", "20260105T090000Z", ""), http.StatusCreated)
}

// A read-write sharee (caldav_per_user on) changes her alarm and, in the
// same PUT, other properties. Everything but the per-user part must land
// in the owner's object.
func TestPerUserMixedPutKeepsEdits(t *testing.T) {
	for name, edit := range map[string][2]string{
		"x-prop":    {"SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nX-FOO-CUSTOM:bar\r\n"},
		"location":  {"SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nLOCATION:Room 1\r\nX-APPLE-STRUCTURED-LOCATION;VALUE=URI;X-TITLE=Room 1:geo:1,2\r\n"},
		"attach":    {"SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nATTACH:https://example.com/a.pdf\r\n"},
		"comment":   {"SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nCOMMENT:hello\r\n"},
		"rrule":     {"SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nRRULE:FREQ=DAILY;COUNT=3\r\n"},
		"attendee":  {"SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nORGANIZER:mailto:bob@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:carol@example.com\r\n"},
		"desc-fold": {"SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nDESCRIPTION:" + strings.Repeat("long ", 30) + "end\r\n"},
	} {
		t.Run(name, func(t *testing.T) {
			_, bob, carol, _ := perUserServer(t, true)
			path := "/dav/calendars/carol/bob~team/ev.ics"
			_, body := etagOf(t, carol, path)
			up := strings.Replace(strings.Replace(body, edit[0], edit[1], 1), "-PT15M", "-PT5M", 1)
			carol.expect("PUT", path, up, http.StatusNoContent, "Content-Type", "text/calendar")
			_, bobBody := etagOf(t, bob, "/dav/calendars/bob/team/ev.ics")
			added := strings.TrimPrefix(edit[1], edit[0])
			for _, line := range strings.Split(strings.TrimSpace(added), "\r\n") {
				// Compare unfolded, by name and value (the merge may add a
				// JSID parameter and reorder parameters).
				unf := strings.ReplaceAll(bobBody, "\r\n ", "")
				name, _, _ := strings.Cut(line, ";")
				name, _, _ = strings.Cut(name, ":")
				_, value, _ := strings.Cut(line, ":")
				if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `(;[^:\r\n]*)?:` + regexp.QuoteMeta(value) + `\r?$`).MatchString(unf) {
					t.Errorf("owner's object lost %q:\n%s", line, bobBody)
				}
			}
		})
	}
}

// RFC 4791 §7.10: "VFREEBUSY components SHOULD be considered in
// generating the free busy time information." A calendar may hold them
// (MKCALENDAR accepts VFREEBUSY in supported-calendar-component-set), but
// free-busy-query only looks at VEVENTs.
func TestFreeBusyIgnoresVFREEBUSY(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/fb/"
	alice.expect("MKCALENDAR", cal, `<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop><c:supported-calendar-component-set><c:comp name="VEVENT"/><c:comp name="VFREEBUSY"/></c:supported-calendar-component-set></d:prop></d:set></c:mkcalendar>`, http.StatusCreated)
	alice.expect("PUT", cal+"fb.ics", "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\nBEGIN:VFREEBUSY\r\nUID:fb1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260105T000000Z\r\nDTEND:20260106T000000Z\r\nFREEBUSY;FBTYPE=BUSY:20260105T100000Z/20260105T120000Z\r\nEND:VFREEBUSY\r\nEND:VCALENDAR\r\n", http.StatusCreated)
	_, out := alice.expect("REPORT", cal, `<c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav"><c:time-range start="20260105T000000Z" end="20260106T000000Z"/></c:free-busy-query>`, http.StatusOK, "Depth", "1")
	contains(t, out, "20260105T100000Z/20260105T120000Z")
}

// RFC 3986 §2.2: "%2F" in a path segment is data, not a separator. A PUT
// to ".../a%2Fb.ics" is a single resource name; ccdav routes on the
// decoded path and answers 404 for a resource in an existing calendar.
func TestEncodedSlashInName(t *testing.T) {
	alice, _ := newTestServer(t)
	st, out, _ := alice.tryDo("PUT", "/dav/calendars/alice/personal/a%2Fb.ics", event("slash", "20260105T090000Z", ""))
	if st != http.StatusBadRequest { // names are file names: no "/" in them
		t.Errorf("PUT a%%2Fb.ics: %d, want 400 rather than 404\n%s", st, out)
	}
}

// allprop alone (no include) is the common case and must keep working.
func TestAllpropWithoutInclude(t *testing.T) {
	alice, _ := newTestServer(t)
	_, out := alice.expect("PROPFIND", "/dav/calendars/alice/personal/", `<d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	parseMS(t, out)
	contains(t, out, "<d:displayname>")
}
