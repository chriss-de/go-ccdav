package jmap

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// Per-user properties (draft-ietf-jmap-calendars): in someone else's
// calendar, each user has their own value. The owner's live in the
// calendar object; everyone else's in their overlay (store.Overlay),
// and until they set one they see the owner's.
var perUserProps = []string{"keywords", "color", "freeBusyStatus", "useDefaultAlerts", "alerts"}

// overlayData is one event's per-user data as stored in an overlay:
// whole properties, and patch paths per recurrence id. A null value
// means "set to nothing" (e.g. no alerts), not "back to the owner's".
type overlayData struct {
	Props     map[string]any            `json:"props,omitempty"`
	Overrides map[string]map[string]any `json:"overrides,omitempty"`
}

func (d overlayData) empty() bool { return len(d.Props) == 0 && len(d.Overrides) == 0 }

// isPerUserPath reports whether a property or patch path is per-user.
func isPerUserPath(path string) bool {
	top, _, _ := strings.Cut(path, "/")
	return slices.Contains(perUserProps, top)
}

// overlayOf returns the caller's data for an event, if any.
func (v *View) overlayOf(id string) overlayData {
	var d overlayData
	if ev, ok := v.Overlay().Events[id]; ok && ev.Data != nil {
		json.Unmarshal(ev.Data, &d)
	}
	return d
}

// applyOverlay returns the event as the user sees it: their values for
// per-user properties, also in the overrides. e is not changed (it may
// be a cached conversion).
func applyOverlay(e map[string]any, d overlayData) map[string]any {
	if d.empty() {
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
	if len(d.Overrides) > 0 {
		ro := maps.Clone(overrides(e))
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

// splitPerUser splits the event a user wants (next, in their view) into
// what goes into the calendar object (the owner's per-user values kept)
// and the user's per-user data (where they differ from the owner's).
func splitPerUser(next, owner map[string]any) (file map[string]any, d overlayData) {
	file = copyJSON(next)
	for _, k := range perUserProps {
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
	nRO, oRO := overrides(next), overrides(owner)
	fileRO := map[string]any{}
	for _, rid := range slices.Sorted(maps.Keys(nRO)) {
		np, _ := nRO[rid].(map[string]any)
		op, hadOwner := oRO[rid].(map[string]any)
		fp := map[string]any{}
		for path, val := range np {
			if !isPerUserPath(path) {
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
			if isPerUserPath(path) {
				fp[path] = val // the owner's own per-user values stay
			}
		}
		// An entry only the user's values made would be an extra date
		// ({} adds an occurrence): the file doesn't get one.
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
