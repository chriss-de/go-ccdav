package jmap

import (
	"slices"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/store"
)

func TestCalendarSet(t *testing.T) {
	e, cal, ids := calendarsEnv(t)
	alerts := map[string]any{"d1": map[string]any{"trigger": map[string]any{"offset": "-PT10M"}}}
	rs := e.api("alice",
		call("Calendar/set", map[string]any{"accountId": e.alice, "create": map[string]any{"w": map[string]any{
			"name": "Work", "color": "#336699", "sortOrder": 2, "isVisible": false, "timeZone": "America/New_York",
			"includeInAvailability": "attending", "defaultAlertsWithTime": alerts}}}, "0"),
		call("CalendarEvent/set", map[string]any{"accountId": e.alice, "create": map[string]any{"ev": map[string]any{
			"calendarIds": map[string]any{"#w": true}, "start": "2026-11-05T10:00:00", "timeZone": "America/New_York", "useDefaultAlerts": true}}}, "1"),
		call("CalendarEvent/get", map[string]any{"accountId": e.alice, "ids": []string{"#ev"}, "properties": []string{"alerts"}}, "2"))
	wid := rs[0].Args["created"].(map[string]any)["w"].(map[string]any)["id"].(string)
	if got := rs[2].Args["list"].([]any)[0].(map[string]any)["alerts"].(map[string]any); got["d1"] == nil {
		t.Fatalf("default alerts not applied: %+v", rs[2].Args)
	}
	col, _ := e.st.CollectionByID("alice", store.Calendars, wid)
	m := col.Meta()
	if !slices.Equal(m.Components, []string{"VEVENT", "VTODO"}) || !m.Invisible || m.Availability != "attending" ||
		!strings.Contains(m.Timezone, "TZID:America/New_York") || m.Color != "#336699" {
		t.Fatalf("meta %+v", m)
	}
	c := e.api("alice", call("Calendar/get", map[string]any{"accountId": e.alice, "ids": []string{wid}}, "0"))[0].Args["list"].([]any)[0].(map[string]any)
	if c["isVisible"] != false || c["timeZone"] != "America/New_York" || c["includeInAvailability"] != "attending" || c["defaultAlertsWithTime"] == nil || c["isDefault"] != false {
		t.Fatalf("%+v", c)
	}

	r := e.set("alice", "Calendar/set", map[string]any{"accountId": e.alice,
		"update":  map[string]any{wid: map[string]any{"name": "Office", "isVisible": true, "defaultAlertsWithTime": nil}, cal.ID(): map[string]any{"myRights/mayDelete": false}},
		"destroy": []string{cal.ID()}})
	if setErrType(r, "notUpdated", cal.ID()) != SetInvalidProperties || setErrType(r, "notDestroyed", cal.ID()) != SetCalendarHasEvent {
		t.Fatalf("%+v", r.Args)
	}
	if m := col.Meta(); m.DisplayName != "Office" || m.Invisible || len(m.DefaultAlertsWithTime) != 0 {
		t.Fatalf("after update %+v", m)
	}
	r = e.set("alice", "Calendar/set", map[string]any{"accountId": e.alice, "destroy": []string{cal.ID()}, "onDestroyRemoveEvents": true})
	if len(r.Args["destroyed"].([]any)) != 1 {
		t.Fatalf("%+v", r.Args)
	}
	if r := e.events("alice", map[string]any{"ids": []string{ids["meeting"]}}); len(r.Args["notFound"].([]any)) != 1 {
		t.Fatalf("events survived: %+v", r.Args)
	}
	r = e.set("alice", "Calendar/set", map[string]any{"accountId": e.alice, "create": map[string]any{
		"bad1": map[string]any{"name": "x", "includeInAvailability": "sometimes"},
		"bad2": map[string]any{"name": "x", "timeZone": "Mars/Olympus"},
		"bad3": map[string]any{"name": "x", "defaultAlertsWithTime": map[string]any{"a": map[string]any{"trigger": map[string]any{"offset": "soon"}}}},
	}})
	for _, k := range []string{"bad1", "bad2", "bad3"} {
		if setErrType(r, "notCreated", k) != SetInvalidProperties {
			t.Errorf("%s: %+v", k, r.Args["notCreated"])
		}
	}
}

func TestCalendarSetShared(t *testing.T) {
	e := newEnv(t)
	team, _ := e.st.CreateCollection("bob", store.Calendars, "team", store.Meta{DisplayName: "Team"})
	team.SetShares([]store.Share{{User: "alice", Access: store.AccessWrite}})
	r := e.set("alice", "Calendar/set", map[string]any{"accountId": e.bob,
		"create":  map[string]any{"x": map[string]any{"name": "Mine"}},
		"update":  map[string]any{team.ID(): map[string]any{"name": "Bob's team", "color": "#ff0000", "isVisible": false}},
		"destroy": []string{team.ID()}})
	if setErrType(r, "notCreated", "x") != SetForbidden || setErrType(r, "notDestroyed", team.ID()) != SetForbidden {
		t.Fatalf("%+v", r.Args)
	}
	if p := e.st.SharePrefs("alice", team); p.DisplayName != "Bob's team" || p.Color != "#ff0000" || !p.Invisible {
		t.Fatalf("prefs %+v", p)
	}
	for _, prop := range []map[string]any{{"description": "x"}, {"timeZone": "Europe/Berlin"}, {"includeInAvailability": "none"}} {
		r = e.set("alice", "Calendar/set", map[string]any{"accountId": e.bob, "update": map[string]any{team.ID(): prop}})
		if setErrType(r, "notUpdated", team.ID()) != SetForbidden {
			t.Errorf("%v: %+v", prop, r.Args)
		}
	}
	// The owner shares via shareWith.
	r = e.set("bob", "Calendar/set", map[string]any{"accountId": e.bob, "update": map[string]any{team.ID(): map[string]any{
		"shareWith": map[string]any{PrincipalID("carol"): map[string]any{"mayReadItems": true, "mayRead": true}}}}})
	if r.Args["updated"] == nil {
		t.Fatalf("%+v", r.Args)
	}
	if sh := team.Shares(); len(sh) != 1 || sh[0].User != "carol" {
		t.Fatalf("shares %+v", sh)
	}
}

func TestAvailabilityModes(t *testing.T) {
	e, cal, _ := calendarsEnv(t)
	// meeting.ics: alice organizes, so "attending" keeps it; "none" drops it.
	busy := func() int {
		r := e.api("alice", call("Principal/getAvailability", map[string]any{"accountId": e.alice, "id": PrincipalID("alice"),
			"utcStart": "2026-11-03T00:00:00Z", "utcEnd": "2026-11-04T00:00:00Z"}, "0"))[0]
		return len(r.Args["list"].([]any))
	}
	cal.UpdateMeta(func(m *store.Meta) { m.Availability = "attending" })
	if busy() != 1 {
		t.Fatal("attending: the organizer's meeting is missing")
	}
	cal.UpdateMeta(func(m *store.Meta) { m.Availability = "none" })
	if busy() != 0 {
		t.Fatal("none: still busy")
	}
}
