package server

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// Scenario tests per client: the request sequences and bodies follow what
// each client sends during account setup and sync, including the
// properties we don't support (those must come back as 404 propstats, not
// fail the request).

var etagRe = regexp.MustCompile(`<d:getetag>([^<]+)</d:getetag>`)

func etagIn(t *testing.T, out string) string {
	t.Helper()
	m := etagRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no getetag in\n%s", out)
	}
	return strings.ReplaceAll(m[1], "&#34;", `"`)
}

func TestClientApple(t *testing.T) {
	alice, _ := newTestServer(t)
	// Setup: well-known, then the principal with Apple's property list.
	resp, _ := alice.expect("PROPFIND", "/.well-known/caldav", "", http.StatusMovedPermanently)
	if resp.Header.Get("Location") != "/dav/" {
		t.Fatal(resp.Header.Get("Location"))
	}
	_, out := alice.expect("PROPFIND", "/dav/", `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:"><A:prop><A:current-user-principal/><A:principal-URL/><A:resourcetype/></A:prop></A:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:current-user-principal><d:href>/dav/principals/alice/</d:href>")
	_, out = alice.expect("PROPFIND", "/dav/principals/alice/", `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:" xmlns:B="urn:ietf:params:xml:ns:caldav" xmlns:C="http://calendarserver.org/ns/" xmlns:D="urn:ietf:params:xml:ns:carddav">
<A:prop><B:calendar-home-set/><B:calendar-user-address-set/><A:current-user-principal/><A:displayname/><C:dropbox-home-URL/>
<C:email-address-set/><C:notification-URL/><A:principal-collection-set/><A:principal-URL/><A:resource-id/><B:schedule-inbox-URL/>
<B:schedule-outbox-URL/><A:supported-report-set/><D:addressbook-home-set/></A:prop></A:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<cal:calendar-home-set><d:href>/dav/calendars/alice/</d:href>", "mailto:alice@example.com",
		"<card:addressbook-home-set><d:href>/dav/addressbooks/alice/</d:href>", "<d:displayname>Alice</d:displayname>",
		"404 Not Found") // dropbox, notification, schedule-*: unsupported

	// Home listing with Apple's (long) property list.
	_, out = alice.expect("PROPFIND", "/dav/calendars/alice/", `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:" xmlns:B="http://apple.com/ns/ical/" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="http://calendarserver.org/ns/">
<A:prop><A:add-member/><C:allowed-sharing-modes/><B:autoprovisioned/><D:bulk-requests/><B:calendar-color/><C:calendar-description/>
<C:calendar-free-busy-set/><B:calendar-order/><C:calendar-timezone/><A:current-user-privilege-set/><C:default-alarm-vevent-date/>
<A:displayname/><D:getctag/><B:language-code/><B:location-code/><A:owner/><D:pre-publish-url/><D:publish-url/><D:push-transports/>
<D:pushkey/><A:quota-available-bytes/><A:quota-used-bytes/><B:refreshrate/><A:resource-id/><A:resourcetype/><C:schedule-calendar-transp/>
<C:schedule-default-calendar-URL/><D:source/><D:subscribed-strip-alarms/><C:supported-calendar-component-set/>
<A:supported-report-set/><A:sync-token/></A:prop></A:propfind>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/calendars/alice/personal/", "<cal:calendar/>", "<cs:getctag>", "<d:sync-token>",
		"<ical:calendar-color>", `<cal:comp name="VTODO"/>`, "<d:write/>")

	// A Reminders list: task-only calendar, created with MKCALENDAR.
	alice.expect("MKCALENDAR", "/dav/calendars/alice/A1B2C3/", `<?xml version="1.0" encoding="UTF-8"?>
<B:mkcalendar xmlns:B="urn:ietf:params:xml:ns:caldav"><A:set xmlns:A="DAV:"><A:prop>
<B:supported-calendar-component-set><B:comp name="VTODO"/></B:supported-calendar-component-set>
<A:displayname>Reminders</A:displayname><D:calendar-color xmlns:D="http://apple.com/ns/ical/">#FF2968FF</D:calendar-color>
<D:calendar-order xmlns:D="http://apple.com/ns/ical/">2</D:calendar-order></A:prop></A:set></B:mkcalendar>`, http.StatusCreated)
	todo := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Apple Inc.//iOS 18.0//EN\r\nBEGIN:VTODO\r\nUID:R1\r\nDTSTAMP:20260101T000000Z\r\nSUMMARY:Milk\r\nSTATUS:NEEDS-ACTION\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	alice.expect("PUT", "/dav/calendars/alice/A1B2C3/R1.ics", todo, http.StatusCreated, "If-None-Match", "*", "Content-Type", "text/calendar")
	_, out = alice.expect("PUT", "/dav/calendars/alice/A1B2C3/E1.ics", event("E1", "20260105T090000Z", ""), http.StatusForbidden)
	contains(t, out, "supported-calendar-component")
	// Colors keep Apple's alpha suffix.
	_, out = alice.expect("PROPFIND", "/dav/calendars/alice/A1B2C3/", `<A:propfind xmlns:A="DAV:" xmlns:B="http://apple.com/ns/ical/"><A:prop><B:calendar-color/><B:calendar-order/></A:prop></A:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "#FF2968FF", "<ical:calendar-order>2</ical:calendar-order>")

	// Contacts: Apple's group vCards are stored as they are.
	alice.expect("PUT", "/dav/addressbooks/alice/contacts/g.vcf", fixture(t, "apple-group.vcf"), http.StatusCreated, "If-None-Match", "*")
}

func TestClientThunderbird(t *testing.T) {
	alice, _ := newTestServer(t)
	cal := "/dav/calendars/alice/personal/"
	ctag := func() string {
		_, out := alice.expect("PROPFIND", cal, `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/"><D:prop><CS:getctag/></D:prop></D:propfind>`, http.StatusMultiStatus, "Depth", "0")
		m := regexp.MustCompile(`<cs:getctag>([^<]+)</cs:getctag>`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no ctag in\n%s", out)
		}
		return m[1]
	}
	// Setup check of the calendar itself.
	_, out := alice.expect("PROPFIND", cal, `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop>
<D:resourcetype/><D:owner/><D:current-user-principal/><D:current-user-privilege-set/><D:supported-report-set/>
<C:supported-calendar-component-set/><CS:getctag/></D:prop></D:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<cal:calendar/>", "<cal:calendar-multiget/>", "<cal:calendar-query/>")

	c0 := ctag()
	alice.expect("PUT", cal+"tb-1.ics", event("tb-1", "20260105T090000Z", ""), http.StatusCreated, "If-None-Match", "*")
	alice.expect("PUT", cal+"tb-2.ics", event("tb-2", "20260301T090000Z", ""), http.StatusCreated, "If-None-Match", "*")
	c1 := ctag()
	if c1 == c0 {
		t.Fatal("ctag did not change")
	}
	// ctag changed: list etags by time range, then multiget.
	_, out = alice.expect("REPORT", cal, `<?xml version="1.0" encoding="UTF-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getcontenttype/><D:resourcetype/><D:getetag/></D:prop>
<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT"><C:time-range start="20260101T000000Z" end="20260201T000000Z"/></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`,
		http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "tb-1.ics")
	notContains(t, out, "tb-2.ics")
	_, out = alice.expect("REPORT", cal, `<?xml version="1.0" encoding="UTF-8"?>
<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop>
<D:href>`+cal+`tb-1.ics</D:href><D:href>`+cal+`tb-2.ics</D:href><D:href>`+cal+`gone.ics</D:href></C:calendar-multiget>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "UID:tb-1", "UID:tb-2", "gone.ics", "404")

	// Update with If-Match, then a stale If-Match fails.
	tag := etagIn(t, out)
	alice.expect("PUT", cal+"tb-1.ics", event("tb-1", "20260105T100000Z", ""), http.StatusNoContent, "If-Match", tag)
	alice.expect("PUT", cal+"tb-1.ics", event("tb-1", "20260105T110000Z", ""), http.StatusPreconditionFailed, "If-Match", tag)
	if ctag() == c1 {
		t.Fatal("ctag did not change after update")
	}
}

func TestClientDAVx5(t *testing.T) {
	alice, _ := newTestServer(t)
	// Service discovery: current-user-principal on the base URL.
	_, out := alice.expect("PROPFIND", "/dav/", `<?xml version='1.0' encoding='UTF-8' ?><propfind xmlns="DAV:"><prop><current-user-principal /></prop></propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "/dav/principals/alice/")
	_, out = alice.expect("PROPFIND", "/dav/principals/alice/", `<?xml version='1.0' encoding='UTF-8' ?><propfind xmlns="DAV:" xmlns:CAL="urn:ietf:params:xml:ns:caldav" xmlns:CARD="urn:ietf:params:xml:ns:carddav">
<prop><CAL:calendar-home-set /><CAL:calendar-user-address-set /><CARD:addressbook-home-set /><displayname /><group-membership /></prop></propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "/dav/calendars/alice/", "/dav/addressbooks/alice/")

	// Collection refresh: what DAVx⁵ asks of every collection in the homes.
	refresh := `<?xml version='1.0' encoding='UTF-8' ?><propfind xmlns="DAV:" xmlns:CAL="urn:ietf:params:xml:ns:caldav" xmlns:CARD="urn:ietf:params:xml:ns:carddav" xmlns:CS="http://calendarserver.org/ns/" xmlns:ICAL="http://apple.com/ns/ical/">
<prop><resourcetype /><displayname /><owner /><current-user-privilege-set /><CS:source /><CAL:calendar-description /><ICAL:calendar-color />
<CAL:supported-calendar-component-set /><CARD:addressbook-description /><CARD:supported-address-data /><supported-report-set /><sync-token /><CS:getctag /></prop></propfind>`
	_, out = alice.expect("PROPFIND", "/dav/calendars/alice/", refresh, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/calendars/alice/personal/", "<d:sync-collection/>", `<cal:comp name="VEVENT"/>`)
	_, out = alice.expect("PROPFIND", "/dav/addressbooks/alice/", refresh, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/addressbooks/alice/contacts/", `version="4.0"`)

	// Sync: the initial sync-collection returns everything (all results
	// share one token, so it can't be cut), then incremental.
	cal := "/dav/calendars/alice/personal/"
	for _, n := range []string{"a", "b", "c"} {
		alice.expect("PUT", cal+n+".ics", event(n, "20260105T090000Z", ""), http.StatusCreated, "If-None-Match", "*")
	}
	_, out = alice.expect("REPORT", cal, syncReport("", 2), http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "a.ics", "b.ics", "c.ics")
	notContains(t, out, "number-of-matches-within-limits")
	tok := tokenOf(t, out)
	_, out = alice.expect("REPORT", cal, syncReport(tok, 2), http.StatusMultiStatus, "Depth", "0")
	notContains(t, out, ".ics")
	tok = tokenOf(t, out)
	alice.expect("DELETE", cal+"a.ics", "", http.StatusNoContent)
	_, out = alice.expect("REPORT", cal, syncReport(tok, 0), http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "a.ics", "404")
	notContains(t, out, "b.ics", "c.ics")

	// Contacts with DAVx⁵'s vCard 4 output and If-None-Match.
	book := "/dav/addressbooks/alice/contacts/"
	alice.expect("PUT", book+"d.vcf", fixture(t, "davx5-contact.vcf"), http.StatusCreated, "If-None-Match", "*", "Content-Type", "text/vcard; charset=utf-8")
	alice.expect("PUT", book+"d.vcf", fixture(t, "davx5-contact.vcf"), http.StatusPreconditionFailed, "If-None-Match", "*")
}

func TestClientEvolution(t *testing.T) {
	alice, _ := newTestServer(t)
	// Memos: a VJOURNAL-only calendar.
	alice.expect("MKCOL", "/dav/calendars/alice/memos/", `<?xml version="1.0" encoding="utf-8"?>
<D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:set><D:prop><D:resourcetype><D:collection/><C:calendar/></D:resourcetype>
<D:displayname>Memos</D:displayname><C:supported-calendar-component-set><C:comp name="VJOURNAL"/></C:supported-calendar-component-set></D:prop></D:set></D:mkcol>`, http.StatusCreated)
	j := func(uid, start string) string {
		return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Evolution//EN\r\nBEGIN:VJOURNAL\r\nUID:" + uid + "\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;VALUE=DATE:" + start + "\r\nSUMMARY:memo\r\nEND:VJOURNAL\r\nEND:VCALENDAR\r\n"
	}
	alice.expect("PUT", "/dav/calendars/alice/memos/m1.ics", j("m1", "20260110"), http.StatusCreated)
	alice.expect("PUT", "/dav/calendars/alice/memos/m2.ics", j("m2", "20260310"), http.StatusCreated)
	_, out := alice.expect("REPORT", "/dav/calendars/alice/memos/", `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/></D:prop><C:filter><C:comp-filter name="VCALENDAR">
<C:comp-filter name="VJOURNAL"><C:time-range start="20260101T000000Z" end="20260201T000000Z"/></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "m1.ics")
	notContains(t, out, "m2.ics")
	// Evolution asks for the whole object list with an empty comp-filter.
	_, out = alice.expect("REPORT", "/dav/calendars/alice/memos/", `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/></D:prop>
<C:filter><C:comp-filter name="VCALENDAR"/></C:filter></C:calendar-query>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "m1.ics", "m2.ics")
}
