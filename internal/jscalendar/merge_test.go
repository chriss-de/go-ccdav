package jscalendar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/ical"
)

func parseFile(t testing.TB, path string) *ical.Component {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ical.ParseBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// comparable drops what Merge maintains itself, and compares overrides
// by the instance they produce (a patch can be written several ways).
func comparable(m map[string]any) map[string]any {
	m = normalize(m)
	if ro := overridesOf(m); ro != nil {
		inst := map[string]any{}
		for k := range ro {
			i := Instance(m, k)
			if i != nil {
				for _, drop := range []string{"updated", "sequence", "created"} {
					delete(i, drop)
				}
			}
			inst[k] = untyped(i)
		}
		m["recurrenceOverrides"] = inst
	}
	for _, k := range []string{"updated", "sequence", "created"} {
		delete(m, k)
	}
	return untyped(m).(map[string]any)
}

func TestMergeUnchanged(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.ics")
	for _, f := range files {
		prev := parseFile(t, f)
		ev, _ := FromICal(prev)
		out, changed, err := Merge(prev, ev)
		if err != nil || changed || out != prev {
			t.Errorf("%s: changed=%v err=%v", f, changed, err)
		}
	}
}

func set(e map[string]any, path string, v any) { applyPointer(e, path, normalizeAny(v)) }

func entries(e map[string]any, prop string) map[string]any {
	m, _ := e[prop].(map[string]any)
	if m == nil {
		m = map[string]any{}
		e[prop] = m
	}
	return m
}

func firstOverrideKey(e map[string]any, patch bool) string {
	for k, v := range overridesOf(e) {
		if isPatch(v.(map[string]any)) == patch {
			return k
		}
	}
	return ""
}

// mods are changes a JMAP client could make; each must survive the way
// into iCalendar and back, for every test file.
var mods = map[string]func(e map[string]any){
	"title":       func(e map[string]any) { e["title"] = "New, title; with \\ special\nchars" },
	"description": func(e map[string]any) { e["description"] = "Line 1\nLine 2" },
	"move start": func(e map[string]any) {
		if e["showWithoutTime"] == true {
			e["start"] = "2026-12-24T00:00:00"
		} else {
			e["start"] = "2026-12-24T18:30:00"
		}
	},
	"other zone": func(e map[string]any) { e["timeZone"] = "America/New_York"; delete(e, "showWithoutTime") },
	"to all-day": func(e map[string]any) {
		e["showWithoutTime"] = true
		e["start"] = "2026-12-24T00:00:00"
		e["duration"] = "P2D"
		delete(e, "timeZone")
		delete(e, "endTimeZone")
		delete(e, "recurrenceOverrides") // their times are no longer valid
		if r, ok := e["recurrenceRule"].(map[string]any); ok && r["until"] != nil {
			r["until"] = str(r["until"])[:10] + "T00:00:00" // all-day: a date
		}
	},
	"to timed": func(e map[string]any) {
		delete(e, "showWithoutTime")
		e["start"] = "2026-12-24T09:15:00"
		e["timeZone"] = "Europe/Berlin"
		e["duration"] = "PT1H45M"
	},
	"duration": func(e map[string]any) {
		if e["showWithoutTime"] == true {
			e["duration"] = "P2D"
		} else {
			e["duration"] = "PT3H"
		}
	},
	"participants": func(e map[string]any) {
		e["organizerCalendarAddress"] = "mailto:org@example.com"
		e["participants"] = map[string]any{
			"o": map[string]any{"calendarAddress": "mailto:org@example.com", "email": "org@example.com", "name": "Org", "roles": map[string]any{"owner": true}},
			"a": map[string]any{"calendarAddress": "mailto:a@example.com", "email": "a@example.com", "roles": map[string]any{"required": true},
				"participationStatus": "accepted", "expectReply": true, "kind": "individual"},
			"b": map[string]any{"calendarAddress": "mailto:b@example.com", "email": "b@example.com", "roles": map[string]any{"optional": true, "chair": true},
				"description": "jmap only"},
		}
	},
	"alerts": func(e map[string]any) {
		e["alerts"] = map[string]any{
			"x1": map[string]any{"trigger": map[string]any{"@type": "OffsetTrigger", "offset": "-PT10M"}},
			"x2": map[string]any{"trigger": map[string]any{"@type": "AbsoluteTrigger", "when": "2026-12-24T08:00:00Z"}, "action": "email", "acknowledged": "2026-12-24T08:01:00Z"},
		}
	},
	"no alerts": func(e map[string]any) { delete(e, "alerts") },
	"rule": func(e map[string]any) {
		until := "2027-06-30T23:00:00"
		if e["showWithoutTime"] == true {
			until = "2027-06-30T00:00:00"
		}
		e["recurrenceRule"] = map[string]any{"frequency": "weekly", "interval": 2, "byDay": []any{map[string]any{"day": "tu"}, map[string]any{"day": "th"}}, "until": until}
	},
	"no rule": func(e map[string]any) { delete(e, "recurrenceRule"); delete(e, "recurrenceOverrides") },
	"exclude and add": func(e map[string]any) {
		ro := entries(e, "recurrenceOverrides")
		t, d := "T10:00:00", "PT2H"
		if e["showWithoutTime"] == true {
			t, d = "T00:00:00", "P2D"
		}
		ro["2027-01-05"+t] = map[string]any{"excluded": true}
		ro["2027-01-09"+t] = map[string]any{}
		ro["2027-01-11"+t] = map[string]any{"duration": d}
	},
	"patch an instance": func(e map[string]any) {
		key, start := "2027-02-02T10:00:00", "2027-02-02T11:00:00"
		if e["showWithoutTime"] == true {
			key, start = "2027-02-02T00:00:00", "2027-02-03T00:00:00"
		}
		entries(e, "recurrenceOverrides")[key] = map[string]any{"title": "Special", "start": start, "keywords": map[string]any{"vip": true}}
	},
	"change an existing override": func(e map[string]any) {
		if k := firstOverrideKey(e, true); k != "" {
			p := overridesOf(e)[k].(map[string]any)
			p["title"] = "Changed override"
		}
	},
	"drop overrides": func(e map[string]any) { delete(e, "recurrenceOverrides") },
	"properties": func(e map[string]any) {
		e["keywords"] = map[string]any{"a,b": true, "c": true}
		e["privacy"] = "secret"
		e["freeBusyStatus"] = "free"
		e["status"] = "cancelled"
		e["priority"] = 5
		e["color"] = "blue"
	},
	"places and links": func(e map[string]any) {
		e["locations"] = map[string]any{"l1": map[string]any{"name": "Hall", "coordinates": "geo:1.5,2.5"}, "l2": map[string]any{"name": "Annex", "locationTypes": map[string]any{"office": true}}}
		e["virtualLocations"] = map[string]any{"v1": map[string]any{"uri": "https://meet.example/x", "name": "Call", "features": map[string]any{"video": true}}}
		e["links"] = map[string]any{"k1": map[string]any{"href": "https://x.example/doc", "rel": "describedby"}, "k2": map[string]any{"href": "data:text/plain;base64,aGk=", "contentType": "text/plain", "title": "hi"}}
	},
	"related": func(e map[string]any) {
		e["relatedTo"] = map[string]any{"other@example.com": map[string]any{"relation": map[string]any{"next": true}}}
	},
	"JSCalendar only": func(e map[string]any) {
		e["localizations"] = map[string]any{"de": map[string]any{"title": "Titel"}}
		e["example.com:custom"] = []any{"a", 1.0}
		e["useDefaultAlerts"] = true
	},
	"iCalProps": func(e map[string]any) {
		e["iCalProps"] = []any{[]any{"x-foo", map[string]any{"x-p": "1"}, "unknown", `raw\;value`}}
	},
}

func TestMergeRoundTrip(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.ics")
	for _, f := range files {
		for name, mod := range mods {
			t.Run(filepath.Base(f)+"/"+name, func(t *testing.T) {
				prev := parseFile(t, f)
				ev, _ := FromICal(prev)
				want := normalize(ev)
				mod(want)
				out, changed, err := Merge(prev, want)
				if err != nil {
					t.Fatal(err)
				}
				if !changed {
					return
				}
				raw := out.Bytes()
				back, err := ical.ParseBytes(raw)
				if err != nil {
					t.Fatalf("output doesn't parse: %v\n%s", err, raw)
				}
				if _, _, err := caldav.Validate(back, nil); err != nil {
					t.Fatalf("invalid for CalDAV: %v\n%s", err, raw)
				}
				got, _ := FromICal(back)
				if !reflect.DeepEqual(comparable(got), comparable(want)) {
					g, _ := json.MarshalIndent(comparable(got), "", " ")
					w, _ := json.MarshalIndent(comparable(want), "", " ")
					t.Fatalf("round trip differs\niCalendar:\n%s\ngot:\n%s\nwant:\n%s", raw, g, w)
				}
				if _, changed, _ := Merge(back, got); changed {
					t.Fatalf("second merge changed something:\n%s", raw)
				}
			})
		}
	}
}

func TestMergeKeepsOtherLines(t *testing.T) {
	raw, _ := os.ReadFile("testdata/meeting.ics")
	prev := parseFile(t, "testdata/meeting.ics")
	ev, _ := FromICal(prev)
	e := normalize(ev)
	set(e, "participants/p2/participationStatus", "accepted")
	out, _, err := Merge(prev, e)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out.Bytes())
	for _, l := range strings.Split(strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n")), "\n") {
		if strings.HasPrefix(l, "ATTENDEE") || strings.HasPrefix(l, "ORGANIZER") || strings.HasPrefix(l, "DTSTAMP") ||
			strings.HasPrefix(l, "LAST-MODIFIED") || strings.HasPrefix(l, " ") {
			continue
		}
		if !strings.Contains(got, l) {
			t.Errorf("line %q lost or changed:\n%s", l, got)
		}
	}
	back, _ := ical.ParseBytes(out.Bytes())
	ev2 := back.ChildrenNamed("VEVENT")[0]
	for _, a := range ev2.PropsNamed("ATTENDEE") {
		switch a.Value {
		case "mailto:bob@example.com":
			if a.Param("PARTSTAT") != "ACCEPTED" || a.Param("RSVP") != "TRUE" || a.Param("JSID") != "p2" || a.Param("CN") != "Bob" {
				t.Errorf("bob: %+v", a)
			}
		case "mailto:carol@example.com":
			if a.Param("JSID") != "p3" || a.Param("DELEGATED-FROM") != "mailto:eve@example.com" {
				t.Errorf("carol not pinned or changed: %+v", a)
			}
		}
	}
}

func TestMergeNewEvent(t *testing.T) {
	e := map[string]any{"uid": "n1", "title": "Lunch", "start": "2026-11-02T12:30:00", "timeZone": "Europe/Berlin", "duration": "PT1H",
		"alerts": map[string]any{"a": map[string]any{"trigger": map[string]any{"offset": "-PT5M"}}}}
	out, changed, err := Merge(nil, e)
	if err != nil || !changed {
		t.Fatal(err)
	}
	got := string(out.Bytes())
	for _, want := range []string{"PRODID:-//ccdav//JMAP//EN", "BEGIN:VTIMEZONE", "TZID:Europe/Berlin", "DTSTART;TZID=Europe/Berlin:20261102T123000",
		"DTEND;TZID=Europe/Berlin:20261102T133000", "SUMMARY:Lunch", "BEGIN:VALARM", "UID:a", "TRIGGER:-PT5M", "CREATED:", "DTSTAMP:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if _, _, err := caldav.Validate(out, nil); err != nil {
		t.Fatal(err, got)
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]any{"start": "tomorrow", "timeZone": "Mars/Olympus", "duration": "-PT1H", "status": "maybe",
		"recurrenceOverrides": map[string]any{"2026-01-01T00:00:00": map[string]any{"excluded": true, "title": "x"}},
		"alerts":              map[string]any{"a": map[string]any{"trigger": map[string]any{"offset": "soon"}}}}
	got := strings.Join(Validate(bad), " ")
	for _, want := range []string{"start", "timeZone", "duration", "status", "recurrenceOverrides/2026-01-01T00:00:00", "alerts/a/trigger"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not reported: %s", want, got)
		}
	}
	if _, _, err := Merge(nil, bad); err == nil {
		t.Fatal("merged an invalid event")
	}
}

func TestMergeSequence(t *testing.T) {
	out, _, _ := Merge(nil, map[string]any{"uid": "s", "start": "2026-11-02T10:00:00"})
	if strings.Contains(string(out.Bytes()), "SEQUENCE") {
		t.Fatalf("new event has a SEQUENCE:\n%s", out.Bytes())
	}
	ev, _ := FromICal(out)
	e := normalize(ev)
	e["start"] = "2026-11-03T10:00:00"
	out, _, _ = Merge(out, e)
	if !strings.Contains(string(out.Bytes()), "SEQUENCE:1") {
		t.Fatalf("moved event not bumped:\n%s", out.Bytes())
	}
}
