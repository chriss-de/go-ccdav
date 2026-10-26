package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/store"
)

const importIcs = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Google Inc//EN\r\nMETHOD:PUBLISH\r\n" +
	"BEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\nBEGIN:STANDARD\r\nDTSTART:19701025T030000\r\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\n" +
	"BEGIN:DAYLIGHT\r\nDTSTART:19700329T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\nUID:series@google\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;TZID=Europe/Berlin:20260105T090000\r\nDTEND;TZID=Europe/Berlin:20260105T100000\r\nRRULE:FREQ=WEEKLY;COUNT=5\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:series@google\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID;TZID=Europe/Berlin:20260112T090000\r\nDTSTART;TZID=Europe/Berlin:20260112T110000\r\nDTEND;TZID=Europe/Berlin:20260112T120000\r\nSUMMARY:Standup moved\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:one@google\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260110T100000Z\r\nSUMMARY:Single\r\nEND:VEVENT\r\n" +
	"BEGIN:VTODO\r\nUID:task@google\r\nDTSTAMP:20260101T000000Z\r\nSUMMARY:A task\r\nEND:VTODO\r\n" +
	"END:VCALENDAR\r\n"

func (b *browser) upload(key, filename, content string, status int) string {
	b.e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf", b.csrf())
	mw.WriteField("key", key)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write([]byte(content))
	mw.Close()
	req, _ := http.NewRequest("POST", b.e.srv.URL+b.ui+"import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := new(bytes.Buffer)
	out.ReadFrom(resp.Body)
	if resp.StatusCode != status {
		b.e.t.Fatalf("import: %d, want %d\n%s", resp.StatusCode, status, out)
	}
	return out.String()
}

func TestImportExport(t *testing.T) {
	e := newEnv(t, nil)
	a := e.browser()
	a.login("alice")
	a.post("collections", http.StatusSeeOther, "kind", "calendars", "displayname", "Events only", "components", "VEVENT")
	var events *store.Collection
	for _, c := range e.st.All() {
		if c.Meta().DisplayName == "Events only" {
			events = c
		}
	}
	out := a.upload(events.Key(), "google.ics", importIcs, http.StatusOK)
	contains(t, out, "Imported into Events only: 2 new, 0 updated, 1 skipped.") // the VTODO doesn't fit
	out = a.upload(events.Key(), "google.ics", importIcs, http.StatusOK)
	contains(t, out, "0 new, 2 updated, 1 skipped.")
	if n := len(events.List()); n != 2 {
		t.Fatalf("%d objects", n)
	}
	// Each stored object is a valid resource with its own VTIMEZONE as needed.
	name, _ := events.ByUID("series@google")
	data, _, _ := events.Get(name)
	contains(t, string(data), "TZID:Europe/Berlin", "Standup moved")
	if strings.Contains(string(data), "METHOD") {
		t.Fatal("METHOD kept")
	}
	// Over DAV: the series answers time-range queries like any other.
	if st := e.dav("PROPFIND", "/dav/calendars/alice/"+events.Name+"/"+name, "alice", "pw"); st != http.StatusMultiStatus {
		t.Fatalf("PROPFIND imported object: %d", st)
	}

	// Export: one file, one VTIMEZONE, all events; also for a sharee.
	resp, out := a.do("GET", "/ui/export?key="+url.QueryEscape(events.Key()), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Disposition"), `filename="Events only.ics"`) {
		t.Fatalf("export: %d %v", resp.StatusCode, resp.Header)
	}
	if strings.Count(out, "BEGIN:VTIMEZONE") != 1 || strings.Count(out, "BEGIN:VEVENT") != 3 {
		t.Fatalf("export:\n%s", out)
	}
	// Round trip: importing the export into a fresh calendar gives the same objects.
	a.post("collections", http.StatusSeeOther, "kind", "calendars", "displayname", "Copy", "components", "VEVENT")
	var cp *store.Collection
	for _, c := range e.st.All() {
		if c.Meta().DisplayName == "Copy" {
			cp = c
		}
	}
	contains(t, a.upload(cp.Key(), "x.ics", out, http.StatusOK), "2 new, 0 updated, 0 skipped.")

	b := e.browser()
	b.login("bob")
	b.expect("GET", "/ui/export?key="+url.QueryEscape(events.Key()), nil, http.StatusNotFound) // not shared yet
	events.SetShares([]store.Share{{User: "bob", Access: "read"}})
	b.expect("GET", "/ui/export?key="+url.QueryEscape(events.Key()), nil, http.StatusOK)
	b.upload(events.Key(), "x.ics", importIcs, http.StatusBadRequest) // read access: no import

	// Contacts.
	var book *store.Collection
	for _, c := range e.st.All() {
		if c.Owner == "alice" && c.Kind == store.AddressBooks {
			book = c
		}
	}
	vcf := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:c1\r\nFN:One\r\nEND:VCARD\r\nBEGIN:VCARD\r\nVERSION:4.0\r\nUID:c2\r\nFN:Two\r\nEND:VCARD\r\nBEGIN:VCARD\r\nVERSION:3.0\r\nFN:No UID\r\nEND:VCARD\r\n"
	contains(t, a.upload(book.Key(), "c.vcf", vcf, http.StatusOK), "2 new, 0 updated, 1 skipped.")
	_, out = a.do("GET", "/ui/export?key="+url.QueryEscape(book.Key()), nil)
	if strings.Count(out, "BEGIN:VCARD") != 2 {
		t.Fatalf("vcf export:\n%s", out)
	}
	a.upload(book.Key(), "c.vcf", "not a vcard", http.StatusBadRequest)
}

func TestDeleteCollectionUI(t *testing.T) {
	e := newEnv(t, nil)
	a := e.browser()
	a.login("alice")
	a.post("collections", http.StatusSeeOther, "kind", "calendars", "displayname", "Doomed", "components", "VEVENT")
	var col *store.Collection
	for _, c := range e.st.All() {
		if c.Meta().DisplayName == "Doomed" {
			col = c
		}
	}
	contains(t, a.expect("GET", "/ui/", nil, http.StatusOK), "Really delete <b>Doomed</b> and its 0 items")
	a.post("collections/delete", http.StatusBadRequest, "key", col.Key())
	b := e.browser()
	b.login("bob")
	b.post("collections/delete", http.StatusBadRequest, "key", col.Key(), "confirm", "yes")
	a.post("collections/delete", http.StatusSeeOther, "key", col.Key(), "confirm", "yes")
	if _, err := e.st.Collection("alice", store.Calendars, col.Name); err == nil {
		t.Fatal("collection still exists")
	}
	if strings.Contains(a.expect("GET", "/ui/?msg=deleted", nil, http.StatusOK), ">Doomed<") {
		t.Fatal("still listed")
	}
}

func TestLastUsedAndUILockout(t *testing.T) {
	e := newEnv(t, nil)
	a := e.browser()
	a.login("alice")
	out := a.post("app-passwords", http.StatusOK, "name", "Laptop")
	contains(t, out, "never used")
	pw := strings.SplitN(strings.SplitN(out, `<code class="secret">`, 2)[1], "<", 2)[0]
	e.dav("PROPFIND", "/dav/", "alice", pw)
	contains(t, a.expect("GET", "/ui/", nil, http.StatusOK), "last used 20")

	c := e.browser()
	for range 10 {
		c.expect("POST", "/ui/login", c.loginForm("bob", "x"), http.StatusForbidden)
	}
	out = c.expect("POST", "/ui/login", c.loginForm("bob", "pw"), http.StatusTooManyRequests)
	contains(t, out, "Too many failed attempts")
	// The shared limiter also blocks DAV for that IP and user.
	if st := e.dav("PROPFIND", "/dav/", "bob", "pw"); st != http.StatusTooManyRequests {
		t.Fatalf("DAV after UI lockout: %d", st)
	}
}

// Collection keys are split into path segments; none may lead out of the
// key owner's home.
func TestCollectionKeyTraversal(t *testing.T) {
	e := newEnv(t, nil)
	if st := e.dav("PROPFIND", "/dav/calendars/bob/", "bob", "pw"); st != http.StatusMultiStatus {
		t.Fatalf("bob's home: %d", st)
	}
	bobDir := filepath.Join(e.dir, "users", "bob")
	a := e.browser()
	a.login("alice")
	for _, key := range []string{"alice/../bob", "alice/../../users", "../bob/calendars", "alice/./personal"} {
		if resp, _ := a.do("GET", "/ui/export?key="+url.QueryEscape(key), nil); resp.StatusCode == http.StatusOK {
			t.Errorf("export %q: %d", key, resp.StatusCode)
		}
		a.post("collections/delete", http.StatusBadRequest, "key", key, "confirm", "yes")
	}
	if _, err := os.Stat(bobDir); err != nil {
		t.Fatalf("bob's data is gone: %v", err)
	}
}

// An import that runs into the storage quota stops and says so, rather
// than reporting the rest as skipped.
func TestImportOverQuota(t *testing.T) {
	e := newEnv(t, nil)
	e.st.SetQuota(func() int64 { return 400 })
	a := e.browser()
	a.login("alice")
	out := a.upload("alice/calendars/personal", "cal.ics", importIcs, http.StatusBadRequest)
	contains(t, out, "quota")
}
