package jmap

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
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

// A reader's reduced view (reduceParticipants, an override window) must
// not leak into the cached conversion that others see and edits build on.
func TestReducedGetKeepsCache(t *testing.T) {
	e, team, meeting, standup := sharedCalendar(t)
	count := func(id, prop string) int {
		m, _ := e.eventAs("bob", e.bob, id, prop)[prop].(map[string]any)
		return len(m)
	}
	parts, overs := count(meeting, "participants"), count(standup, "recurrenceOverrides")
	if parts < 2 || overs < 1 {
		t.Fatalf("fixture: %d participants, %d overrides", parts, overs)
	}
	e.api("alice", call("CalendarEvent/get", map[string]any{"accountId": e.bob, "ids": []string{meeting}, "reduceParticipants": true}, "g"))
	e.api("alice", call("CalendarEvent/get", map[string]any{"accountId": e.bob, "ids": []string{standup},
		"recurrenceOverridesAfter": "2100-01-01T00:00:00Z"}, "g"))
	if p, o := count(meeting, "participants"), count(standup, "recurrenceOverrides"); p != parts || o != overs {
		t.Fatalf("after alice's reduced gets bob sees %d participants (want %d), %d overrides (want %d)", p, parts, o, overs)
	}
	before, _, _ := team.Get("moved_deleted.ics")
	r := e.api("bob", call("CalendarEvent/set", map[string]any{"accountId": e.bob,
		"update": map[string]any{standup: map[string]any{"title": "renamed"}}}, "s"))[0]
	if _, ok := r.Args["updated"].(map[string]any)[standup]; !ok {
		t.Fatalf("update: %+v", r.Args)
	}
	after, _, _ := team.Get("moved_deleted.ics")
	if n, m := strings.Count(string(before), "RECURRENCE-ID"), strings.Count(string(after), "RECURRENCE-ID"); n != m {
		t.Fatalf("RECURRENCE-ID lines %d -> %d", n, m)
	}
}

// Blobs were kept per account owner: a read-only sharee could download or
// parse whatever anyone uploaded into the account, given the id (a hash
// of the content). Now each upload belongs to its uploader.
func TestBlobsBelongToUploader(t *testing.T) {
	e, _, _, _ := sharedCalendar(t)
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:p\r\nDTSTART:20260101T000000Z\r\nSUMMARY:salary talk\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	upload := func(user string) string {
		resp := e.do(user, "POST", "/jmap/upload/"+e.bob+"/", "text/calendar", []byte(ics))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s upload: %d", user, resp.StatusCode)
		}
		return decode[uploadResponse](t, resp).BlobID
	}
	parsed := func(user, id string) bool {
		r := e.api(user, call("CalendarEvent/parse", map[string]any{"accountId": e.bob, "blobIds": []string{id}}, "0"))[0]
		p, _ := r.Args["parsed"].(map[string]any)
		return p[id] != nil
	}
	download := func(user, id string) int {
		resp := e.do(user, "GET", "/jmap/download/"+e.bob+"/"+id+"/x.ics", "", nil)
		resp.Body.Close()
		return resp.StatusCode
	}
	id := upload("bob")
	if download("bob", id) != http.StatusOK || !parsed("bob", id) {
		t.Fatal("the uploader can't use the blob")
	}
	for _, other := range []string{"alice", "carol"} {
		if st := download(other, id); st != http.StatusNotFound {
			t.Errorf("%s downloaded bob's upload: %d", other, st)
		}
		if parsed(other, id) {
			t.Errorf("%s parsed bob's upload", other)
		}
	}
	if id := upload("carol"); download("carol", id) != http.StatusOK || !parsed("carol", id) {
		t.Error("a read-write sharee can't use their own upload")
	}
}

// A move between two of the owner's private calendars made a sharee's
// /changes fail ("objects moved"), telling them about activity they
// can't see. Now moves are judged by where the object was and is.
func TestChangesHideMovesOutOfSight(t *testing.T) {
	e, team, _, _ := sharedCalendar(t)
	p1, _ := e.st.CreateCollection("bob", store.Calendars, "p1", store.Meta{})
	p2, _ := e.st.CreateCollection("bob", store.Calendars, "p2", store.Meta{})
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:%s\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	p1.Put("x.ics", []byte(fmt.Sprintf(ics, "x")), store.Cond{})
	p1.Put("y.ics", []byte(fmt.Sprintf(ics, "y")), store.Cond{})
	log, _ := e.st.Account("bob", store.Calendars)
	y, _ := log.ObjectID(p1, "y.ics")
	state := func() string {
		r := e.api("alice", call("CalendarEvent/get", map[string]any{"accountId": e.bob, "ids": []string{}}, "0"))[0]
		return r.Args["state"].(string)
	}
	changes := func(since string) apiResp {
		return e.api("alice", call("CalendarEvent/changes", map[string]any{"accountId": e.bob, "sinceState": since}, "0"))[0]
	}
	s0 := state()
	if _, err := p1.Move("x.ics", p2, "x.ics", store.Cond{}); err != nil {
		t.Fatal(err)
	}
	if r := changes(s0); errType(r) != "" || len(ids2list(r.Args["created"]))+len(ids2list(r.Args["updated"]))+len(ids2list(r.Args["destroyed"])) != 0 {
		t.Fatalf("private move: %.300v", r)
	}
	s1 := state()
	if _, err := p1.Move("y.ics", team, "y.ics", store.Cond{}); err != nil {
		t.Fatal(err)
	}
	if r := changes(s1); errType(r) != "" || !slices.Equal(ids2list(r.Args["created"]), []string{y}) {
		t.Fatalf("move into sight: %.300v", r)
	}
	s2 := state()
	if _, err := team.Move("y.ics", p2, "y.ics", store.Cond{}); err != nil {
		t.Fatal(err)
	}
	if r := changes(s2); errType(r) != "" || !slices.Equal(ids2list(r.Args["destroyed"]), []string{y}) {
		t.Fatalf("move out of sight: %.300v", r)
	}
	// The state tells the same: unchanged by the private move, changed
	// when an event left the shared calendar.
	if s1 != s0 {
		t.Error("a private move changed the sharee's state")
	}
	if state() == s2 {
		t.Error("a move out of the shared calendar left the sharee's state as it was")
	}
}

// Uploads are kept for a day; without a cap per user they could fill the
// disk.
func TestPendingUploadsCapped(t *testing.T) {
	defer func(n int64) { maxPendingUploads = n }(maxPendingUploads)
	maxPendingUploads = 1000
	e := newEnv(t)
	up := func(n int) int {
		resp := e.do("alice", "POST", "/jmap/upload/"+e.alice+"/", "application/octet-stream", []byte(strings.Repeat("x", n)+strconv.Itoa(n)))
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := up(600); st != http.StatusCreated {
		t.Fatal(st)
	}
	if st := up(601); st != http.StatusRequestEntityTooLarge {
		t.Fatalf("past the cap: %d", st)
	}
	if st := up(300); st != http.StatusCreated {
		t.Fatalf("within the cap: %d", st)
	}
}
