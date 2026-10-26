package jmap

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/store"
)

// calendarsEnv gives alice her default calendar with the jscalendar test
// events, plus a task list that JMAP must not show.
func calendarsEnv(t *testing.T) (*env, *store.Collection, map[string]string) {
	t.Helper()
	e := newEnv(t)
	e.api("alice", call("Calendar/get", map[string]any{"accountId": e.alice}, "0")) // creates the home
	cal, err := e.st.Collection("alice", store.Calendars, "personal")
	if err != nil {
		t.Fatal(err)
	}
	tasks, _ := e.st.CreateCollection("alice", store.Calendars, "tasks", store.Meta{DisplayName: "Tasks", Components: []string{"VTODO"}})
	tasks.Put("t.ics", []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:t1\r\nSUMMARY:Milk\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"), store.Cond{})
	cal.Put("todo.ics", []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:t2\r\nSUMMARY:Bread\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"), store.Cond{})
	ids := map[string]string{}
	log, _ := e.st.Account("alice", store.Calendars)
	for _, name := range []string{"moved_deleted", "meeting", "allday", "floating", "outlook"} {
		raw, err := os.ReadFile("../jscalendar/testdata/" + name + ".ics")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := cal.Put(name+".ics", raw, store.Cond{}); err != nil {
			t.Fatal(err)
		}
		ids[name], _ = log.ObjectID(cal, name+".ics")
	}
	return e, cal, ids
}

func (e *env) events(user string, args map[string]any) apiResp {
	e.t.Helper()
	if _, ok := args["accountId"]; !ok {
		args["accountId"] = e.alice
	}
	return e.api(user, call("CalendarEvent/get", args, "g"))[0]
}

func TestCalendarSessionAndGet(t *testing.T) {
	e, cal, _ := calendarsEnv(t)
	s := decode[session](t, e.do("alice", "GET", "/jmap/session", "", nil))
	caps := s.Accounts[e.alice].AccountCapabilities
	if c := caps[CapCalendars].(map[string]any); c["maxCalendarsPerEvent"] != float64(1) || c["mayCreateCalendar"] != true || caps[CapAvailability] == nil {
		t.Fatalf("%+v", caps)
	}
	r := e.api("alice", call("Calendar/get", map[string]any{"accountId": e.alice}, "0"))[0]
	list := r.Args["list"].([]any)
	if len(list) != 1 { // the task list is not a JMAP calendar
		t.Fatalf("%d calendars: %+v", len(list), list)
	}
	c := list[0].(map[string]any)
	rights := c["myRights"].(map[string]any)
	if c["id"] != cal.ID() || c["isDefault"] != true || c["name"] != "Personal" || rights["mayWriteAll"] != true || rights["mayShare"] != true {
		t.Fatalf("%+v", c)
	}

	// Shared read-only with alice, who picked her own color.
	bc, _ := e.st.CreateCollection("bob", store.Calendars, "team", store.Meta{DisplayName: "Team", Color: "#00ff00"})
	bc.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})
	e.st.UpdateSharePrefs("alice", bc.Key(), func(p *store.SharePrefs) { p.Color = "#ff0000" })
	c = e.api("alice", call("Calendar/get", map[string]any{"accountId": e.bob}, "0"))[0].Args["list"].([]any)[0].(map[string]any)
	rights = c["myRights"].(map[string]any)
	if c["color"] != "#ff0000" || c["shareWith"] != nil || rights["mayWriteAll"] != false || rights["mayReadItems"] != true {
		t.Fatalf("shared %+v", c)
	}
}

func TestCalendarEventGet(t *testing.T) {
	e, cal, ids := calendarsEnv(t)
	r := e.events("alice", map[string]any{})
	if n := len(r.Args["list"].([]any)); n != len(ids) {
		t.Fatalf("%d events, want %d (tasks must not show)", n, len(ids))
	}
	r = e.events("alice", map[string]any{"ids": []string{ids["meeting"]}})
	m := r.Args["list"].([]any)[0].(map[string]any)
	if m["calendarIds"].(map[string]any)[cal.ID()] != true || m["isDraft"] != false || m["isOrigin"] != true ||
		m["utcStart"] != "2026-11-03T13:00:00Z" || m["utcEnd"] != "2026-11-03T14:30:00Z" || m["created"] != "2026-10-20T08:00:00Z" {
		t.Fatalf("%+v", m)
	}
	r = e.events("alice", map[string]any{"ids": []string{ids["meeting"]}, "reduceParticipants": true, "properties": []string{"participants"}})
	if ps := r.Args["list"].([]any)[0].(map[string]any)["participants"].(map[string]any); len(ps) != 1 {
		t.Fatalf("reduced to %d", len(ps))
	}

	// Instances of the weekly standup.
	base := ids["moved_deleted"]
	get := func(id string) map[string]any {
		r := e.events("alice", map[string]any{"ids": []string{id}})
		if l := r.Args["list"].([]any); len(l) == 1 {
			return l[0].(map[string]any)
		}
		return nil
	}
	moved := get(base + "_20251013T100000")
	if moved == nil || moved["title"] != "Standup (moved)" || moved["start"] != "2025-10-14T15:00:00" ||
		moved["baseEventId"] != base || moved["recurrenceId"] != "2025-10-13T10:00:00" || moved["recurrenceRule"] != nil {
		t.Fatalf("moved %+v", moved)
	}
	if plain := get(base + "_20251027T100000"); plain == nil || plain["title"] != "Standup" || plain["utcStart"] != "2025-10-27T09:00:00Z" {
		t.Fatalf("generated %+v", plain)
	}
	for _, bad := range []string{base + "_20251020T100000" /* excluded */, base + "_20251015T100000" /* no such day */, base + "_junk"} {
		if get(bad) != nil {
			t.Errorf("%s found", bad)
		}
	}
	// recurrenceOverridesAfter drops the October overrides.
	r = e.events("alice", map[string]any{"ids": []string{base}, "recurrenceOverridesAfter": "2025-11-01T00:00:00Z", "properties": []string{"recurrenceOverrides"}})
	ro := r.Args["list"].([]any)[0].(map[string]any)["recurrenceOverrides"].(map[string]any)
	if len(ro) != 1 || ro["2025-11-03T10:00:00"] == nil {
		t.Fatalf("limited overrides %+v", ro)
	}
}

func (e *env) eventQuery(args map[string]any) apiResp {
	e.t.Helper()
	args["accountId"] = e.alice
	return e.api("alice", call("CalendarEvent/query", args, "q"))[0]
}

func TestCalendarEventQuery(t *testing.T) {
	e, cal, ids := calendarsEnv(t)
	q := func(args map[string]any) []string {
		t.Helper()
		r := e.eventQuery(args)
		if r.Name == "error" {
			t.Fatalf("%v: %+v", args, r.Args)
		}
		var out []string
		for _, id := range r.Args["ids"].([]any) {
			out = append(out, id.(string))
		}
		return out
	}
	name := map[string]string{}
	for n, id := range ids {
		name[id] = n
	}
	names := func(got []string) []string {
		var out []string
		for _, id := range got {
			out = append(out, name[id])
		}
		slices.Sort(out)
		return out
	}
	// Series overlapping October 2025 (Berlin): the standup only.
	if got := names(q(map[string]any{"filter": map[string]any{"after": "2025-10-01T00:00:00", "before": "2025-11-01T00:00:00"}, "timeZone": "Europe/Berlin"})); !slices.Equal(got, []string{"moved_deleted"}) {
		t.Errorf("window: %v", got)
	}
	for f, want := range map[string]string{"title": "planning", "location": "room 1", "owner": "alice", "participant": "bob", "description": "hiring", "text": "budget"} {
		if got := names(q(map[string]any{"filter": map[string]any{f: want}})); !slices.Equal(got, []string{"meeting"}) {
			t.Errorf("%s=%s: %v", f, want, got)
		}
	}
	if got := names(q(map[string]any{"filter": map[string]any{"text": "moved earlier"}})); !slices.Equal(got, []string{"moved_deleted"}) {
		t.Errorf("text in an override: %v", got)
	}
	if got := q(map[string]any{"filter": map[string]any{"inCalendar": cal.ID()}}); len(got) != len(ids) {
		t.Errorf("inCalendar: %d", len(got))
	}
	if got := names(q(map[string]any{"filter": map[string]any{"uid": "leap-birthday@example.com"}})); !slices.Equal(got, []string{"allday"}) {
		t.Errorf("uid: %v", got)
	}
	// Sorted by start, descending: the 2026 meeting first.
	if got := q(map[string]any{"sort": []any{map[string]any{"property": "start", "isAscending": false}}}); name[got[0]] != "meeting" {
		t.Errorf("by start desc: %v", names(got))
	}

	// Expanded: the standup's instances in the window, by start.
	base := ids["moved_deleted"]
	got := q(map[string]any{"expandRecurrences": true, "timeZone": "Europe/Berlin",
		"filter": map[string]any{"after": "2025-10-01T00:00:00", "before": "2025-11-10T00:00:00", "title": "standup"}})
	want := []string{base + "_20251006T100000", base + "_20251013T100000", base + "_20251027T100000", base + "_20251103T100000"}
	if !slices.Equal(got, want) {
		t.Errorf("expanded:\n got %v\nwant %v", got, want)
	}
	// The ids work with /get.
	r := e.events("alice", map[string]any{"ids": got[3:], "properties": []string{"title", "start", "timeZone"}})
	if ev := r.Args["list"].([]any)[0].(map[string]any); ev["title"] != "Standup (moved earlier, RECURRENCE-ID in UTC)" || ev["start"] != "2025-10-31T08:00:00" {
		t.Errorf("instance from query: %+v", ev)
	}

	for _, tc := range []struct {
		args map[string]any
		err  string
	}{
		{map[string]any{"expandRecurrences": true, "filter": map[string]any{"after": "2025-10-01T00:00:00"}}, ErrInvalidArguments},
		{map[string]any{"expandRecurrences": true, "filter": map[string]any{"operator": "AND", "conditions": []any{}}}, ErrInvalidArguments},
		{map[string]any{"expandRecurrences": true, "filter": map[string]any{"after": "2025-01-01T00:00:00", "before": "2027-01-01T00:00:00"}}, ErrInvalidArguments},
		{map[string]any{"filter": map[string]any{"attendee": "x"}}, ErrUnsupportedFilter},
		{map[string]any{"sort": []any{map[string]any{"property": "title"}}}, ErrUnsupportedSort},
		{map[string]any{"timeZone": "Mars/Olympus"}, ErrInvalidArguments},
		{map[string]any{"timeZone": "Local"}, ErrInvalidArguments}, // the server's zone
	} {
		if r := e.eventQuery(tc.args); errType(r) != tc.err {
			t.Errorf("%v: %+v, want %s", tc.args, r, tc.err)
		}
	}
}

// An event every second has millions of occurrences in the longest window:
// expanding stops at the budget, and plain overlap checks copy nothing.
func TestEventQueryExpansionBudget(t *testing.T) {
	e, cal, _ := calendarsEnv(t)
	cal.Put("secondly.ics", []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:sec\r\nDTSTAMP:20260101T000000Z\r\n"+
		"DTSTART:20260101T000000Z\r\nRRULE:FREQ=SECONDLY\r\nSUMMARY:tick\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), store.Cond{})
	log, _ := e.st.Account("alice", store.Calendars)
	id, _ := log.ObjectID(cal, "secondly.ics")
	r := e.eventQuery(map[string]any{"expandRecurrences": true,
		"filter": map[string]any{"after": "2026-01-01T00:00:00", "before": "2026-12-31T00:00:00"}})
	if errType(r) != ErrRequestTooLarge {
		t.Fatalf("expanded a year of seconds: %.300v", r)
	}
	r = e.eventQuery(map[string]any{"filter": map[string]any{"after": "2026-01-05T00:00:00", "title": "tick"}})
	if got, _ := r.Args["ids"].([]any); len(got) != 1 || got[0] != id {
		t.Fatalf("open-ended window: %.300v", r)
	}
}

func TestAvailabilityAndIdentity(t *testing.T) {
	e, _, _ := calendarsEnv(t)
	r := e.api("alice", call("Principal/getAvailability", map[string]any{"accountId": e.alice, "id": PrincipalID("alice"),
		"utcStart": "2026-11-03T00:00:00Z", "utcEnd": "2026-11-04T00:00:00Z"}, "0"))[0]
	list := r.Args["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("%+v", r.Args)
	}
	if p := list[0].(map[string]any); p["utcStart"] != "2026-11-03T13:00:00Z" || p["utcEnd"] != "2026-11-03T14:30:00Z" || p["busyStatus"] != "confirmed" {
		t.Fatalf("%+v", p)
	}
	// Bob sees none of alice's (unshared) calendars.
	r = e.api("bob", call("Principal/getAvailability", map[string]any{"accountId": e.bob, "id": PrincipalID("alice"),
		"utcStart": "2026-11-03T00:00:00Z", "utcEnd": "2026-11-04T00:00:00Z"}, "0"))[0]
	if len(r.Args["list"].([]any)) != 0 {
		t.Fatalf("bob: %+v", r.Args)
	}
	r = e.api("alice", call("Principal/getAvailability", map[string]any{"accountId": e.alice, "id": PrincipalID("alice"),
		"utcStart": "2026-01-01T00:00:00Z", "utcEnd": "2027-06-01T00:00:00Z"}, "0"))[0]
	if errType(r) != ErrInvalidArguments {
		t.Fatalf("too long: %+v", r)
	}

	r = e.api("alice", call("ParticipantIdentity/get", map[string]any{"accountId": e.alice}, "0"))[0]
	if l := r.Args["list"].([]any); len(l) != 1 || l[0].(map[string]any)["calendarAddress"] != "mailto:alice@example.com" {
		t.Fatalf("%+v", r.Args)
	}
	state := r.Args["state"].(string)
	r = e.api("alice", call("ParticipantIdentity/changes", map[string]any{"accountId": e.alice, "sinceState": state}, "0"))[0]
	if r.Name == "error" {
		t.Fatalf("%+v", r)
	}
	if l := e.api("bob", call("ParticipantIdentity/get", map[string]any{"accountId": e.bob}, "0"))[0].Args["list"].([]any); len(l) != 0 {
		t.Fatalf("bob has no email: %+v", l)
	}
}

func TestCalendarEventChanges(t *testing.T) {
	e, cal, ids := calendarsEnv(t)
	state := e.events("alice", map[string]any{"ids": []string{}}).Args["state"].(string)
	cal.Delete("allday.ics", store.Cond{})
	r := e.api("alice", call("CalendarEvent/changes", map[string]any{"accountId": e.alice, "sinceState": state}, "0"))[0]
	if !slices.Equal(ids2list(r.Args["destroyed"]), []string{ids["allday"]}) {
		t.Fatalf("%+v", r.Args)
	}
}

func ids2list(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Like DAV requests, a JMAP request that runs past its deadline while
// expanding recurrences stops, and its calls fail instead of answering
// with partial results.
func TestExpansionDeadline(t *testing.T) {
	defer func(d time.Duration) { workTimeout = d }(workTimeout)
	workTimeout = 100 * time.Millisecond
	e, cal, _ := calendarsEnv(t)
	for i := range 5 {
		cal.Put(fmt.Sprintf("s%d.ics", i), []byte(fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:s%d\r\n", i)+
			"DTSTAMP:20000101T000000Z\r\nDTSTART:20000101T000000Z\r\nRRULE:FREQ=SECONDLY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), store.Cond{})
	}
	start := time.Now()
	r := e.api("alice",
		call("CalendarEvent/query", map[string]any{"accountId": e.alice, "filter": map[string]any{"after": "2026-01-01T00:00:00", "before": "2026-01-02T00:00:00"}}, "q"),
		call("Principal/getAvailability", map[string]any{"accountId": e.alice, "id": PrincipalID("alice"),
			"utcStart": "2026-01-01T00:00:00Z", "utcEnd": "2026-01-02T00:00:00Z"}, "a"))
	for _, x := range r {
		if errType(x) != ErrServerUnavailable {
			t.Errorf("%s: %.200v", x.ID, x)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
}
