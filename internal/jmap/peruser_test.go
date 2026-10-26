package jmap

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/store"
)

// sharedCalendar gives bob a calendar with the meeting and the weekly
// standup, shared read-only with alice and read-write with carol.
func sharedCalendar(t *testing.T) (*env, *store.Collection, string, string) {
	t.Helper()
	e := newEnv(t)
	team, _ := e.st.CreateCollection("bob", store.Calendars, "team", store.Meta{DisplayName: "Team"})
	team.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}, {User: "carol", Access: store.AccessWrite}})
	log, _ := e.st.Account("bob", store.Calendars)
	ids := []string{}
	for _, n := range []string{"meeting", "moved_deleted"} {
		raw, _ := os.ReadFile("../jscalendar/testdata/" + n + ".ics")
		team.Put(n+".ics", raw, store.Cond{})
		id, _ := log.ObjectID(team, n+".ics")
		ids = append(ids, id)
	}
	return e, team, ids[0], ids[1]
}

func (e *env) eventAs(user, acct, id string, props ...string) map[string]any {
	e.t.Helper()
	args := map[string]any{"accountId": acct, "ids": []string{id}}
	if len(props) > 0 {
		args["properties"] = props
	}
	r := e.api(user, call("CalendarEvent/get", args, "g"))[0]
	l, _ := r.Args["list"].([]any)
	if len(l) != 1 {
		e.t.Fatalf("%s: %+v", id, r.Args)
	}
	return l[0].(map[string]any)
}

func alertOffsets(ev map[string]any) string {
	var out []string
	for _, a := range ev["alerts"].(map[string]any) {
		out = append(out, a.(map[string]any)["trigger"].(map[string]any)["offset"].(string))
	}
	return strings.Join(out, ",")
}

func TestPerUserReadOnly(t *testing.T) {
	e, team, meeting, _ := sharedCalendar(t)
	if ev := e.eventAs("alice", e.bob, meeting); len(ev["alerts"].(map[string]any)) != 2 {
		t.Fatalf("alice should start with bob's alerts: %+v", ev["alerts"])
	}
	c := e.api("alice", call("Calendar/get", map[string]any{"accountId": e.bob}, "0"))[0].Args["list"].([]any)[0].(map[string]any)
	if c["myRights"].(map[string]any)["mayUpdatePrivate"] != true {
		t.Fatalf("%+v", c["myRights"])
	}
	_, before, _ := team.Get("meeting.ics")
	aliceState := e.api("alice", call("CalendarEvent/get", map[string]any{"accountId": e.bob, "ids": []string{}}, "0"))[0].Args["state"].(string)
	bobState := e.api("bob", call("CalendarEvent/get", map[string]any{"accountId": e.bob, "ids": []string{}}, "0"))[0].Args["state"].(string)

	// Read-only, but her own reminders and color are hers to set.
	r := e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.bob, "update": map[string]any{meeting: map[string]any{
		"alerts": map[string]any{"mine": map[string]any{"trigger": map[string]any{"offset": "-PT30M"}}}, "color": "green"}}})
	if r.Args["updated"] == nil {
		t.Fatalf("%+v", r.Args)
	}
	if ev := e.eventAs("alice", e.bob, meeting); alertOffsets(ev) != "-PT30M" || ev["color"] != "green" {
		t.Fatalf("alice sees %+v %v", ev["alerts"], ev["color"])
	}
	if ev := e.eventAs("bob", e.bob, meeting); len(ev["alerts"].(map[string]any)) != 2 || ev["color"] != "tomato" {
		t.Fatalf("bob sees %+v %v", ev["alerts"], ev["color"])
	}
	if _, after, _ := team.Get("meeting.ics"); after.ETag != before.ETag {
		t.Fatal("the calendar object changed")
	}
	// Her state moved on (with the change listed), bob's didn't.
	ch := e.api("alice", call("CalendarEvent/changes", map[string]any{"accountId": e.bob, "sinceState": aliceState}, "0"))[0]
	if ch.Name == "error" || strings.Join(ids2list(ch.Args["updated"]), ",") != meeting {
		t.Fatalf("alice's changes %+v", ch)
	}
	ch = e.api("bob", call("CalendarEvent/changes", map[string]any{"accountId": e.bob, "sinceState": bobState}, "0"))[0]
	if ch.Name == "error" || len(ch.Args["updated"].([]any)) != 0 || ch.Args["newState"] != bobState {
		t.Fatalf("bob's changes %+v", ch)
	}

	// Anything else stays read-only.
	r = e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.bob, "update": map[string]any{meeting: map[string]any{"title": "Mine now"}}})
	if setErrType(r, "notUpdated", meeting) != SetForbidden {
		t.Fatalf("%+v", r.Args)
	}
	// No alerts at all is a value of its own; bob's stay.
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.bob, "update": map[string]any{meeting: map[string]any{"alerts": nil}}})
	if ev := e.eventAs("alice", e.bob, meeting); ev["alerts"] != nil {
		t.Fatalf("alice still has alerts %+v", ev["alerts"])
	}
	// Setting bob's values again means "follow bob" (no overlay left).
	bobAlerts := e.eventAs("bob", e.bob, meeting)["alerts"]
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.bob, "update": map[string]any{meeting: map[string]any{"alerts": bobAlerts, "color": "tomato"}}})
	if o, _ := e.st.Overlay("alice", "bob"); o.Events[meeting].Data != nil {
		t.Fatalf("overlay left: %s", o.Events[meeting].Data)
	}
}

func TestPerUserInstances(t *testing.T) {
	e, team, _, standup := sharedCalendar(t)
	inst := standup + "_20251027T100000"
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.bob, "update": map[string]any{
		inst: map[string]any{"alerts": map[string]any{"x": map[string]any{"trigger": map[string]any{"offset": "-PT1H"}}}}}})
	if ev := e.eventAs("alice", e.bob, inst); alertOffsets(ev) != "-PT1H" || ev["title"] != "Standup" {
		t.Fatalf("alice's instance %+v", ev)
	}
	if ev := e.eventAs("bob", e.bob, inst); ev["alerts"] != nil {
		t.Fatalf("bob's instance got alice's alert: %+v", ev["alerts"])
	}
	data, _, _ := team.Get("moved_deleted.ics")
	if strings.Contains(string(data), "20251027T100000") {
		t.Fatalf("the file got an override:\n%s", data)
	}
	// Expansion for alice still lists the instance once, with her alert.
	r := e.api("alice", call("CalendarEvent/query", map[string]any{"accountId": e.bob, "expandRecurrences": true, "timeZone": "Europe/Berlin",
		"filter": map[string]any{"after": "2025-10-27T00:00:00", "before": "2025-10-28T00:00:00"}}, "0"))[0]
	if got := ids2list(r.Args["ids"]); len(got) != 1 || got[0] != inst {
		t.Fatalf("%+v", r.Args)
	}
}

func TestPerUserWithWriteAccess(t *testing.T) {
	e, team, meeting, _ := sharedCalendar(t)
	e.set("carol", "CalendarEvent/set", map[string]any{"accountId": e.bob, "update": map[string]any{meeting: map[string]any{
		"title": "Planning (room changed)", "keywords": map[string]any{"mine": true}}}})
	bob := e.eventAs("bob", e.bob, meeting)
	if bob["title"] != "Planning (room changed)" || bob["keywords"].(map[string]any)["mine"] != nil {
		t.Fatalf("bob sees %v %v", bob["title"], bob["keywords"])
	}
	if carol := e.eventAs("carol", e.bob, meeting); len(carol["keywords"].(map[string]any)) != 1 {
		t.Fatalf("carol sees %v", carol["keywords"])
	}
	data, _, _ := team.Get("meeting.ics")
	if !strings.Contains(string(data), "SUMMARY:Planning (room changed)") || !strings.Contains(string(data), "CATEGORIES:Work,Planning") {
		t.Fatalf("file:\n%s", data)
	}
}

func TestPerUserPush(t *testing.T) {
	fastPush(t)
	e, _, meeting, _ := sharedCalendar(t)
	alice, stopA := e.openStream("alice", "types=CalendarEvent&closeafter=no&ping=0")
	defer stopA()
	bob, stopB := e.openStream("bob", "types=CalendarEvent&closeafter=no&ping=0")
	defer stopB()
	next(t, alice, time.Second)
	next(t, bob, time.Second)
	e.set("alice", "CalendarEvent/set", map[string]any{"accountId": e.bob, "update": map[string]any{meeting: map[string]any{"color": "green"}}})
	if ev, ok := next(t, alice, time.Second); !ok || changed(ev)[e.bob] == nil {
		t.Fatalf("alice not told: %+v", ev)
	}
	if ev, ok := next(t, bob, 300*time.Millisecond); ok {
		t.Fatalf("bob told about alice's color: %+v", ev)
	}
}
