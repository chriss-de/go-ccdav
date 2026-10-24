package server

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/store"
)

func TestPrefixAndRoot(t *testing.T) {
	srv, _ := newServer(t, func(c *config.Config) { c.DAVPrefix = "/caldav/" })
	alice := &client{t, srv, "alice"}
	resp, _ := alice.expect("GET", "/.well-known/carddav", "", http.StatusMovedPermanently)
	if resp.Header.Get("Location") != "/caldav/" {
		t.Fatal(resp.Header.Get("Location"))
	}
	_, out := alice.expect("PROPFIND", "/caldav/", propfindPrincipal, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:href>/caldav/principals/alice/</d:href>")
	alice.expect("PROPFIND", "/caldav", propfindPrincipal, http.StatusMultiStatus, "Depth", "0")
	_, out = alice.expect("PROPFIND", "/caldav/calendars/alice/", "", http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "<d:href>/caldav/calendars/alice/personal/</d:href>")
	notContains(t, out, "/dav/")
	for _, p := range []string{"/", "/dav/", "/ui/", "/caldavx/"} {
		alice.expect("PROPFIND", p, propfindPrincipal, http.StatusNotFound, "Depth", "0")
		alice.expect("GET", p, "", http.StatusNotFound)
	}
}

var ctagRe = regexp.MustCompile(`<cs:getctag>([^<]*)</cs:getctag>`)

func ctagOf(t *testing.T, c *client, path string) string {
	t.Helper()
	_, out := c.expect("PROPFIND", path, `<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><cs:getctag/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	m := ctagRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no ctag:\n%s", out)
	}
	return m[1]
}

func TestSharing(t *testing.T) {
	srv, st := newServer(t, func(c *config.Config) { c.Groups = map[string][]string{"fam": {"bob", "carol"}} })
	alice, bob, carol := &client{t, srv, "alice"}, &client{t, srv, "bob"}, &client{t, srv, "carol"}

	own := "/dav/calendars/alice/personal/"
	alice.expect("PUT", own+"a.ics", event("a", "20260105T090000Z", ""), http.StatusCreated)
	col, err := st.Collection("alice", store.Calendars, "personal")
	if err != nil {
		t.Fatal(err)
	}
	// Group read plus a direct write share for bob: the highest wins.
	if err := col.SetShares([]store.Share{{Group: "fam", Access: "read"}, {User: "bob", Access: "write"}}); err != nil {
		t.Fatal(err)
	}

	// Shared collection shows up in both sharees' homes.
	props := `<d:propfind xmlns:d="DAV:" xmlns:a="http://apple.com/ns/ical/"><d:prop><d:displayname/><d:owner/><d:current-user-privilege-set/><a:calendar-color/></d:prop></d:propfind>`
	for _, c := range []*client{bob, carol} {
		_, out := c.expect("PROPFIND", "/dav/calendars/"+c.user+"/", props, http.StatusMultiStatus, "Depth", "1")
		contains(t, out, "/dav/calendars/"+c.user+"/alice~personal/</d:href>", "<d:displayname>Personal (Alice)</d:displayname>",
			"<d:owner><d:href>/dav/principals/alice/</d:href></d:owner>")
	}
	// The owner's real path stays private.
	bob.expect("PROPFIND", own, "", http.StatusForbidden, "Depth", "0")

	bobCal, carolCal := "/dav/calendars/bob/alice~personal/", "/dav/calendars/carol/alice~personal/"
	_, out := carol.expect("PROPFIND", carolCal, props, http.StatusMultiStatus, "Depth", "0")
	notContains(t, out, "<d:write-content/>", "<d:all/>")
	_, out = bob.expect("PROPFIND", bobCal, props, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:write-content/>")
	notContains(t, out, "<d:all/>")

	// Read vs write.
	carol.expect("GET", carolCal+"a.ics", "", http.StatusOK)
	_, out = carol.expect("PUT", carolCal+"c.ics", event("c", "20260105T090000Z", ""), http.StatusForbidden)
	contains(t, out, "need-privileges")
	carol.expect("DELETE", carolCal+"a.ics", "", http.StatusForbidden)
	bob.expect("PUT", bobCal+"b.ics", event("b", "20260106T090000Z", ""), http.StatusCreated)
	alice.expect("GET", own+"b.ics", "", http.StatusOK) // lands in alice's calendar

	// Multiget and query through the alias use alias hrefs.
	_, out = carol.expect("REPORT", carolCal, `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop><d:href>`+carolCal+`b.ics</d:href></c:calendar-multiget>`, http.StatusMultiStatus)
	contains(t, out, carolCal+"b.ics</d:href><d:propstat>")
	_, out = carol.expect("REPORT", carolCal, query("20260101T000000Z", "20260201T000000Z"), http.StatusMultiStatus, "Depth", "1")
	contains(t, out, carolCal+"a.ics", carolCal+"b.ics")

	// Personal settings: bob's color and name don't touch alice's.
	aliceTag, bobTag := ctagOf(t, alice, own), ctagOf(t, bob, bobCal)
	_, syncOut := bob.expect("REPORT", bobCal, syncReport("", 0), http.StatusMultiStatus)
	bobSync := tokenOf(t, syncOut)
	_, out = bob.expect("PROPPATCH", bobCal, `<d:propertyupdate xmlns:d="DAV:" xmlns:a="http://apple.com/ns/ical/"><d:set><d:prop>
		<a:calendar-color>#00ff00ff</a:calendar-color><d:displayname>Alice's</d:displayname></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	contains(t, out, "200 OK")
	notContains(t, out, "403")
	_, out = bob.expect("PROPFIND", bobCal, props, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "#00ff00ff", "<d:displayname>Alice&#39;s</d:displayname>")
	_, out = alice.expect("PROPFIND", own, props, http.StatusMultiStatus, "Depth", "0")
	notContains(t, out, "#00ff00ff")
	contains(t, out, "<d:displayname>Personal</d:displayname>")
	if ctagOf(t, alice, own) != aliceTag || ctagOf(t, bob, bobCal) == bobTag {
		t.Fatal("sharee prefs must move only the sharee's ctag")
	}
	_, out = bob.expect("REPORT", bobCal, syncReport(bobSync, 0), http.StatusMultiStatus)
	notContains(t, out, ".ics")
	if tokenOf(t, out) == bobSync {
		t.Fatal("sharee sync token did not move after their own color change")
	}
	_, out = bob.expect("PROPPATCH", bobCal, `<d:propertyupdate xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop>
		<c:calendar-description>x</c:calendar-description></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	contains(t, out, "403 Forbidden")

	// Sharee DELETE hides; the calendar stays for alice and carol.
	bob.expect("DELETE", bobCal, "", http.StatusNoContent)
	_, out = bob.expect("PROPFIND", "/dav/calendars/bob/", "", http.StatusMultiStatus, "Depth", "1")
	notContains(t, out, "alice~personal")
	alice.expect("GET", own+"a.ics", "", http.StatusOK)
	_, out = carol.expect("PROPFIND", "/dav/calendars/carol/", "", http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "alice~personal")

	// Unsharing (group removed) takes access away.
	if err := col.SetShares([]store.Share{{User: "bob", Access: "write"}}); err != nil {
		t.Fatal(err)
	}
	carol.expect("GET", carolCal+"a.ics", "", http.StatusNotFound)
	_, out = carol.expect("PROPFIND", "/dav/calendars/carol/", "", http.StatusMultiStatus, "Depth", "1")
	notContains(t, out, "alice~personal")

	// Bogus aliases.
	for _, p := range []string{"/dav/calendars/bob/bob~personal/", "/dav/calendars/bob/nobody~personal/", "/dav/calendars/bob/alice~nope/"} {
		bob.expect("PROPFIND", p, "", http.StatusNotFound, "Depth", "0")
		bob.expect("MKCALENDAR", p, "", http.StatusNotFound)
	}
	// Address books share the same way.
	ab, _ := st.Collection("alice", store.AddressBooks, "contacts")
	if ab == nil {
		alice.expect("PROPFIND", "/dav/addressbooks/alice/", "", http.StatusMultiStatus, "Depth", "1")
		ab, _ = st.Collection("alice", store.AddressBooks, "contacts")
	}
	ab.SetShares([]store.Share{{User: "carol", Access: "read"}})
	_, out = carol.expect("PROPFIND", "/dav/addressbooks/carol/", "", http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/addressbooks/carol/alice~contacts/")
}

func TestGroupPrincipals(t *testing.T) {
	srv, _ := newServer(t, func(c *config.Config) { c.Groups = map[string][]string{"fam": {"alice", "bob"}} })
	alice := &client{t, srv, "alice"}
	_, out := alice.expect("PROPFIND", "/dav/principals/groups/fam/", "", http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:principal/>", "<d:group-member-set><d:href>/dav/principals/alice/</d:href><d:href>/dav/principals/bob/</d:href>",
		"<cal:calendar-user-type>GROUP</cal:calendar-user-type>")
	_, out = alice.expect("PROPFIND", "/dav/principals/bob/", `<d:propfind xmlns:d="DAV:"><d:prop><d:group-membership/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:group-membership><d:href>/dav/principals/groups/fam/</d:href>")
	alice.expect("PROPFIND", "/dav/principals/groups/nope/", "", http.StatusNotFound, "Depth", "0")
	_, out = alice.expect("PROPFIND", "/dav/principals/groups/", "", http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/principals/groups/fam/")
	if strings.Count(out, "<d:response>") != 2 {
		t.Errorf("unexpected listing:\n%s", out)
	}
}
