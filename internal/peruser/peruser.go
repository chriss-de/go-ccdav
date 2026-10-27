// Package peruser handles per-user event properties: in someone else's
// calendar, each user has their own alerts, color, keywords, free/busy
// status and default-alert choice (draft-ietf-jmap-calendars), plus the
// alarm bookkeeping clients write (Thunderbird's X-MOZ-LASTACK and
// X-MOZ-SNOOZE-TIME). The owner's values live in the calendar object;
// everyone else's in their overlay (store.Overlay) as Data, and until
// they set one they see the owner's. JMAP and CalDAV share it.
//
// Events are JSCalendar maps (package jscalendar).
package peruser

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// Props are the per-user Event properties.
var Props = []string{"keywords", "color", "freeBusyStatus", "useDefaultAlerts", "alerts"}

// Data is one event's per-user data as stored in an overlay: whole
// properties, patch paths per recurrence id, and per-user iCalendar
// lines (jCal arrays). A null property means "set to nothing" (e.g. no
// alerts), not "back to the owner's".
type Data struct {
	Props     map[string]any            `json:"props,omitempty"`
	Overrides map[string]map[string]any `json:"overrides,omitempty"`
	ICalProps []any                     `json:"iCalProps,omitempty"`
	HasICal   bool                      `json:"hasICal,omitempty"` // ICalProps is set (maybe to none)
}

// Empty reports whether d holds nothing (the user follows the owner).
func (d Data) Empty() bool { return len(d.Props) == 0 && len(d.Overrides) == 0 && !d.HasICal }

// IsPerUserPath reports whether a property or patch path is per-user.
func IsPerUserPath(path string) bool {
	top, _, _ := strings.Cut(path, "/")
	return slices.Contains(Props, top)
}

// isPerUserICal reports whether an iCalProps entry (a jCal array) is a
// client's alarm bookkeeping.
func isPerUserICal(entry any) bool {
	a, _ := entry.([]any)
	if len(a) == 0 {
		return false
	}
	name, _ := a[0].(string)
	name = strings.ToLower(name)
	return name == "x-moz-lastack" || strings.HasPrefix(name, "x-moz-snooze-time")
}

// splitICal separates an iCalProps list into per-user and other entries.
func splitICal(v any) (mine, rest []any) {
	l, _ := v.([]any)
	for _, e := range l {
		if isPerUserICal(e) {
			mine = append(mine, e)
		} else {
			rest = append(rest, e)
		}
	}
	return mine, rest
}

// Apply returns the event as the user sees it: their values for
// per-user properties, also in the overrides. e is not changed (it may
// be a cached conversion).
func Apply(e map[string]any, d Data) map[string]any {
	if d.Empty() {
		return e
	}
	out := maps.Clone(e)
	for k, val := range d.Props {
		if val == nil {
			delete(out, k)
		} else {
			out[k] = val
		}
	}
	if d.HasICal {
		_, rest := splitICal(e["iCalProps"])
		all := append(rest, d.ICalProps...)
		if len(all) > 0 {
			out["iCalProps"] = all
		} else {
			delete(out, "iCalProps")
		}
	}
	if len(d.Overrides) > 0 {
		ro, _ := e["recurrenceOverrides"].(map[string]any)
		ro = maps.Clone(ro)
		if ro == nil {
			ro = map[string]any{}
		}
		for rid, paths := range d.Overrides {
			p, _ := ro[rid].(map[string]any)
			if p["excluded"] == true {
				continue
			}
			p = maps.Clone(p)
			if p == nil {
				p = map[string]any{}
			}
			maps.Copy(p, paths)
			ro[rid] = p
		}
		out["recurrenceOverrides"] = ro
	}
	return out
}

// Split splits the event a user wants (next, in their view) into what
// goes into the calendar object (the owner's per-user values kept) and
// the user's per-user data (where they differ from the owner's).
func Split(next, owner map[string]any) (file map[string]any, d Data) {
	file = copyJSON(next)
	for _, k := range Props {
		nv, nok := next[k]
		ov, ook := owner[k]
		if nok != ook || !jsonEqual(nv, ov) {
			if d.Props == nil {
				d.Props = map[string]any{}
			}
			d.Props[k] = nv // nil if the user removed it
		}
		if ook {
			file[k] = ov
		} else {
			delete(file, k)
		}
	}
	nMine, nRest := splitICal(next["iCalProps"])
	oMine, _ := splitICal(owner["iCalProps"])
	if !jsonEqual(nMine, oMine) {
		d.ICalProps, d.HasICal = nMine, true
	}
	if all := append(nRest, oMine...); len(all) > 0 {
		file["iCalProps"] = copyAny(all)
	} else {
		delete(file, "iCalProps")
	}
	nRO, _ := next["recurrenceOverrides"].(map[string]any)
	oRO, _ := owner["recurrenceOverrides"].(map[string]any)
	fileRO := map[string]any{}
	for _, rid := range slices.Sorted(maps.Keys(nRO)) {
		np, _ := nRO[rid].(map[string]any)
		op, hadOwner := oRO[rid].(map[string]any)
		fp := map[string]any{}
		for path, val := range np {
			if !IsPerUserPath(path) {
				fp[path] = val
			} else if pv, ok := op[path]; !ok || !jsonEqual(pv, val) {
				if d.Overrides == nil {
					d.Overrides = map[string]map[string]any{}
				}
				if d.Overrides[rid] == nil {
					d.Overrides[rid] = map[string]any{}
				}
				d.Overrides[rid][path] = val
			}
		}
		for path, val := range op {
			if !IsPerUserPath(path) {
				continue
			}
			fp[path] = val // the owner's own per-user values stay
			if _, kept := np[path]; !kept {
				// Removed by the user: null, which in an override patch
				// removes the value for this occurrence.
				if d.Overrides == nil {
					d.Overrides = map[string]map[string]any{}
				}
				if d.Overrides[rid] == nil {
					d.Overrides[rid] = map[string]any{}
				}
				d.Overrides[rid][path] = nil
			}
		}
		// An entry only the user's values made would be an extra date
		// ({} adds an occurrence): the calendar object doesn't get one.
		if len(fp) > 0 || hadOwner {
			fileRO[rid] = fp
		}
	}
	if len(fileRO) > 0 {
		file["recurrenceOverrides"] = fileRO
	} else {
		delete(file, "recurrenceOverrides")
	}
	return file, d
}

// Decode reads stored per-user data; nil gives the empty Data.
func Decode(raw json.RawMessage) Data {
	var d Data
	if raw != nil {
		json.Unmarshal(raw, &d)
	}
	return d
}

// Encode is the stored form of d; nil when d is empty (follow the owner).
func Encode(d Data) json.RawMessage {
	if d.Empty() {
		return nil
	}
	b, _ := json.Marshal(d)
	return b
}

func copyJSON(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

func copyAny(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
