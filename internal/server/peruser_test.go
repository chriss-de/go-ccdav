package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/peruser"
	"github.com/chriss-de/go-ccdav/internal/store"
)

const tbEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Mozilla.org/NONSGML Mozilla Calendar V1.1//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:tb1\r\nDTSTAMP:20261020T080000Z\r\nDTSTART;TZID=Europe/Berlin:20261103T100000\r\n" +
	"DTEND;TZID=Europe/Berlin:20261103T103000\r\nSUMMARY:Standup\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;VALUE=DURATION:-PT15M\r\nDESCRIPTION:Default Mozilla Description\r\nEND:VALARM\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// perUserServer: bob's calendar "team" with one event, shared read-only
// with alice and read-write with carol.
func perUserServer(t *testing.T, on bool) (alice, bob, carol *client, st *store.Store) {
	t.Helper()
	srv, st := newServer(t, func(c *config.Config) { c.CalDAVPerUser = on })
	alice, bob, carol = &client{t, srv, "alice"}, &client{t, srv, "bob"}, &client{t, srv, "carol"}
	bob.expect("MKCALENDAR", "/dav/calendars/bob/team/", "", http.StatusCreated)
	col, _ := st.Collection("bob", store.Calendars, "team")
	col.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}, {User: "carol", Access: store.AccessWrite}})
	bob.expect("PUT", "/dav/calendars/bob/team/ev.ics", tbEvent, http.StatusCreated, "Content-Type", "text/calendar")
	return alice, bob, carol, st
}

const aliceEv = "/dav/calendars/alice/bob~team/ev.ics"

func etagOf(t *testing.T, c *client, path string) (string, string) {
	t.Helper()
	resp, body := c.expect("GET", path, "", http.StatusOK)
	return resp.Header.Get("ETag"), body
}

func TestPerUserThunderbirdDismiss(t *testing.T) {
	alice, bob, _, _ := perUserServer(t, true)
	bobTag, bobBody := etagOf(t, bob, "/dav/calendars/bob/team/ev.ics")
	_, out := alice.expect("PROPFIND", "/dav/calendars/alice/bob~team/", `<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:getctag/><d:sync-token/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	token := tokenOf(t, out)
	_, bobCol := bob.expect("PROPFIND", "/dav/calendars/bob/team/", `<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:getctag/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")

	// Thunderbird, read-only, dismisses the alarm.
	_, body := etagOf(t, alice, aliceEv)
	dismissed := strings.Replace(body, "SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nX-MOZ-LASTACK:20261103T094600Z\r\n", 1)
	resp, _ := alice.expect("PUT", aliceEv, dismissed, http.StatusNoContent, "Content-Type", "text/calendar")
	if resp.Header.Get("ETag") != "" {
		t.Fatal("ETag on a PUT whose result differs from the upload")
	}
	// Alice sees her acknowledgement, under an ETag of her own.
	aTag, aBody := etagOf(t, alice, aliceEv)
	if !strings.Contains(aBody, "X-MOZ-LASTACK:20261103T094600Z") || !strings.HasSuffix(aTag, `-u1"`) {
		t.Fatalf("alice: %s\n%s", aTag, aBody)
	}
	// Bob's object, ETag and CTag are untouched.
	if tag, body := etagOf(t, bob, "/dav/calendars/bob/team/ev.ics"); tag != bobTag || body != bobBody {
		t.Fatalf("bob's object changed:\n%s", body)
	}
	_, bobCol2 := bob.expect("PROPFIND", "/dav/calendars/bob/team/", `<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:getctag/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	if bobCol2 != bobCol {
		t.Fatal("bob's CTag changed")
	}
	// Reports deliver her view too.
	_, out = alice.expect("REPORT", "/dav/calendars/alice/bob~team/", `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop><d:href>`+aliceEv+`</d:href></c:calendar-multiget>`, http.StatusMultiStatus)
	contains(t, out, "X-MOZ-LASTACK:20261103T094600Z", strings.Trim(aTag, `"`))
	// Her next sync reports the object, with her ETag.
	_, out = alice.expect("REPORT", "/dav/calendars/alice/bob~team/", syncReport(token, 0), http.StatusMultiStatus)
	contains(t, out, "ev.ics", strings.Trim(aTag, `"`))

	// Anything else stays read-only.
	alice.expect("PUT", aliceEv, strings.Replace(dismissed, "SUMMARY:Standup", "SUMMARY:Mine", 1), http.StatusForbidden, "Content-Type", "text/calendar")
	// If-Match: her own ETag works, a stale one doesn't.
	snoozed := strings.Replace(aBody, "X-MOZ-LASTACK:20261103T094600Z", "X-MOZ-LASTACK:20261103T095000Z", 1)
	alice.expect("PUT", aliceEv, snoozed, http.StatusPreconditionFailed, "Content-Type", "text/calendar", "If-Match", bobTag)
	alice.expect("PUT", aliceEv, snoozed, http.StatusNoContent, "Content-Type", "text/calendar", "If-Match", aTag)
	if _, b := etagOf(t, alice, aliceEv); !strings.Contains(b, "X-MOZ-LASTACK:20261103T095000Z") {
		t.Fatalf("second dismiss:\n%s", b)
	}
}

func TestPerUserAppleAlarms(t *testing.T) {
	alice, bob, _, st := perUserServer(t, true)
	_, body := etagOf(t, alice, aliceEv)
	// Apple acknowledges and replaces the alarm with its own.
	mine := strings.Replace(body, "TRIGGER;VALUE=DURATION:-PT15M", "X-WR-ALARMUID:A-1\r\nUID:A-1\r\nTRIGGER:-PT30M\r\nACKNOWLEDGED:20261103T092900Z\r\nX-APPLE-DEFAULT-ALARM:TRUE", 1)
	alice.expect("PUT", aliceEv, mine, http.StatusNoContent, "Content-Type", "text/calendar")
	_, got := etagOf(t, alice, aliceEv)
	contains(t, got, "TRIGGER:-PT30M", "ACKNOWLEDGED:20261103T092900Z", "X-WR-ALARMUID:A-1", "X-APPLE-DEFAULT-ALARM:TRUE")
	notContains(t, got, "-PT15M")
	_, bobBody := etagOf(t, bob, "/dav/calendars/bob/team/ev.ics")
	contains(t, bobBody, "TRIGGER;VALUE=DURATION:-PT15M")
	notContains(t, bobBody, "-PT30M", "A-1")
	// Stored as JMAP keeps per-user data: JMAP shows alice the same alarm.
	o, _ := st.Overlay("alice", "bob")
	for _, ev := range o.Events {
		d := peruser.Decode(ev.Data)
		alerts, _ := d.Props["alerts"].(map[string]any)
		if a, _ := alerts["A-1"].(map[string]any); a == nil || a["acknowledged"] != "2026-11-03T09:29:00Z" {
			t.Fatalf("overlay %s", ev.Data)
		}
	}
}

func TestPerUserWriteAccess(t *testing.T) {
	_, bob, carol, _ := perUserServer(t, true)
	path := "/dav/calendars/carol/bob~team/ev.ics"
	_, body := etagOf(t, carol, path)
	changed := strings.Replace(strings.Replace(body, "SUMMARY:Standup", "SUMMARY:Standup (room 2)", 1), "-PT15M", "-PT5M", 1)
	carol.expect("PUT", path, changed, http.StatusNoContent, "Content-Type", "text/calendar")
	_, bobBody := etagOf(t, bob, "/dav/calendars/bob/team/ev.ics")
	contains(t, bobBody, "SUMMARY:Standup (room 2)", "-PT15M")
	notContains(t, bobBody, "-PT5M")
	_, carolBody := etagOf(t, carol, path)
	contains(t, carolBody, "SUMMARY:Standup (room 2)", "-PT5M")
	// Back to bob's alarm: plain PUT again, carol follows bob from now on.
	_, carolBody = etagOf(t, carol, path)
	resp, _ := carol.expect("PUT", path, strings.Replace(carolBody, "-PT5M", "-PT15M", 1), http.StatusNoContent, "Content-Type", "text/calendar")
	if tag, _ := etagOf(t, carol, path); strings.Contains(tag, "-u") || resp.Header.Get("ETag") != tag {
		t.Fatalf("carol still has data of her own: %s / %s", tag, resp.Header.Get("ETag"))
	}
}

func TestPerUserOff(t *testing.T) {
	alice, _, _, _ := perUserServer(t, false)
	_, body := etagOf(t, alice, aliceEv)
	dismissed := strings.Replace(body, "SUMMARY:Standup\r\n", "SUMMARY:Standup\r\nX-MOZ-LASTACK:20261103T094600Z\r\n", 1)
	alice.expect("PUT", aliceEv, dismissed, http.StatusForbidden, "Content-Type", "text/calendar")
}
