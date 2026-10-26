package jscalendar

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/rrule"
)

// Validate checks an event as far as Merge relies on it and returns the
// paths of invalid properties.
func Validate(e map[string]any) []string {
	var bad []string
	fail := func(p string) { bad = append(bad, p) }
	isStr := func(k string) bool { _, ok := e[k].(string); return ok }
	allDay := e["showWithoutTime"] == true
	midnight := func(s string) bool { return strings.HasSuffix(s, "T00:00:00") }
	if !isStr("start") {
		fail("start")
	} else if _, err := time.Parse(localFmt, str(e["start"])); err != nil || allDay && !midnight(str(e["start"])) {
		fail("start")
	}
	for _, k := range []string{"timeZone", "endTimeZone"} {
		if v, ok := e[k]; ok && v != nil {
			if !isStr(k) {
				fail(k)
			} else if _, err := time.LoadLocation(str(v)); err != nil {
				fail(k)
			}
		}
	}
	if v, ok := e["duration"]; ok {
		// All-day events last whole days: iCalendar has no other way.
		if d, err := rrule.ParseDuration(str(v)); err != nil || d.Neg || e["showWithoutTime"] == true && d.Clock != 0 {
			fail("duration")
		}
	}
	for k, allowed := range map[string][]string{
		"status": {"confirmed", "cancelled", "tentative"}, "freeBusyStatus": {"free", "busy"},
		"privacy": {"public", "private", "secret"},
	} {
		if v, ok := e[k]; ok && !slices.Contains(allowed, str(v)) {
			fail(k)
		}
	}
	for _, k := range []string{"uid", "title", "description", "color", "organizerCalendarAddress"} {
		if v, ok := e[k]; ok && v != nil && !isStr(k) {
			fail(k)
		}
	}
	if v, ok := e["showWithoutTime"]; ok {
		if _, isBool := v.(bool); !isBool {
			fail("showWithoutTime")
		}
	}
	for _, k := range []string{"sequence", "priority"} {
		if v, ok := e[k]; ok {
			if n, isNum := num(v); !isNum || n < 0 {
				fail(k)
			}
		}
	}
	if v, ok := e["keywords"]; ok && !isSet(v) {
		fail("keywords")
	}
	if r, ok := e["recurrenceRule"]; ok && r != nil {
		rm, isMap := r.(map[string]any)
		if !isMap || freqNamesUpper[str(rm["frequency"])] == "" {
			fail("recurrenceRule")
		} else if u, ok := rm["until"]; ok {
			if _, err := time.Parse(localFmt, str(u)); err != nil {
				fail("recurrenceRule/until")
			}
		}
	}
	if ro, ok := e["recurrenceOverrides"]; ok && ro != nil {
		rm, isMap := ro.(map[string]any)
		if !isMap {
			fail("recurrenceOverrides")
		}
		for _, k := range slices.Sorted(maps.Keys(rm)) {
			p, isObj := rm[k].(map[string]any)
			_, err := time.Parse(localFmt, k)
			if !isObj || err != nil || p["excluded"] == true && len(p) > 1 || !parentsExist(e, p) || allDay && !midnight(k) {
				fail("recurrenceOverrides/" + escapePointer(k))
			}
		}
	}
	for _, prop := range []string{"participants", "alerts", "locations", "virtualLocations", "links", "relatedTo"} {
		v, ok := e[prop]
		if !ok || v == nil {
			continue
		}
		m, isMap := v.(map[string]any)
		if !isMap {
			fail(prop)
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(m)) {
			path := prop + "/" + escapePointer(id)
			obj, isObj := m[id].(map[string]any)
			if !isObj || prop != "relatedTo" && !validID(id) {
				fail(path)
				continue
			}
			switch prop {
			case "alerts":
				trig, _ := obj["trigger"].(map[string]any)
				switch str(trig["@type"]) {
				case "AbsoluteTrigger":
					if _, err := time.Parse(time.RFC3339, str(trig["when"])); err != nil {
						fail(path + "/trigger")
					}
				case "OffsetTrigger", "":
					if _, err := rrule.ParseDuration(str(trig["offset"])); err != nil {
						fail(path + "/trigger")
					}
				}
			case "virtualLocations":
				if str(obj["uri"]) == "" {
					fail(path + "/uri")
				}
			case "links":
				if str(obj["href"]) == "" {
					fail(path + "/href")
				}
			case "participants":
				for _, k := range []string{"roles", "delegatedTo", "delegatedFrom", "memberOf"} {
					if v, ok := obj[k]; ok && !isSet(v) {
						fail(path + "/" + k)
					}
				}
			}
		}
	}
	return bad
}

// parentsExist reports whether every path of a patch has its parent in
// e (RFC 8620 PatchObject rules).
func parentsExist(e map[string]any, patch map[string]any) bool {
	for path := range patch {
		segs := strings.Split(path, "/")
		m := e
		for _, sg := range segs[:len(segs)-1] {
			next, ok := m[unescapePointer(sg)].(map[string]any)
			if !ok {
				return false
			}
			m = next
		}
	}
	return true
}

// isSet reports whether v is a JSON object whose values are all true.
func isSet(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, x := range m {
		if x != true {
			return false
		}
	}
	return true
}
