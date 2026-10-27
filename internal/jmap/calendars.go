package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/jscalendar"
	"github.com/chriss-de/go-ccdav/internal/rrule"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// JMAP for Calendars (draft-ietf-jmap-calendars).
const (
	CapCalendars    = "urn:ietf:params:jmap:calendars"
	CapAvailability = "urn:ietf:params:jmap:principals:availability"
)

var eventType = &typeDef{name: "CalendarEvent", kind: store.Calendars, codec: jscalendar.Codec{}, collProp: "calendarIds", comp: "VEVENT"}

// maxExpanded bounds the time range of an expanding query and of an
// availability request.
const (
	maxExpanded    = 366 * 24 * time.Hour
	maxExpandedISO = "P366D"
)

// hasEvents reports whether a calendar collection takes events: task
// lists and journals (Apple Reminders) aren't JMAP calendars.
func hasEvents(col *store.Collection) bool {
	comps := col.Meta().Components
	return len(comps) == 0 || slices.Contains(comps, "VEVENT")
}

func (h *Handler) registerCalendars() {
	h.AddCapability(CapCalendars, struct{}{}, func(a account) any {
		return map[string]any{
			"maxCalendarsPerEvent": 1, "minDateTime": "1900-01-01T00:00:00Z", "maxDateTime": "2199-12-31T23:59:59Z",
			"maxExpandedQueryDuration": maxExpandedISO, "maxParticipantsPerEvent": nil, "mayCreateCalendar": a.personal,
		}
	})
	h.AddCapability(CapAvailability, struct{}{}, func(a account) any {
		if !a.personal {
			return nil
		}
		return map[string]any{"maxAvailabilityDuration": maxExpandedISO}
	})
	h.Register("Calendar/get", CapCalendars, func(c *Call, args json.RawMessage) (any, error) { return c.calendarGet(args) })
	h.Register("Calendar/changes", CapCalendars, func(c *Call, args json.RawMessage) (any, error) {
		return c.Changes(args, store.Calendars, store.LevelCollections)
	})
	h.Register("CalendarEvent/get", CapCalendars, func(c *Call, args json.RawMessage) (any, error) { return c.eventGet(args) })
	h.Register("CalendarEvent/changes", CapCalendars, func(c *Call, args json.RawMessage) (any, error) {
		return c.Changes(args, store.Calendars, store.LevelObjects)
	})
	h.AddCapability(CapCalendarsParse, struct{}{}, func(account) any { return struct{}{} })
	h.Register("Calendar/set", CapCalendars, func(c *Call, args json.RawMessage) (any, error) { return c.calendarSet(args) })
	h.Register("CalendarEvent/set", CapCalendars, func(c *Call, args json.RawMessage) (any, error) { return c.eventSet(args) })
	h.Register("CalendarEvent/copy", CapCalendars, func(c *Call, args json.RawMessage) (any, error) { return c.eventCopy(args) })
	h.Register("CalendarEvent/parse", CapCalendarsParse, func(c *Call, args json.RawMessage) (any, error) { return c.eventParse(args) })
	h.Register("CalendarEvent/query", CapCalendars, func(c *Call, args json.RawMessage) (any, error) { return c.eventQuery(args) })
	h.Register("CalendarEvent/queryChanges", CapCalendars, func(*Call, json.RawMessage) (any, error) {
		return nil, Errorf(ErrCannotCalculateChanges, "query changes are not supported; query again")
	})
	h.Register("ParticipantIdentity/get", CapCalendars, func(c *Call, args json.RawMessage) (any, error) { return c.identityGet(args) })
	h.Register("ParticipantIdentity/changes", CapCalendars, func(c *Call, args json.RawMessage) (any, error) { return c.identityChanges(args) })
	h.Register("Principal/getAvailability", CapAvailability, func(c *Call, args json.RawMessage) (any, error) { return c.availability(args) })
}

func (c *Call) calendarGet(raw json.RawMessage) (any, error) {
	var args GetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := CheckGet(args); err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, store.Calendars)
	if err != nil {
		return nil, err
	}
	resp := &GetResponse{AccountID: args.AccountID, State: v.State(store.LevelCollections), List: []any{}, NotFound: []string{}}
	ids := slices.Sorted(maps.Keys(v.Colls))
	if args.IDs != nil {
		ids = *args.IDs
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		col := v.Colls[id]
		if col == nil {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		resp.List = append(resp.List, Select(c.calendar(v, col), args.Properties))
	}
	return resp, nil
}

// calendar is the Calendar object of a collection as the caller sees it.
func (c *Call) calendar(v *View, col *store.Collection) map[string]any {
	m := col.Meta()
	name, color, order, subscribed, visible := m.DisplayName, m.Color, m.Order, !m.Unsubscribed, !m.Invisible
	if name == "" {
		name = col.Name
	}
	if !v.Own {
		p := c.h.store.SharePrefs(c.User, col)
		if p.DisplayName != "" {
			name = p.DisplayName
		}
		if p.Color != "" {
			color = p.Color
		}
		if p.Order != "" {
			order = p.Order
		}
		subscribed, visible = !p.Hidden, !p.Invisible
	}
	sortOrder, err := strconv.Atoi(order)
	if err != nil || sortOrder < 0 {
		sortOrder = 0
	}
	id := col.ID()
	acc := v.Access[id]
	owner := acc == store.AccessOwner
	write := owner || acc == store.AccessWrite
	var sharing any
	if owner {
		sharing = shareWith(col)
	}
	return map[string]any{
		"id":                       id,
		"name":                     name,
		"description":              nilIfEmpty(m.Description),
		"color":                    nilIfEmpty(color),
		"sortOrder":                sortOrder,
		"isSubscribed":             subscribed,
		"isVisible":                visible,
		"isDefault":                col.Name == c.h.defaultName(store.Calendars),
		"includeInAvailability":    availabilityOf(m),
		"defaultAlertsWithTime":    rawJSON(m.DefaultAlertsWithTime),
		"defaultAlertsWithoutTime": rawJSON(m.DefaultAlertsWithoutTime),
		"timeZone":                 nilIfEmpty(calendarZone(m.Timezone)),
		"shareWith":                sharing,
		"myRights": map[string]any{
			"mayReadFreeBusy": true, "mayReadItems": true, "mayWriteAll": write, "mayWriteOwn": write,
			"mayUpdatePrivate": true, "mayRSVP": false, "mayShare": owner, "mayDelete": owner,
		},
	}
}

func availabilityOf(m store.Meta) string {
	if m.Availability == "" {
		return "all"
	}
	return m.Availability
}

// rawJSON decodes stored JSON, or gives nil.
func rawJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// calendarZone is the IANA name of a calendar's time zone (stored as a
// VCALENDAR with one VTIMEZONE), or "".
func calendarZone(tzCal string) string {
	if tzCal == "" {
		return ""
	}
	cal, err := ical.ParseBytes([]byte(tzCal))
	if err != nil {
		return ""
	}
	for _, vtz := range cal.ChildrenNamed("VTIMEZONE") {
		if loc, ok := rrule.LoadLocation(vtz.Value("TZID"), cal); ok {
			if _, err := time.LoadLocation(loc.String()); err == nil && loc.String() != "Local" {
				return loc.String()
			}
		}
	}
	return ""
}

// eventGetArgs are CalendarEvent/get's arguments.
type eventGetArgs struct {
	GetArgs
	RecurrenceOverridesBefore *string `json:"recurrenceOverridesBefore"`
	RecurrenceOverridesAfter  *string `json:"recurrenceOverridesAfter"`
	ReduceParticipants        bool    `json:"reduceParticipants"`
	TimeZone                  string  `json:"timeZone"`
}

func (c *Call) eventGet(raw json.RawMessage) (any, error) {
	var args eventGetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := CheckGet(args.GetArgs); err != nil {
		return nil, err
	}
	floating, err := zoneArg(args.TimeZone)
	if err != nil {
		return nil, err
	}
	var before, after time.Time
	for _, b := range []struct {
		v *string
		t *time.Time
	}{{args.RecurrenceOverridesBefore, &before}, {args.RecurrenceOverridesAfter, &after}} {
		if b.v != nil {
			if *b.t, err = time.Parse(time.RFC3339, *b.v); err != nil {
				return nil, Errorf(ErrInvalidArguments, "%s is not a UTCDate", *b.v)
			}
		}
	}
	v, err := c.View(args.AccountID, store.Calendars)
	if err != nil {
		return nil, err
	}
	resp := &GetResponse{AccountID: args.AccountID, State: v.State(store.LevelObjects), List: []any{}, NotFound: []string{}}
	finish := func(e map[string]any) {
		c.eventMeta(e, floating)
		defaultAlerts(v, e)
		if args.RecurrenceOverridesBefore != nil || args.RecurrenceOverridesAfter != nil {
			limitOverrides(e, before, after, floating)
		}
		if args.ReduceParticipants {
			c.reduceParticipants(e)
		}
		resp.List = append(resp.List, Select(e, args.Properties))
	}
	if args.IDs == nil {
		es := c.h.all(eventType, v)
		if len(es) > MaxObjectsInGet {
			return nil, Errorf(ErrRequestTooLarge, "%d events; ask for ids", len(es))
		}
		for _, e := range es {
			finish(e.obj)
		}
		return resp, nil
	}
	seen := map[string]bool{}
	for _, id := range *args.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		e, ok := c.event(v, id)
		if !ok {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		finish(e)
	}
	return resp, nil
}

// event loads an event by id: a stored event, or an instance of a
// recurring one ("<id>_<recurrence id>", from an expanding query).
func (c *Call) event(v *View, id string) (map[string]any, bool) {
	id, ok := c.ResolveID(id)
	if !ok {
		return nil, false
	}
	base, rid, isInstance := strings.Cut(id, "_")
	ref, found := v.Log.Lookup(base)
	if !found {
		return nil, false
	}
	e, found := c.h.load(eventType, v, ref)
	if !found {
		return nil, false
	}
	if !isInstance {
		return e.obj, true
	}
	local, err := time.Parse("20060102T150405", rid)
	if err != nil {
		return nil, false
	}
	inst, ok := instance(e.obj, local.Format(localFmt))
	if !ok {
		return nil, false
	}
	// Only real occurrences: an override, or one the rule generates.
	if _, isOverride := overrides(e.obj)[local.Format(localFmt)]; !isOverride && !c.h.occurs(c.ctx, v, ref, e.obj, local.Format(localFmt)) {
		return nil, false
	}
	inst["id"] = id
	return inst, true
}

const localFmt = "2006-01-02T15:04:05"

func overrides(e map[string]any) map[string]any {
	ro, _ := e["recurrenceOverrides"].(map[string]any)
	return ro
}

// instanceID is the id of one occurrence of a recurring event.
func instanceID(base, recurrenceID string) string {
	t, _ := time.Parse(localFmt, recurrenceID)
	return base + "_" + t.Format("20060102T150405")
}

// instance builds the occurrence of e at recurrence id rid: the master
// without its recurrence, at rid, with the override patch applied.
// false if the occurrence is excluded.
func instance(e map[string]any, rid string) (map[string]any, bool) {
	inst := copyJSON(e)
	patch, _ := overrides(e)[rid].(map[string]any)
	if patch["excluded"] == true {
		return nil, false
	}
	delete(inst, "recurrenceRule")
	delete(inst, "recurrenceOverrides")
	inst["baseEventId"] = e["id"]
	inst["recurrenceId"] = rid
	if tz, ok := e["timeZone"].(string); ok {
		inst["recurrenceIdTimeZone"] = tz
	}
	inst["start"] = rid
	if len(patch) > 0 && ApplyPatch(inst, copyJSON(patch)) != nil {
		return nil, false
	}
	return inst, true
}

// occurs reports whether the stored event's rules generate an instance
// at the local recurrence id rid.
func (h *Handler) occurs(ctx context.Context, v *View, ref store.ObjectRef, e map[string]any, rid string) bool {
	at, ok := toUTC(rid, e, time.UTC)
	if !ok {
		return false
	}
	cal := h.rawCalendar(v, ref)
	if cal == nil {
		return false
	}
	for o := range rrule.ExpandContext(ctx, cal, "VEVENT", str(e["uid"]), at.Add(-time.Second), at.Add(time.Second), time.UTC) {
		if o.RecurrenceID.Equal(at) {
			return true
		}
	}
	return false
}

func (h *Handler) rawCalendar(v *View, ref store.ObjectRef) *ical.Component {
	col := v.Colls[ref.Coll]
	if col == nil {
		return nil
	}
	data, _, err := col.Get(ref.Name)
	if err != nil {
		return nil
	}
	cal, err := ical.ParseBytes(data)
	if err != nil {
		return nil
	}
	return cal
}

// toUTC resolves a LocalDateTime of event e (its time zone, or floating
// for events without one).
func toUTC(local string, e map[string]any, floating *time.Location) (time.Time, bool) {
	loc := floating
	if tz, ok := e["timeZone"].(string); ok {
		l, err := rrule.LoadZone(tz)
		if err != nil {
			return time.Time{}, false
		}
		loc = l
	}
	t, err := time.ParseInLocation(localFmt, local, loc)
	return t.UTC(), err == nil
}

// addDuration adds a JSCalendar duration: nominal days on the wall
// clock, then exact time.
func addDuration(t time.Time, loc *time.Location, dur string) time.Time {
	if dur == "" {
		return t
	}
	d, err := rrule.ParseDuration(dur)
	if err != nil {
		return t
	}
	sign := 1
	if d.Neg {
		sign = -1
	}
	return t.In(loc).AddDate(0, 0, sign*d.Days).Add(time.Duration(sign) * d.Clock)
}

// eventMeta adds CalendarEvent's server-set properties: isDraft,
// isOrigin, baseEventId, and utcStart/utcEnd for single events.
func (c *Call) eventMeta(e map[string]any, floating *time.Location) {
	e["isDraft"] = false
	if _, ok := e["baseEventId"]; !ok {
		e["baseEventId"] = nil
	}
	org, _ := e["organizerCalendarAddress"].(string)
	email := c.h.cfg.Get().Users[c.User].Email
	e["isOrigin"] = org == "" || email != "" && strings.EqualFold(strings.TrimPrefix(strings.ToLower(org), "mailto:"), strings.ToLower(email))
	if _, recurring := e["recurrenceRule"]; recurring || len(overrides(e)) > 0 {
		return
	}
	start, _ := e["start"].(string)
	if t, ok := toUTC(start, e, floating); ok {
		dur, _ := e["duration"].(string)
		loc := floating
		if tz, ok := e["timeZone"].(string); ok {
			loc, _ = rrule.LoadZone(tz)
		}
		e["utcStart"] = t.Format(time.RFC3339)
		e["utcEnd"] = addDuration(t, loc, dur).UTC().Format(time.RFC3339)
	}
}

// defaultAlerts puts the calendar's default alerts into an event that
// asks for them (useDefaultAlerts): those for events with or without a
// time, as fits.
func defaultAlerts(v *View, e map[string]any) {
	if e["useDefaultAlerts"] != true {
		return
	}
	for id := range e["calendarIds"].(map[string]any) {
		if col := v.Colls[id]; col != nil {
			m := col.Meta()
			raw := m.DefaultAlertsWithTime
			if e["showWithoutTime"] == true {
				raw = m.DefaultAlertsWithoutTime
			}
			if a := rawJSON(raw); a != nil {
				e["alerts"] = a
			} else {
				delete(e, "alerts")
			}
		}
	}
}

// limitOverrides drops recurrenceOverrides outside [after, before). The
// nested maps may belong to a cached conversion: replace, don't change.
func limitOverrides(e map[string]any, before, after time.Time, floating *time.Location) {
	ro := overrides(e)
	if ro == nil {
		return
	}
	kept := make(map[string]any, len(ro))
	for rid, o := range ro {
		t, ok := toUTC(rid, e, floating)
		if ok && (before.IsZero() || t.Before(before)) && (after.IsZero() || !t.Before(after)) {
			kept[rid] = o
		}
	}
	e["recurrenceOverrides"] = kept
}

// reduceParticipants keeps the owner and the caller. Like limitOverrides,
// it replaces the participants map instead of changing it.
func (c *Call) reduceParticipants(e map[string]any) {
	ps, _ := e["participants"].(map[string]any)
	if ps == nil {
		return
	}
	email := strings.ToLower(c.h.cfg.Get().Users[c.User].Email)
	kept := make(map[string]any, len(ps))
	for id, p := range ps {
		pm, _ := p.(map[string]any)
		roles, _ := pm["roles"].(map[string]any)
		mine := email != "" && strings.EqualFold(strings.TrimPrefix(strings.ToLower(str(pm["calendarAddress"])), "mailto:"), email)
		if roles["owner"] == true || mine {
			kept[id] = p
		}
	}
	e["participants"] = kept
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// zoneArg resolves a timeZone argument (default Etc/UTC).
func zoneArg(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := rrule.LoadZone(tz)
	if err != nil {
		return nil, Errorf(ErrInvalidArguments, "unknown time zone %q", tz)
	}
	return loc, nil
}

// attending reports whether the user (by email) takes part in an event:
// it has no attendees, they organize it, or they haven't declined.
func attending(cal *ical.Component, email string) bool {
	for _, ev := range cal.ChildrenNamed("VEVENT") {
		atts := ev.PropsNamed("ATTENDEE")
		if len(atts) == 0 {
			return true
		}
		if email == "" {
			return false
		}
		addr := "mailto:" + strings.ToLower(email)
		if o := ev.Prop("ORGANIZER"); o != nil && strings.ToLower(o.Value) == addr {
			return true
		}
		for _, a := range atts {
			if strings.ToLower(a.Value) == addr && !strings.EqualFold(a.Param("PARTSTAT"), "DECLINED") {
				return true
			}
		}
		return false
	}
	return false
}

// identityGet: one identity per user, from config.yaml's email.
func (c *Call) identityGet(raw json.RawMessage) (any, error) {
	var args GetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := c.principalsView(args.AccountID); err != nil {
		return nil, err
	}
	u := c.h.cfg.Get().Users[c.User]
	resp := &GetResponse{AccountID: args.AccountID, State: identityState(u.Name, u.Email), List: []any{}, NotFound: []string{}}
	var ids []string
	if u.Email != "" {
		ids = append(ids, "me")
	}
	want := ids
	if args.IDs != nil {
		want = *args.IDs
	}
	for _, id := range want {
		if id != "me" || u.Email == "" {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		name := u.Name
		if name == "" {
			name = c.User
		}
		resp.List = append(resp.List, Select(map[string]any{"id": "me", "name": name, "calendarAddress": "mailto:" + u.Email, "isDefault": true}, args.Properties))
	}
	return resp, nil
}

func identityState(name, email string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + email))
	return hex.EncodeToString(sum[:8])
}

func (c *Call) identityChanges(raw json.RawMessage) (any, error) {
	var args ChangesArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := c.principalsView(args.AccountID); err != nil {
		return nil, err
	}
	u := c.h.cfg.Get().Users[c.User]
	state := identityState(u.Name, u.Email)
	if args.SinceState != state {
		return nil, Errorf(ErrCannotCalculateChanges, "the identity changed")
	}
	return &ChangesResponse{AccountID: args.AccountID, OldState: state, NewState: state,
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}, nil
}

// availabilityArgs are Principal/getAvailability's arguments.
type availabilityArgs struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"`
	UTCStart  string `json:"utcStart"`
	UTCEnd    string `json:"utcEnd"`
}

// availability computes busy periods of a principal from the calendars
// of theirs the caller can see, as CalDAV's free-busy-query does.
func (c *Call) availability(raw json.RawMessage) (any, error) {
	var args availabilityArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := c.principalsView(args.AccountID); err != nil {
		return nil, err
	}
	from, err1 := time.Parse(time.RFC3339, args.UTCStart)
	to, err2 := time.Parse(time.RFC3339, args.UTCEnd)
	if err1 != nil || err2 != nil || !to.After(from) {
		return nil, Errorf(ErrInvalidArguments, "utcStart and utcEnd must be UTCDates, start before end")
	}
	if to.Sub(from) > maxExpanded {
		return nil, Errorf(ErrInvalidArguments, "longer than %s", maxExpandedISO)
	}
	user, _, ok := principalOf(c.h.cfg.Get(), args.ID)
	if !ok || user == "" {
		return nil, Errorf("notFound", "no such user principal")
	}
	var cals []*ical.Component
	if v, err := c.h.view(c.User, AccountID(user), store.Calendars); err == nil {
		email := c.h.cfg.Get().Users[user].Email
		for _, col := range v.Colls {
			mode := availabilityOf(col.Meta())
			if mode == "none" {
				continue
			}
			for _, info := range col.List() {
				if info.Comp != "VEVENT" {
					continue
				}
				if data, _, err := col.Get(info.Name); err == nil {
					if cal, err := ical.ParseBytes(data); err == nil && (mode == "all" || attending(cal, email)) {
						cals = append(cals, cal)
					}
				}
			}
		}
	}
	ps, err := caldav.FreeBusy(c.ctx, cals, from.UTC(), to.UTC(), time.UTC)
	if c.ctx.Err() != nil {
		return nil, c.ctx.Err() // run answers serverUnavailable
	}
	if err != nil {
		return nil, Errorf(ErrRequestTooLarge, "more than %d busy periods; ask for a shorter time", caldav.MaxFreeBusyPeriods)
	}
	list := []any{}
	for _, p := range ps {
		status := "confirmed"
		if p.Type == caldav.FBBusyTentative {
			status = "tentative"
		}
		list = append(list, map[string]any{"utcStart": p.Start.UTC().Format(time.RFC3339), "utcEnd": p.End.UTC().Format(time.RFC3339),
			"busyStatus": status, "event": nil})
	}
	return map[string]any{"list": list}, nil
}
