package jmap

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/rrule"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// Filter conditions and sort properties of CalendarEvent/query.
var (
	eventConds = map[string]bool{
		"inCalendar": true, "after": true, "before": true, "text": true, "title": true,
		"description": true, "location": true, "owner": true, "participant": true, "uid": true,
	}
	eventSorts = map[string]bool{"start": true, "uid": true, "id": true, "created": true, "updated": true, "recurrenceId": true}
)

type eventQueryArgs struct {
	QueryArgs
	ExpandRecurrences bool   `json:"expandRecurrences"`
	TimeZone          string `json:"timeZone"`
}

// hit is one query result: an event or, when expanding, one occurrence.
type hit struct {
	id    string
	obj   map[string]any
	start time.Time
}

func (c *Call) eventQuery(raw json.RawMessage) (any, error) {
	var args eventQueryArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	floating, err := zoneArg(args.TimeZone)
	if err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, store.Calendars)
	if err != nil {
		return nil, err
	}
	f, err := parseFilter(args.Filter, eventConds)
	if err != nil {
		return nil, err
	}
	sorts, err := parseSort(args.Sort, eventSorts)
	if err != nil {
		return nil, err
	}
	// The time window: from a plain condition, or the conditions of an
	// expanding query (which must be one condition with both bounds).
	var from, to time.Time
	window := func(cond map[string]json.RawMessage) error {
		for k, t := range map[string]*time.Time{"after": &from, "before": &to} {
			if raw, ok := cond[k]; ok {
				var s string
				json.Unmarshal(raw, &s)
				lt, err := time.ParseInLocation(localFmt, s, floating)
				if err != nil {
					return Errorf(ErrInvalidArguments, "filter %s: not a LocalDateTime", k)
				}
				*t = lt.UTC()
			}
		}
		return nil
	}
	if args.ExpandRecurrences {
		if f == nil || f.op != "" {
			return nil, Errorf(ErrInvalidArguments, "expandRecurrences needs a single filter condition")
		}
		if err := window(f.cond); err != nil {
			return nil, err
		}
		if from.IsZero() || to.IsZero() {
			return nil, Errorf(ErrInvalidArguments, "expandRecurrences needs after and before")
		}
		if to.Sub(from) > maxExpanded {
			return nil, Errorf(ErrInvalidArguments, "the window is longer than %s", maxExpandedISO)
		}
	}
	state := v.State(store.LevelObjects)
	var hits []hit
	budget := expandBudget{n: maxOccurrences, bytes: maxOccurrenceBytes}
	for _, e := range c.h.all(eventType, v) {
		if args.ExpandRecurrences {
			occs, err := c.h.occurrences(c.ctx, v, e, from, to, floating, &budget)
			if err != nil {
				return nil, err
			}
			for _, h := range occs {
				ok, err := f.eval(func(cond map[string]json.RawMessage) (bool, error) {
					return c.matchEvent(h.obj, e.ref.Coll, cond, floating, nil)
				})
				if err != nil {
					return nil, err
				}
				if ok {
					hits = append(hits, h)
				}
			}
			continue
		}
		ok, err := f.eval(func(cond map[string]json.RawMessage) (bool, error) {
			return c.matchEvent(e.obj, e.ref.Coll, cond, floating, func(after, before time.Time) bool {
				return c.h.overlaps(c.ctx, v, e, after, before, floating)
			})
		})
		if err != nil {
			return nil, err
		}
		if ok {
			start, _ := toUTC(str(e.obj["start"]), e.obj, floating)
			hits = append(hits, hit{id: e.ref.ID, obj: e.obj, start: start})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return compareEvents(a, b, sorts) })
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	resp, err := Page(ids, args.QueryArgs, maxQueryLimit)
	if err != nil {
		return nil, err
	}
	resp.QueryState = state
	return resp, nil
}

// maxOccurrences and maxOccurrenceBytes bound what an expanding query
// materializes: every occurrence is a copy of its event, and a tiny
// FREQ=SECONDLY event has millions in the longest window.
const (
	maxOccurrences     = 50_000
	maxOccurrenceBytes = 64 << 20
)

// expandBudget is what an expanding query may still materialize: a
// number of occurrences and the bytes of their events.
type expandBudget struct{ n, bytes int }

// occurrences lists the instances of event e that overlap [from, to);
// zero bounds are open. A non-recurring event is one instance with its
// own id. It fails with requestTooLarge when b is used up.
func (h *Handler) occurrences(ctx context.Context, v *View, e *entry, from, to time.Time, floating *time.Location, b *expandBudget) ([]hit, error) {
	cal := h.rawCalendar(v, e.ref)
	if cal == nil {
		return nil, nil
	}
	_, recurring := e.obj["recurrenceRule"]
	recurring = recurring || len(overrides(e.obj)) > 0
	size := 0 // of e as JSON, what each instance copies
	if recurring {
		raw, _ := json.Marshal(e.obj)
		size = len(raw)
	}
	var out []hit
	for o := range rrule.ExpandContext(ctx, cal, "VEVENT", str(e.obj["uid"]), from, to, floating) {
		if b.n--; b.n < 0 {
			return nil, Errorf(ErrRequestTooLarge, "more than %d occurrences; narrow the window", maxOccurrences)
		}
		if !recurring {
			out = append(out, hit{id: e.ref.ID, obj: e.obj, start: o.Start})
			continue
		}
		if b.bytes -= size; b.bytes < 0 {
			return nil, Errorf(ErrRequestTooLarge, "the occurrences are larger than %d bytes; narrow the window", maxOccurrenceBytes)
		}
		rid := localRecurrenceID(o, e.obj, floating)
		inst, ok := instance(e.obj, rid)
		if !ok {
			continue
		}
		id := instanceID(e.ref.ID, rid)
		inst["id"] = id
		out = append(out, hit{id: id, obj: inst, start: o.Start})
	}
	return out, nil
}

// overlaps reports whether event e has an instance overlapping [from, to);
// zero bounds are open. Unlike occurrences it stops at the first one and
// copies nothing.
func (h *Handler) overlaps(ctx context.Context, v *View, e *entry, from, to time.Time, floating *time.Location) bool {
	cal := h.rawCalendar(v, e.ref)
	if cal == nil {
		return false
	}
	ro := overrides(e.obj)
	_, recurring := e.obj["recurrenceRule"]
	recurring = recurring || len(ro) > 0
	for o := range rrule.ExpandContext(ctx, cal, "VEVENT", str(e.obj["uid"]), from, to, floating) {
		if !recurring {
			return true
		}
		if p, _ := ro[localRecurrenceID(o, e.obj, floating)].(map[string]any); p["excluded"] != true {
			return true
		}
	}
	return false
}

// localRecurrenceID renders an occurrence's recurrence id in the event's
// time zone (as stored for all-day and floating events).
func localRecurrenceID(o rrule.Occurrence, e map[string]any, floating *time.Location) string {
	if o.AllDay || e["showWithoutTime"] == true {
		return o.RecurrenceID.In(floating).Format("2006-01-02") + "T00:00:00"
	}
	loc := floating
	if tz, ok := e["timeZone"].(string); ok {
		if l, err := rrule.LoadZone(tz); err == nil {
			loc = l
		}
	}
	return o.RecurrenceID.In(loc).Format(localFmt)
}

// matchEvent checks one FilterCondition. overlaps decides after/before
// for a whole series; nil means e is one instance and is checked as such.
func (c *Call) matchEvent(e map[string]any, coll string, cond map[string]json.RawMessage, loc *time.Location, overlaps func(after, before time.Time) bool) (bool, error) {
	var after, before time.Time
	for k, raw := range cond {
		var want string
		if err := json.Unmarshal(raw, &want); err != nil {
			return false, Errorf(ErrInvalidArguments, "filter %s: want a string", k)
		}
		switch k {
		case "after", "before":
			t, err := time.ParseInLocation(localFmt, want, loc)
			if err != nil {
				return false, Errorf(ErrInvalidArguments, "filter %s: not a LocalDateTime", k)
			}
			if k == "after" {
				after = t
			} else {
				before = t
			}
			continue
		case "inCalendar":
			if coll != want {
				return false, nil
			}
		case "uid":
			if e["uid"] != want {
				return false, nil
			}
		default:
			if !anyContains(eventTexts(e, k), want) {
				return false, nil
			}
		}
	}
	if (!after.IsZero() || !before.IsZero()) && overlaps != nil {
		// For expansion the window was applied when listing instances.
		return overlaps(asUTC(after), asUTC(before)), nil
	}
	return true, nil
}

// asUTC keeps a zero time zero.
func asUTC(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// eventTexts collects the strings a text condition searches.
func eventTexts(e map[string]any, cond string) []string {
	var out []string
	add := func(prop string) { collectStrings(e[prop], &out) }
	participants := func(ownerOnly bool) {
		ps, _ := e["participants"].(map[string]any)
		for _, p := range ps {
			pm, _ := p.(map[string]any)
			if roles, _ := pm["roles"].(map[string]any); ownerOnly && roles["owner"] != true {
				continue
			}
			for _, f := range []string{"name", "email", "calendarAddress"} {
				if s := str(pm[f]); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	switch cond {
	case "title":
		add("title")
	case "description":
		add("description")
	case "location":
		add("locations")
		add("virtualLocations")
	case "owner":
		participants(true)
	case "participant":
		participants(false)
	case "text":
		for _, p := range []string{"title", "description", "locations", "virtualLocations"} {
			add(p)
		}
		participants(false)
		kw, _ := e["keywords"].(map[string]any)
		for k := range kw {
			out = append(out, k)
		}
		// Text in overrides counts too: a query without expansion matches
		// a series if any instance matches.
		for _, patch := range overrides(e) {
			collectStrings(patch, &out)
		}
	}
	return out
}

// compareEvents orders by the sort criteria, then by start and id.
func compareEvents(a, b hit, sorts []comparator) int {
	for _, s := range sorts {
		var d int
		switch s.Property {
		case "start":
			d = a.start.Compare(b.start)
		case "id":
			d = strings.Compare(a.id, b.id)
		default:
			d = strings.Compare(strings.ToLower(str(a.obj[s.Property])), strings.ToLower(str(b.obj[s.Property])))
		}
		if d != 0 {
			if !s.ascending() {
				d = -d
			}
			return d
		}
	}
	if d := a.start.Compare(b.start); d != 0 {
		return d
	}
	return strings.Compare(a.id, b.id)
}
