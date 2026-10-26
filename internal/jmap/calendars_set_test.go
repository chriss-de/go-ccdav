package jmap

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// calFile returns the stored iCalendar of an event id in alice's account.
func (e *env) calFile(id string) string {
	e.t.Helper()
	log, _ := e.st.Account("alice", store.Calendars)
	ref, ok := log.Lookup(id)
	if !ok {
		e.t.Fatalf("no event %s", id)
	}
	col, _ := e.st.CollectionByID("alice", store.Calendars, ref.Coll)
	data, _, err := col.Get(ref.Name)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func TestCalendarEventCreate(t *testing.T) {
	e, cal, _ := calendarsEnv(t)
	rs := e.api("alice",
		call("CalendarEvent/set", map[string]any{"accountId": e.alice, "sendSchedulingMessages": true, "create": map[string]any{
			"n": map[string]any{"calendarIds": map[string]any{cal.ID(): true}, "title": "Lunch", "start": "2026-11-02T12:30:00",
				"timeZone": "Europe/Berlin", "duration": "PT1H", "alerts": map[string]any{"a": map[string]any{"trigger": map[string]any{"offset": "-PT5M"}}},
				"participants":             map[string]any{"me": map[string]any{"calendarAddress": "mailto:alice@example.com", "roles": map[string]any{"owner": true}}},
				"organizerCalendarAddress": "mailto:alice@example.com"}}}, "0"),
		call("CalendarEvent/get", map[string]any{"accountId": e.alice, "ids": []string{"#n"}, "properties": []string{"title", "utcStart", "alerts", "isOrigin"}}, "1"))
	created := rs[0].Args["created"].(map[string]any)["n"].(map[string]any)
	id := created["id"].(string)
	if created["uid"] == nil {
		t.Fatalf("%+v", rs[0].Args)
	}
	got := rs[1].Args["list"].([]any)[0].(map[string]any)
	if got["title"] != "Lunch" || got["utcStart"] != "2026-11-02T11:30:00Z" || got["alerts"].(map[string]any)["a"] == nil || got["isOrigin"] != true {
		t.Fatalf("%+v", got)
	}
	ics := e.calFile(id)
	parsed, err := ical.ParseBytes([]byte(ics))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := caldav.Validate(parsed, nil); err != nil || !strings.Contains(ics, "BEGIN:VTIMEZONE") || !strings.Contains(ics, "TZID:Europe/Berlin") {
		t.Fatalf("stored (%v):\n%s", err, ics)
	}

	bc, _ := e.st.CreateCollection("bob", store.Calendars, "ro", store.Meta{})
	bc.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})
	tasks, _ := e.st.Collection("alice", store.Calendars, "tasks")
	ev := func(extra map[string]any) map[string]any {
		m := map[string]any{"calendarIds": map[string]any{cal.ID(): true}, "start": "2026-11-02T12:30:00"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	r := e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "create": map[string]any{
		"noCal":    map[string]any{"start": "2026-11-02T12:30:00"},
		"taskList": ev(map[string]any{"calendarIds": map[string]any{tasks.ID(): true}}),
		"utc":      ev(map[string]any{"utcStart": "2026-11-02T11:30:00Z"}),
		"badZone":  ev(map[string]any{"timeZone": "Mars/Olympus"}),
		"dupUID":   ev(map[string]any{"uid": "meeting-1@example.com"}),
	}})
	for cid, want := range map[string]string{"noCal": SetInvalidProperties, "taskList": SetInvalidProperties, "utc": SetInvalidProperties,
		"badZone": SetInvalidProperties, "dupUID": SetAlreadyExists} {
		if got := setErrType(r, "notCreated", cid); got != want {
			t.Errorf("%s: %q, want %q", cid, got, want)
		}
	}
	r = e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.bob, "create": map[string]any{
		"ro": map[string]any{"calendarIds": map[string]any{bc.ID(): true}, "start": "2026-11-02T12:30:00"}}})
	if setErrType(r, "notCreated", "ro") != SetForbidden {
		t.Fatalf("%+v", r.Args)
	}
}

func TestCalendarEventUpdate(t *testing.T) {
	e, _, ids := calendarsEnv(t)
	id := ids["meeting"]
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "update": map[string]any{
		id: map[string]any{"title": "Planning Q4 (moved)", "start": "2026-11-04T09:00:00", "participants/p2/participationStatus": "accepted"}}})
	ics := e.calFile(id)
	for _, want := range []string{"SUMMARY:Planning Q4 (moved)", "DTSTART;TZID=Europe/Berlin:20261104T090000", "DTEND;TZID=Europe/Berlin:20261104T103000",
		"X-MOZ-GENERATION:3", "SEQUENCE:3", "CONFERENCE;VALUE=URI;FEATURE=AUDIO,VIDEO;LABEL=Video call:https://meet.example.com/abc"} {
		if !strings.Contains(ics, want) {
			t.Errorf("missing %q:\n%s", want, ics)
		}
	}
	r := e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "update": map[string]any{
		id: map[string]any{"uid": "x"}, ids["allday"]: map[string]any{"utcStart": "2026-01-01T00:00:00Z"}, "nope": map[string]any{"title": "x"}}})
	for k, want := range map[string]string{id: SetInvalidProperties, ids["allday"]: SetInvalidProperties, "nope": SetNotFound} {
		if got := setErrType(r, "notUpdated", k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
}

func TestCalendarEventInstances(t *testing.T) {
	e, _, ids := calendarsEnv(t)
	base := ids["moved_deleted"]
	// Change one occurrence: it becomes an override VEVENT.
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "update": map[string]any{
		base + "_20251027T100000": map[string]any{"title": "Standup with guests", "duration": "PT1H"}}})
	ics := e.calFile(base)
	if strings.Count(ics, "BEGIN:VEVENT") != 4 || !strings.Contains(ics, "RECURRENCE-ID;TZID=Europe/Berlin:20251027T100000") || !strings.Contains(ics, "SUMMARY:Standup with guests") {
		t.Fatalf("override not written:\n%s", ics)
	}
	inst := e.events("alice", map[string]any{"ids": []string{base + "_20251027T100000"}}).Args["list"].([]any)[0].(map[string]any)
	if inst["title"] != "Standup with guests" || inst["duration"] != "PT1H" {
		t.Fatalf("%+v", inst)
	}
	if master := e.events("alice", map[string]any{"ids": []string{base}}).Args["list"].([]any)[0].(map[string]any); master["title"] != "Standup" {
		t.Fatalf("master changed: %v", master["title"])
	}
	// Cancel one: EXDATE, and it's gone from an expanding query.
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "destroy": []string{base + "_20251006T100000"}})
	if ics := e.calFile(base); !strings.Contains(ics, "20251006T100000") || !strings.Contains(ics, "EXDATE") {
		t.Fatalf("not excluded:\n%s", ics)
	}
	got := e.eventQuery(map[string]any{"expandRecurrences": true, "timeZone": "Europe/Berlin",
		"filter": map[string]any{"after": "2025-10-01T00:00:00", "before": "2025-10-08T00:00:00", "uid": "weekly-standup@example.com"}})
	if n := len(got.Args["ids"].([]any)); n != 0 {
		t.Fatalf("cancelled occurrence still listed: %+v", got.Args)
	}
	// Not an occurrence at all.
	r := e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "update": map[string]any{base + "_20251008T100000": map[string]any{"title": "x"}}})
	if setErrType(r, "notUpdated", base+"_20251008T100000") != SetNotFound {
		t.Fatalf("%+v", r.Args)
	}
}

func TestCalendarEventMoveRetryDestroy(t *testing.T) {
	e, cal, ids := calendarsEnv(t)
	other, _ := e.st.CreateCollection("alice", store.Calendars, "work", store.Meta{})
	id := ids["floating"]
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "update": map[string]any{id: map[string]any{"calendarIds": map[string]any{other.ID(): true}}}})
	if _, err := cal.Stat("floating.ics"); err == nil || len(other.List()) != 1 {
		t.Fatal("not moved")
	}
	// A CalDAV client writes in between: both changes survive.
	raw, _ := os.ReadFile("../jscalendar/testdata/allday.ics")
	once := true
	testHookBeforeWrite = func() {
		if once {
			once = false
			cal.Put("allday.ics", []byte(strings.Replace(string(raw), "SUMMARY:Leap day birthday", "SUMMARY:Leap day birthday\r\nX-DAV-EDIT:yes", 1)), store.Cond{})
		}
	}
	defer func() { testHookBeforeWrite = nil }()
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "update": map[string]any{ids["allday"]: map[string]any{"color": "red"}}})
	if ics := e.calFile(ids["allday"]); !strings.Contains(ics, "X-DAV-EDIT:yes") || !strings.Contains(ics, "COLOR:red") {
		t.Fatalf("lost a write:\n%s", ics)
	}
	r := e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.alice, "destroy": []string{ids["allday"], "nope"}})
	if len(r.Args["destroyed"].([]any)) != 1 || setErrType(r, "notDestroyed", "nope") != SetNotFound {
		t.Fatalf("%+v", r.Args)
	}
}

func TestCalendarEventCopyAndParse(t *testing.T) {
	e, cal, ids := calendarsEnv(t)
	team, _ := e.st.CreateCollection("bob", store.Calendars, "team", store.Meta{})
	team.SetShares([]store.Share{{User: "alice", Access: store.AccessWrite}})
	rs := e.api("alice", call("CalendarEvent/copy", map[string]any{"fromAccountId": e.alice, "accountId": e.bob, "onSuccessDestroyOriginal": true,
		"create": map[string]any{"c": map[string]any{"id": ids["meeting"], "calendarIds": map[string]any{team.ID(): true}, "title": "Copied"}}}, "0"))
	if len(rs) != 2 || rs[1].Name != "CalendarEvent/set" || len(rs[1].Args["destroyed"].([]any)) != 1 {
		t.Fatalf("%+v", rs)
	}
	if len(team.List()) != 1 {
		t.Fatal("not copied")
	}
	if _, err := cal.Stat("meeting.ics"); err == nil {
		t.Fatal("original not destroyed")
	}

	up := func(body string) string {
		resp := e.do("alice", "POST", "/jmap/upload/"+e.alice+"/", "text/calendar", []byte(body))
		if resp.StatusCode != http.StatusCreated {
			t.Fatal(resp.StatusCode)
		}
		return decode[uploadResponse](t, resp).BlobID
	}
	raw, _ := os.ReadFile("../jscalendar/testdata/allday.ics")
	two, junk := up(string(raw)), up("not a calendar")
	r := e.api("alice", call("CalendarEvent/parse", map[string]any{"accountId": e.alice, "blobIds": []string{two, junk, "Gnope"}, "properties": []string{"title", "id"}}, "0"))[0]
	parsed := r.Args["parsed"].(map[string]any)
	if l := parsed[two].([]any); len(l) != 2 || l[0].(map[string]any)["title"] != "Leap day birthday" || l[0].(map[string]any)["id"] != nil {
		t.Fatalf("%+v", r.Args)
	}
	if !strings.Contains(strings.Join(ids2list(r.Args["notParsable"]), ","), junk) || !strings.Contains(strings.Join(ids2list(r.Args["notFound"]), ","), "Gnope") {
		t.Fatalf("%+v", r.Args)
	}
}
