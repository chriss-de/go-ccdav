package jscalendar

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/rrule"
)

// ProdID is written into calendars that Merge creates.
const ProdID = "-//ccdav//JMAP//EN"

// serverProps are Event properties Merge never writes: JMAP metadata, and
// what Merge maintains itself (updated as DTSTAMP/LAST-MODIFIED).
var serverProps = map[string]bool{
	"@type": true, "version": true, "id": true, "calendarIds": true, "isDraft": true, "isOrigin": true,
	"baseEventId": true, "utcStart": true, "utcEnd": true, "updated": true, "method": true,
	"recurrenceId": true, "recurrenceIdTimeZone": true,
}

// timingProps are written together as DTSTART and DTEND/DURATION.
var timingProps = []string{"start", "timeZone", "endTimeZone", "duration", "showWithoutTime"}

// Merge writes event into the calendar object prev and returns the
// result. Only what differs from prev's own conversion is rewritten:
// lines and VALARMs of unchanged properties stay as they are (byte for
// byte, see ical), so whatever CalDAV clients stored survives JMAP
// edits. changed is false (and prev returned) if there is nothing to
// write. A nil prev creates a new calendar object.
//
// Overrides are written as EXDATE (excluded), RDATE (additions) or a
// VEVENT with RECURRENCE-ID (any other patch). What iCalendar can't hold
// goes into JSPROP lines, which the conversion reads back. Missing
// VTIMEZONEs are generated.
func Merge(prev *ical.Component, event map[string]any) (out *ical.Component, changed bool, err error) {
	if paths := Validate(event); len(paths) > 0 {
		return nil, false, &InvalidError{Paths: paths}
	}
	newE := normalize(event)
	if prev == nil {
		uid, _ := newE["uid"].(string)
		prev = &ical.Component{Name: "VCALENDAR", Props: []*ical.Prop{
			{Name: "VERSION", Value: "2.0"},
			{Name: "PRODID", Value: ical.EscapeText(ProdID)},
		}, Children: []*ical.Component{{Name: "VEVENT", Props: []*ical.Prop{{Name: "UID", Value: ical.EscapeText(uid)}}}}}
		if _, ok := newE["prodId"]; !ok {
			newE["prodId"] = ProdID
		}
		newE["created"] = time.Now().UTC().Format(time.RFC3339)
	}
	oldFull, err := FromICal(prev)
	if err != nil {
		return nil, false, err
	}
	oldE := normalize(oldFull)
	if sameEvent(oldE, newE) {
		return prev, false, nil
	}
	out = &ical.Component{Name: "VCALENDAR", Props: slices.Clone(prev.Props)}
	if !same(oldE["prodId"], newE["prodId"]) {
		setProp(out, &ical.Prop{Name: "PRODID", Value: ical.EscapeText(str(newE["prodId"]))})
	}
	master, overrides := split(prev)
	now := time.Now().UTC()
	var events []*ical.Component
	mOld, mMeta := convertEvent(master, prev)
	mOld = normalize(mOld)
	// The master: the event without overrides; RDATE/EXDATE come from them.
	m := newMerger(master, prev, mMeta)
	if err := m.merge(withoutOverrides(mOld), withoutOverrides(newE), true, now); err != nil {
		return nil, false, err
	}
	m.recurrences(overridesOf(oldE), overridesOf(newE), newE)
	events = append(events, m.result())

	// Overrides: one VEVENT per patch that isn't an exclusion or addition.
	newRO := overridesOf(newE)
	for _, key := range slices.Sorted(maps.Keys(newRO)) {
		patch, _ := newRO[key].(map[string]any)
		if !isPatch(patch) {
			continue
		}
		inst := Instance(newE, key)
		comp := overrides[key]
		if comp == nil {
			comp = &ical.Component{Name: "VEVENT", Props: []*ical.Prop{
				{Name: "UID", Value: ical.EscapeText(str(newE["uid"]))},
				recurrenceIDProp(key, newE),
			}}
		}
		oOld, oMeta := convertEvent(comp, prev)
		om := newMerger(comp, prev, oMeta)
		if err := om.merge(normalize(oOld), inst, false, now); err != nil {
			return nil, false, err
		}
		// The master's zone or all-day state may have changed.
		if rid := recurrenceIDProp(key, newE); comp.Prop("RECURRENCE-ID") == nil ||
			comp.Prop("RECURRENCE-ID").Value != rid.Value || comp.Prop("RECURRENCE-ID").Param("TZID") != rid.Param("TZID") {
			om.touch("@rid", "*", []*ical.Prop{rid})
		}
		events = append(events, om.result())
	}
	// Other children (VTIMEZONEs, components we don't know) stay.
	used := map[string]bool{}
	for _, ev := range events {
		for _, p := range ev.Props {
			if tz := p.Param("TZID"); tz != "" {
				used[tz] = true
			}
		}
	}
	have := map[string]bool{}
	for _, ch := range prev.Children {
		if ch.Name == "VEVENT" {
			continue
		}
		if ch.Name == "VTIMEZONE" {
			have[ch.Value("TZID")] = true
		}
		out.Children = append(out.Children, ch)
	}
	from := earliest(newE)
	for _, tz := range slices.Sorted(maps.Keys(used)) {
		if have[tz] {
			continue
		}
		// Zones Go doesn't know come from untouched lines (Mozilla's
		// "/mozilla.org/..." TZIDs, Windows names): left as they are.
		if vtz, err := rrule.VTimezone(tz, from); err == nil {
			out.Children = append(out.Children, vtz)
		}
	}
	out.Children = append(out.Children, events...)
	return out, true, nil
}

// InvalidError lists the event properties (as paths) that are invalid.
type InvalidError struct{ Paths []string }

func (e *InvalidError) Error() string {
	return "jscalendar: invalid properties " + strings.Join(e.Paths, ", ")
}

// Instance builds the occurrence of event e at recurrence id rid: the
// master without its recurrence, starting at rid, with the override
// patch applied (an "excluded" patch gives nil).
func Instance(e map[string]any, rid string) map[string]any {
	inst := normalize(e)
	patch, _ := overridesOf(e)[rid].(map[string]any)
	if patch["excluded"] == true {
		return nil
	}
	delete(inst, "recurrenceRule")
	delete(inst, "recurrenceOverrides")
	inst["recurrenceId"] = rid
	inst["start"] = rid
	for _, path := range slices.Sorted(maps.Keys(patch)) {
		applyPointer(inst, path, normalizeAny(patch[path]))
	}
	return inst
}

// applyPointer sets (or with nil, removes) the value at a patch path.
func applyPointer(m map[string]any, path string, v any) {
	segs := strings.Split(path, "/")
	for _, sg := range segs[:len(segs)-1] {
		next, ok := m[unescapePointer(sg)].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	last := unescapePointer(segs[len(segs)-1])
	if v == nil {
		delete(m, last)
	} else {
		m[last] = v
	}
}

func split(cal *ical.Component) (master *ical.Component, overrides map[string]*ical.Component) {
	overrides = map[string]*ical.Component{}
	events := cal.ChildrenNamed("VEVENT")
	uid := ""
	if len(events) > 0 {
		uid = events[0].Value("UID")
	}
	var masterE map[string]any
	for _, ev := range events {
		if ev.Value("UID") == uid && ev.Prop("RECURRENCE-ID") == nil && master == nil {
			master = ev
			masterE, _ = convertEvent(ev, cal)
		}
	}
	if master == nil { // only overrides: the first one is the event
		master = events[0]
		return master, overrides
	}
	for _, ev := range events {
		if ev.Value("UID") == uid && ev.Prop("RECURRENCE-ID") != nil {
			if key, ok := localValue(ev.Prop("RECURRENCE-ID"), cal, masterE); ok {
				overrides[key] = ev
			}
		}
	}
	return master, overrides
}

func overridesOf(e map[string]any) map[string]any {
	ro, _ := e["recurrenceOverrides"].(map[string]any)
	return ro
}

func withoutOverrides(e map[string]any) map[string]any {
	out := maps.Clone(e)
	delete(out, "recurrenceOverrides")
	return out
}

// isPatch reports whether an override needs a VEVENT of its own: not an
// exclusion and not a plain addition (which RDATE can hold).
func isPatch(p map[string]any) bool {
	if p["excluded"] == true {
		return false
	}
	for k := range p {
		if k != "duration" {
			return true
		}
	}
	return false
}

func sameEvent(a, b map[string]any) bool {
	for k := range union(a, b) {
		if !serverProps[k] && k != "created" && !same(a[k], b[k]) {
			return false
		}
	}
	return true
}

// earliest is the start of the event (VTIMEZONEs cover from its year).
func earliest(e map[string]any) time.Time {
	t, err := time.Parse(localFmt, str(e["start"]))
	if err != nil {
		return time.Now()
	}
	for k := range overridesOf(e) {
		if o, err := time.Parse(localFmt, k); err == nil && o.Before(t) {
			t = o
		}
	}
	return t
}

// merger rewrites one VEVENT.
type merger struct {
	comp   *ical.Component
	cal    *ical.Component
	meta   *eventMeta
	props  []*ical.Prop
	locs   []loc
	drop   []bool
	before map[int][]*ical.Prop
	tail   []*ical.Prop
	alarms []*ical.Component // VALARMs, nil where removed
	newAl  []*ical.Component
	others []*ical.Component // other children

	timingChanged bool // DTSTART changed: EXDATE/RDATE follow its form
}

// loc is where a VEVENT line went in the event: a property (or a group
// like "@timing") and, for participants, the entry id.
type loc struct{ prop, id string }

func newMerger(comp, cal *ical.Component, meta *eventMeta) *merger {
	m := &merger{comp: comp, cal: cal, meta: meta, props: slices.Clone(comp.Props), drop: make([]bool, len(comp.Props)), before: map[int][]*ical.Prop{}}
	for _, p := range comp.Props {
		m.locs = append(m.locs, m.origin(p))
	}
	for _, ch := range comp.Children {
		if ch.Name == "VALARM" {
			m.alarms = append(m.alarms, ch)
		} else {
			m.others = append(m.others, ch)
		}
	}
	return m
}

// origin maps a line to the event property it went into.
func (m *merger) origin(p *ical.Prop) loc {
	switch p.Name {
	case "UID":
		return loc{"uid", ""}
	case "SEQUENCE":
		return loc{"sequence", ""}
	case "CREATED":
		return loc{"created", ""}
	case "LAST-MODIFIED", "DTSTAMP":
		return loc{"updated", ""}
	case "SUMMARY":
		return loc{"title", ""}
	case "DESCRIPTION":
		return loc{"description", ""}
	case "STATUS":
		return loc{"status", ""}
	case "TRANSP":
		return loc{"freeBusyStatus", ""}
	case "CLASS":
		return loc{"privacy", ""}
	case "PRIORITY":
		return loc{"priority", ""}
	case "COLOR":
		return loc{"color", ""}
	case "CATEGORIES":
		return loc{"keywords", ""}
	case "DTSTART", "DTEND", "DURATION":
		return loc{"@timing", ""}
	case "RRULE":
		if p == m.comp.Prop("RRULE") {
			if _, err := rrule.Parse(p.Value); err == nil && !strings.Contains(strings.ToUpper(p.Value), "RSCALE") {
				return loc{"recurrenceRule", ""}
			}
		}
		return loc{"iCalProps", ""}
	case "RDATE":
		return loc{"@rdate", ""}
	case "EXDATE":
		return loc{"@exdate", ""}
	case "RECURRENCE-ID":
		return loc{"@rid", ""}
	case "LOCATION", "GEO":
		return loc{"locations", ""}
	case "CONFERENCE":
		return loc{"virtualLocations", ""}
	case "URL", "ATTACH":
		return loc{"links", ""}
	case "ORGANIZER", "ATTENDEE":
		return loc{"participants", m.meta.partID[strings.ToLower(p.Text())]}
	case "RELATED-TO":
		return loc{"relatedTo", ""}
	case "JSPROP":
		if ptr := p.Param("JSPTR"); ptr != "" {
			segs := strings.SplitN(ptr, "/", 3)
			l := loc{unescapePointer(segs[0]), ""}
			if (l.prop == "participants" || l.prop == "alerts") && len(segs) > 1 {
				l.id = unescapePointer(segs[1])
			}
			return l
		}
	}
	return loc{"iCalProps", ""}
}

// touch replaces the lines of l (all of l.prop when id is "*") with
// lines, at the position of the first of them.
func (m *merger) touch(prop, id string, lines []*ical.Prop) {
	at := -1
	for i, l := range m.locs {
		if l.prop == prop && (id == "*" || l.id == id) && !m.drop[i] {
			m.drop[i] = true
			if at < 0 {
				at = i
			}
		}
	}
	if at < 0 {
		m.tail = append(m.tail, lines...)
	} else {
		m.before[at] = append(m.before[at], lines...)
	}
}

func (m *merger) result() *ical.Component {
	out := &ical.Component{Name: "VEVENT"}
	for i, p := range m.props {
		out.Props = append(out.Props, m.before[i]...)
		if !m.drop[i] {
			out.Props = append(out.Props, p)
		}
	}
	out.Props = append(out.Props, m.tail...)
	for _, a := range m.alarms {
		if a != nil {
			out.Children = append(out.Children, a)
		}
	}
	out.Children = append(out.Children, m.newAl...)
	out.Children = append(out.Children, m.others...)
	return out
}

// merge writes the differences between oldE and newE (both without
// recurrenceOverrides). master also bumps SEQUENCE on changes clients
// must notice (time, rule, place).
func (m *merger) merge(oldE, newE map[string]any, master bool, now time.Time) error {
	groups := map[string]bool{}
	for k := range union(oldE, newE) {
		// prodId belongs to the VCALENDAR; an override keeps its CREATED.
		if serverProps[k] || k == "prodId" || !master && k == "created" || same(oldE[k], newE[k]) {
			continue
		}
		if slices.Contains(timingProps, k) {
			groups["@timing"] = true
		} else if k == "organizerCalendarAddress" {
			groups["participants"] = true
		} else {
			groups[k] = true
		}
	}
	if len(groups) == 0 {
		return nil
	}
	m.timingChanged = groups["@timing"]
	if groups["@timing"] && newE["recurrenceRule"] != nil {
		groups["recurrenceRule"] = true // UNTIL is written in the event's zone
	}
	existing := m.comp.Prop("DTSTART") != nil // a new event starts at 0
	if master && existing && (groups["@timing"] || groups["recurrenceRule"] || groups["locations"]) && same(oldE["sequence"], newE["sequence"]) {
		n, _ := num(oldE["sequence"])
		newE["sequence"] = n + 1
		groups["sequence"] = true
	}
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		if err := m.group(g, oldE, newE); err != nil {
			return err
		}
	}
	stamp := now.Format("20060102T150405Z")
	m.touch("updated", "*", []*ical.Prop{{Name: "DTSTAMP", Value: stamp}, {Name: "LAST-MODIFIED", Value: stamp}})
	if master && m.comp.Prop("CREATED") == nil && !groups["created"] {
		m.touch("created", "*", []*ical.Prop{{Name: "CREATED", Value: stamp}})
	}
	return nil
}

func (m *merger) group(g string, oldE, newE map[string]any) error {
	text := func(name string) []*ical.Prop {
		if s, ok := newE[g].(string); ok {
			return []*ical.Prop{{Name: name, Value: ical.EscapeText(s)}}
		}
		return nil
	}
	var lines []*ical.Prop
	switch g {
	case "uid":
		lines = text("UID")
	case "title":
		lines = text("SUMMARY")
	case "description":
		lines = text("DESCRIPTION")
	case "color":
		lines = text("COLOR")
	case "created":
		if t, err := time.Parse(time.RFC3339, str(newE[g])); err == nil {
			lines = []*ical.Prop{{Name: "CREATED", Value: t.UTC().Format("20060102T150405Z")}}
		}
	case "sequence", "priority":
		if n, ok := num(newE[g]); ok {
			lines = []*ical.Prop{{Name: strings.ToUpper(g), Value: strconv.Itoa(n)}}
		}
	case "status":
		if s := str(newE[g]); s != "" {
			lines = []*ical.Prop{{Name: "STATUS", Value: strings.ToUpper(s)}}
		}
	case "freeBusyStatus":
		switch str(newE[g]) {
		case "free":
			lines = []*ical.Prop{{Name: "TRANSP", Value: "TRANSPARENT"}}
		case "busy":
			lines = []*ical.Prop{{Name: "TRANSP", Value: "OPAQUE"}}
		}
	case "privacy":
		switch str(newE[g]) {
		case "private":
			lines = []*ical.Prop{{Name: "CLASS", Value: "PRIVATE"}}
		case "secret":
			lines = []*ical.Prop{{Name: "CLASS", Value: "CONFIDENTIAL"}}
		case "public":
			lines = []*ical.Prop{{Name: "CLASS", Value: "PUBLIC"}}
		}
	case "keywords":
		var esc []string
		for _, k := range keys(newE[g]) {
			esc = append(esc, ical.EscapeText(k))
		}
		if len(esc) > 0 {
			lines = []*ical.Prop{{Name: "CATEGORIES", Value: strings.Join(esc, ",")}}
		}
	case "@timing":
		lines = m.timing(newE)
	case "recurrenceRule":
		if r, ok := newE[g].(map[string]any); ok {
			lines = []*ical.Prop{{Name: "RRULE", Value: rruleValue(r, newE)}}
		}
	case "relatedTo":
		rel, _ := newE[g].(map[string]any)
		for _, key := range slices.Sorted(maps.Keys(rel)) {
			r, _ := rel[key].(map[string]any)
			p := &ical.Prop{Name: "RELATED-TO", Value: ical.EscapeText(key)}
			for _, t := range keys(r["relation"]) {
				if t != "parent" {
					p.Params = append(p.Params, ical.Param{Name: "RELTYPE", Values: []string{strings.ToUpper(t)}})
				}
			}
			lines = append(lines, p)
			lines = append(lines, m.rest("relatedTo/"+escapePointer(key), r, "relation")...)
		}
	case "locations":
		lines = m.locations(newE)
	case "virtualLocations":
		lines = m.virtualLocations(newE)
	case "links":
		lines = m.links(newE)
	case "participants":
		return m.participants(oldE, newE)
	case "alerts":
		m.alerts(oldE, newE)
		return nil
	case "iCalProps":
		lines = m.iCalProps(newE[g])
	default:
		if v, ok := newE[g]; ok && v != nil {
			lines = []*ical.Prop{jsprop(escapePointer(g), v)}
		}
	}
	m.touch(g, "*", lines)
	return nil
}

// timing writes DTSTART and DTEND (or DURATION, if the event used it).
func (m *merger) timing(e map[string]any) []*ical.Prop {
	start := str(e["start"])
	t, err := time.Parse(localFmt, start)
	if err != nil {
		return nil
	}
	allDay := e["showWithoutTime"] == true
	tz := str(e["timeZone"])
	dtstart := dateTimeProp("DTSTART", t, tz, allDay)
	lines := []*ical.Prop{dtstart}
	dur := str(e["duration"])
	if dur == "" {
		if e["showWithoutTime"] == true {
			dur = "P1D"
		} else {
			return lines
		}
	}
	d, err := rrule.ParseDuration(dur)
	if err != nil {
		return lines
	}
	if m.comp.Prop("DURATION") != nil {
		return append(lines, &ical.Prop{Name: "DURATION", Value: dur})
	}
	switch {
	case allDay:
		end := t.AddDate(0, 0, d.Days)
		return append(lines, dateTimeProp("DTEND", end, "", true))
	case tz == "":
		end := t.AddDate(0, 0, d.Days).Add(d.Clock)
		return append(lines, dateTimeProp("DTEND", end, "", false))
	default:
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return lines
		}
		local := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
		end := local.AddDate(0, 0, d.Days).Add(d.Clock)
		endTZ := tz
		if etz := str(e["endTimeZone"]); etz != "" {
			if eloc, err := time.LoadLocation(etz); err == nil {
				end, endTZ = end.In(eloc), etz
			}
		}
		return append(lines, dateTimeProp("DTEND", end.In(end.Location()), endTZ, false))
	}
}

// dateTimeProp writes a DATE, a UTC, a zoned or a floating date-time.
func dateTimeProp(name string, t time.Time, tz string, allDay bool) *ical.Prop {
	p := &ical.Prop{Name: name}
	switch {
	case allDay:
		p.Value = t.Format("20060102")
		p.Params = []ical.Param{{Name: "VALUE", Values: []string{"DATE"}}}
	case tz == "Etc/UTC" || tz == "UTC":
		p.Value = t.Format("20060102T150405") + "Z"
	case tz != "":
		p.Value = t.Format("20060102T150405")
		p.Params = []ical.Param{{Name: "TZID", Values: []string{tz}}}
	default:
		p.Value = t.Format("20060102T150405")
	}
	return p
}

// recurrenceIDProp writes the RECURRENCE-ID of an override, in the
// master's way of writing DTSTART.
func recurrenceIDProp(key string, e map[string]any) *ical.Prop {
	t, _ := time.Parse(localFmt, key)
	return dateTimeProp("RECURRENCE-ID", t, str(e["timeZone"]), e["showWithoutTime"] == true)
}

var freqNamesUpper = map[string]string{"yearly": "YEARLY", "monthly": "MONTHLY", "weekly": "WEEKLY", "daily": "DAILY", "hourly": "HOURLY", "minutely": "MINUTELY", "secondly": "SECONDLY"}

// rruleValue renders a RecurrenceRule. UNTIL is UTC for zoned events,
// a DATE for all-day ones and floating otherwise, as RFC 5545 asks.
func rruleValue(r map[string]any, e map[string]any) string {
	parts := []string{"FREQ=" + freqNamesUpper[str(r["frequency"])]}
	if rs := str(r["rscale"]); rs != "" && rs != "gregorian" {
		parts = append(parts, "RSCALE="+strings.ToUpper(rs))
		if sk := str(r["skip"]); sk != "" && sk != "omit" {
			parts = append(parts, "SKIP="+strings.ToUpper(sk))
		}
	}
	if n, ok := num(r["interval"]); ok && n > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(n))
	}
	if n, ok := num(r["count"]); ok {
		parts = append(parts, "COUNT="+strconv.Itoa(n))
	}
	if u := str(r["until"]); u != "" {
		if t, err := time.Parse(localFmt, u); err == nil {
			tz := str(e["timeZone"])
			switch {
			case e["showWithoutTime"] == true:
				parts = append(parts, "UNTIL="+t.Format("20060102"))
			case tz != "":
				if loc, err := time.LoadLocation(tz); err == nil {
					local := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
					parts = append(parts, "UNTIL="+local.UTC().Format("20060102T150405Z"))
				}
			default:
				parts = append(parts, "UNTIL="+t.Format("20060102T150405"))
			}
		}
	}
	if days, ok := r["byDay"].([]any); ok && len(days) > 0 {
		var ds []string
		for _, d := range days {
			dm, _ := d.(map[string]any)
			s := strings.ToUpper(str(dm["day"]))
			if n, ok := num(dm["nthOfPeriod"]); ok && n != 0 {
				s = strconv.Itoa(n) + s
			}
			ds = append(ds, s)
		}
		parts = append(parts, "BYDAY="+strings.Join(ds, ","))
	}
	for _, f := range []struct{ prop, part string }{
		{"byMonthDay", "BYMONTHDAY"}, {"byMonth", "BYMONTH"}, {"byYearDay", "BYYEARDAY"}, {"byWeekNo", "BYWEEKNO"},
		{"byHour", "BYHOUR"}, {"byMinute", "BYMINUTE"}, {"bySecond", "BYSECOND"}, {"bySetPosition", "BYSETPOS"},
	} {
		if l, ok := r[f.prop].([]any); ok && len(l) > 0 {
			var vs []string
			for _, v := range l {
				vs = append(vs, fmt.Sprint(v))
			}
			parts = append(parts, f.part+"="+strings.Join(vs, ","))
		}
	}
	if w := str(r["firstDayOfWeek"]); w != "" && w != "mo" {
		parts = append(parts, "WKST="+strings.ToUpper(w))
	}
	return strings.Join(parts, ";")
}

// recurrences writes RDATE and EXDATE from the overrides that are plain
// additions and exclusions, if those changed.
func (m *merger) recurrences(oldRO, newRO map[string]any, e map[string]any) {
	kind := func(ro map[string]any, want string) map[string]any {
		out := map[string]any{}
		for k, v := range ro {
			p, _ := v.(map[string]any)
			switch {
			case p["excluded"] == true && want == "ex":
				out[k] = true
			case p["excluded"] != true && !isPatch(p) && want == "add":
				out[k] = p["duration"]
			}
		}
		return out
	}
	timingChanged := m.timingChanged
	if !same(kind(oldRO, "ex"), kind(newRO, "ex")) || timingChanged {
		var lines []*ical.Prop
		if ex := slices.Sorted(maps.Keys(kind(newRO, "ex"))); len(ex) > 0 {
			lines = append(lines, dateList("EXDATE", ex, e))
		}
		m.touch("@exdate", "*", lines)
	}
	if !same(kind(oldRO, "add"), kind(newRO, "add")) || timingChanged {
		adds := kind(newRO, "add")
		var plain []string
		var lines []*ical.Prop
		for _, k := range slices.Sorted(maps.Keys(adds)) {
			if d := str(adds[k]); d != "" {
				p := dateList("RDATE", []string{k}, e)
				p.Value += "/" + d
				p.Params = append(p.Params, ical.Param{Name: "VALUE", Values: []string{"PERIOD"}})
				lines = append(lines, p)
			} else {
				plain = append(plain, k)
			}
		}
		if len(plain) > 0 {
			lines = append([]*ical.Prop{dateList("RDATE", plain, e)}, lines...)
		}
		m.touch("@rdate", "*", lines)
	}
}

// dateList writes several LocalDateTimes of event e as one EXDATE/RDATE.
func dateList(name string, locals []string, e map[string]any) *ical.Prop {
	var vals []string
	var p *ical.Prop
	for _, l := range locals {
		t, _ := time.Parse(localFmt, l)
		p = dateTimeProp(name, t, str(e["timeZone"]), e["showWithoutTime"] == true)
		vals = append(vals, p.Value)
	}
	p.Value = strings.Join(vals, ",")
	return p
}

func (m *merger) locations(e map[string]any) []*ical.Prop {
	ls, _ := e["locations"].(map[string]any)
	var lines []*ical.Prop
	geoDone := false
	for _, id := range slices.Sorted(maps.Keys(ls)) {
		l, _ := ls[id].(map[string]any)
		used := []string{"name"}
		if name := str(l["name"]); name != "" {
			lines = append(lines, &ical.Prop{Name: "LOCATION", Params: []ical.Param{{Name: "JSID", Values: []string{id}}}, Value: ical.EscapeText(name)})
		} else {
			used = nil
		}
		if c := str(l["coordinates"]); c != "" && !geoDone && (len(used) > 0 || len(ls) == 1) {
			if lat, lon, ok := strings.Cut(strings.TrimPrefix(c, "geo:"), ","); ok {
				lines = append(lines, &ical.Prop{Name: "GEO", Value: lat + ";" + strings.SplitN(lon, ";", 2)[0]})
				used = append(used, "coordinates")
				geoDone = true
			}
		}
		if len(used) == 0 {
			lines = append(lines, jsprop("locations/"+escapePointer(id), l))
			continue
		}
		lines = append(lines, m.rest("locations/"+escapePointer(id), l, used...)...)
	}
	return lines
}

func (m *merger) virtualLocations(e map[string]any) []*ical.Prop {
	vs, _ := e["virtualLocations"].(map[string]any)
	var lines []*ical.Prop
	for _, id := range slices.Sorted(maps.Keys(vs)) {
		v, _ := vs[id].(map[string]any)
		p := &ical.Prop{Name: "CONFERENCE", Value: str(v["uri"]), Params: []ical.Param{{Name: "VALUE", Values: []string{"URI"}}}}
		if f := keys(v["features"]); len(f) > 0 {
			for i := range f {
				f[i] = strings.ToUpper(f[i])
			}
			p.Params = append(p.Params, ical.Param{Name: "FEATURE", Values: f})
		}
		if n := str(v["name"]); n != "" {
			p.Params = append(p.Params, ical.Param{Name: "LABEL", Values: []string{n}})
		}
		p.Params = append(p.Params, ical.Param{Name: "JSID", Values: []string{id}})
		lines = append(lines, p)
		lines = append(lines, m.rest("virtualLocations/"+escapePointer(id), v, "uri", "features", "name")...)
	}
	return lines
}

func (m *merger) links(e map[string]any) []*ical.Prop {
	ls, _ := e["links"].(map[string]any)
	var lines []*ical.Prop
	for _, id := range slices.Sorted(maps.Keys(ls)) {
		l, _ := ls[id].(map[string]any)
		href, ct := str(l["href"]), str(l["contentType"])
		var p *ical.Prop
		used := []string{"href", "contentType", "rel", "size"}
		if str(l["rel"]) == "describedby" && ct == "" {
			p = &ical.Prop{Name: "URL", Value: href}
		} else {
			p = &ical.Prop{Name: "ATTACH", Value: href}
			if head, data, ok := strings.Cut(strings.TrimPrefix(href, "data:"), ";base64,"); ok && strings.HasPrefix(href, "data:") {
				if ct == "" {
					ct = head
				}
				p.Value = data
				p.Params = append(p.Params, ical.Param{Name: "ENCODING", Values: []string{"BASE64"}}, ical.Param{Name: "VALUE", Values: []string{"BINARY"}})
			}
			if ct != "" {
				p.Params = append(p.Params, ical.Param{Name: "FMTTYPE", Values: []string{ct}})
			}
			if n, ok := num(l["size"]); ok {
				p.Params = append(p.Params, ical.Param{Name: "SIZE", Values: []string{strconv.Itoa(n)}})
			}
			if r := str(l["rel"]); r != "" && r != "enclosure" {
				used = slices.DeleteFunc(used, func(s string) bool { return s == "rel" })
			}
		}
		p.Params = append(p.Params, ical.Param{Name: "JSID", Values: []string{id}})
		lines = append(lines, p)
		lines = append(lines, m.rest("links/"+escapePointer(id), l, used...)...)
	}
	return lines
}

var roleOrder = []struct{ role, ical string }{{"chair", "CHAIR"}, {"required", "REQ-PARTICIPANT"}, {"optional", "OPT-PARTICIPANT"}, {"informational", "NON-PARTICIPANT"}}

var cutypes = map[string]string{"individual": "INDIVIDUAL", "group": "GROUP", "resource": "RESOURCE", "location": "ROOM"}

// participants rewrites ORGANIZER/ATTENDEE lines: entry by entry, or all
// of them when the organizer changed.
func (m *merger) participants(oldE, newE map[string]any) error {
	oldP, _ := oldE["participants"].(map[string]any)
	newP, _ := newE["participants"].(map[string]any)
	org := str(newE["organizerCalendarAddress"])
	if !same(oldE["organizerCalendarAddress"], newE["organizerCalendarAddress"]) {
		var lines []*ical.Prop
		for _, id := range slices.Sorted(maps.Keys(newP)) {
			lines = append(lines, m.participant(id, newP[id].(map[string]any), org)...)
		}
		if org != "" && !slices.ContainsFunc(slices.Collect(maps.Values(newP)), func(v any) bool {
			return strings.EqualFold(str(v.(map[string]any)["calendarAddress"]), org)
		}) {
			lines = append([]*ical.Prop{{Name: "ORGANIZER", Value: org}}, lines...)
		}
		m.touch("participants", "*", lines)
		return nil
	}
	m.pin("participants")
	for _, id := range slices.Sorted(maps.Keys(oldP)) {
		if _, ok := newP[id]; !ok {
			m.touch("participants", id, nil)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(newP)) {
		if same(oldP[id], newP[id]) {
			continue
		}
		m.touch("participants", id, m.participant(id, newP[id].(map[string]any), org))
	}
	return nil
}

// pin writes participant ids as JSID parameters, so they survive
// removals before them.
func (m *merger) pin(prop string) {
	for i, l := range m.locs {
		p := m.props[i]
		if l.prop != prop || l.id == "" || m.drop[i] || p.Name == "JSPROP" || p.Param("JSID") == l.id {
			continue
		}
		cp := *p
		cp.Params = slices.Clone(p.Params)
		cp.SetParam("JSID", l.id)
		m.props[i] = &cp
	}
}

// participant renders one participant: ORGANIZER if it is the
// organizer, ATTENDEE if it has any other role or a status.
func (m *merger) participant(id string, p map[string]any, org string) []*ical.Prop {
	addr := str(p["calendarAddress"])
	ptr := "participants/" + escapePointer(id)
	if addr == "" {
		return []*ical.Prop{jsprop(ptr, p)}
	}
	common := func(name string) *ical.Prop {
		pr := &ical.Prop{Name: name, Value: addr}
		if n := str(p["name"]); n != "" {
			pr.Params = append(pr.Params, ical.Param{Name: "CN", Values: []string{n}})
		}
		if em := str(p["email"]); em != "" && !strings.EqualFold("mailto:"+em, addr) {
			pr.Params = append(pr.Params, ical.Param{Name: "EMAIL", Values: []string{em}})
		}
		if s := str(p["sentBy"]); s != "" {
			pr.Params = append(pr.Params, ical.Param{Name: "SENT-BY", Values: []string{"mailto:" + s}})
		}
		return pr
	}
	roles := keys(p["roles"])
	var lines []*ical.Prop
	isOrg := org != "" && strings.EqualFold(addr, org)
	if isOrg {
		o := common("ORGANIZER")
		o.Params = append(o.Params, ical.Param{Name: "JSID", Values: []string{id}})
		lines = append(lines, o)
	}
	attendee := !isOrg || slices.ContainsFunc(roles, func(r string) bool { return r != "owner" }) || p["participationStatus"] != nil
	used := []string{"calendarAddress", "name", "email", "sentBy", "roles", "kind", "participationStatus", "expectReply", "delegatedTo", "delegatedFrom", "memberOf"}
	if attendee {
		a := common("ATTENDEE")
		for _, r := range roleOrder {
			if slices.Contains(roles, r.role) {
				if r.ical != "REQ-PARTICIPANT" {
					a.Params = append(a.Params, ical.Param{Name: "ROLE", Values: []string{r.ical}})
				}
				break
			}
		}
		if ct := cutypes[str(p["kind"])]; ct != "" {
			a.Params = append(a.Params, ical.Param{Name: "CUTYPE", Values: []string{ct}})
		}
		if ps := str(p["participationStatus"]); ps != "" {
			a.Params = append(a.Params, ical.Param{Name: "PARTSTAT", Values: []string{strings.ToUpper(ps)}})
		}
		if p["expectReply"] == true {
			a.Params = append(a.Params, ical.Param{Name: "RSVP", Values: []string{"TRUE"}})
		}
		for _, f := range []struct{ prop, param string }{{"delegatedTo", "DELEGATED-TO"}, {"delegatedFrom", "DELEGATED-FROM"}, {"memberOf", "MEMBER"}} {
			if v := keys(p[f.prop]); len(v) > 0 {
				a.Params = append(a.Params, ical.Param{Name: f.param, Values: v})
			}
		}
		a.Params = append(a.Params, ical.Param{Name: "JSID", Values: []string{id}})
		lines = append(lines, a)
		// Roles iCalendar has no word for (several at once, "attendee"
		// spelled otherwise) are kept exactly.
		if !rolesExpressible(roles, isOrg) {
			lines = append(lines, jsprop(ptr+"/roles", p["roles"]))
		}
	}
	return append(lines, m.rest(ptr, p, used...)...)
}

// rolesExpressible reports whether ROLE (plus being the organizer) can
// hold the roles: owner if organizer, and one of the others.
func rolesExpressible(roles []string, isOrg bool) bool {
	n := 0
	for _, r := range roles {
		switch r {
		case "owner":
			if !isOrg {
				return false
			}
		case "chair", "required", "optional", "informational":
			n++
		default:
			return false
		}
	}
	return n <= 1
}

// alerts rewrites VALARMs entry by entry; ids go into their UID.
func (m *merger) alerts(oldE, newE map[string]any) {
	oldA, _ := oldE["alerts"].(map[string]any)
	newA, _ := newE["alerts"].(map[string]any)
	byID := map[string]int{}
	for i, a := range m.alarms {
		byID[m.meta.alarmID[a]] = i
	}
	title := str(newE["title"])
	for _, id := range slices.Sorted(maps.Keys(oldA)) {
		if i, ok := byID[id]; ok {
			if _, keep := newA[id]; !keep || !same(oldA[id], newA[id]) {
				m.alarms[i] = nil
			} else if m.alarms[i].Value("UID") != id {
				// Pin the id.
				a := *m.alarms[i]
				a.Props = append(slices.Clone(a.Props), &ical.Prop{Name: "UID", Value: id})
				m.alarms[i] = &a
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(newA)) {
		if same(oldA[id], newA[id]) {
			if _, ok := byID[id]; ok {
				continue
			}
		}
		va, extra := alarm(id, newA[id].(map[string]any), title)
		m.newAl = append(m.newAl, va)
		m.touch("alerts", id, extra)
	}
	for _, id := range slices.Sorted(maps.Keys(oldA)) {
		if _, ok := newA[id]; !ok {
			m.touch("alerts", id, nil)
		}
	}
}

// alarm renders one alert as a VALARM, and JSPROP lines (for the VEVENT)
// for what a VALARM can't hold.
func alarm(id string, a map[string]any, title string) (*ical.Component, []*ical.Prop) {
	va := &ical.Component{Name: "VALARM", Props: []*ical.Prop{{Name: "UID", Value: id}}}
	action := "DISPLAY"
	if str(a["action"]) == "email" {
		action = "EMAIL"
	}
	va.Props = append(va.Props, &ical.Prop{Name: "ACTION", Value: action})
	trig, _ := a["trigger"].(map[string]any)
	ptr := "alerts/" + escapePointer(id)
	var extra []*ical.Prop
	switch str(trig["@type"]) {
	case "AbsoluteTrigger":
		if t, err := time.Parse(time.RFC3339, str(trig["when"])); err == nil {
			va.Props = append(va.Props, &ical.Prop{Name: "TRIGGER", Params: []ical.Param{{Name: "VALUE", Values: []string{"DATE-TIME"}}}, Value: t.UTC().Format("20060102T150405Z")})
		}
	case "OffsetTrigger", "":
		p := &ical.Prop{Name: "TRIGGER", Value: str(trig["offset"])}
		if str(trig["relativeTo"]) == "end" {
			p.Params = []ical.Param{{Name: "RELATED", Values: []string{"END"}}}
		}
		va.Props = append(va.Props, p)
	default:
		// An unknown trigger: keep it, and a VALARM that never fires.
		va.Props = append(va.Props, &ical.Prop{Name: "TRIGGER", Params: []ical.Param{{Name: "VALUE", Values: []string{"DATE-TIME"}}}, Value: "19760401T005545Z"})
		extra = append(extra, jsprop(ptr+"/trigger", trig))
	}
	if title == "" {
		title = "Reminder"
	}
	va.Props = append(va.Props, &ical.Prop{Name: "DESCRIPTION", Value: ical.EscapeText(title)})
	if action == "EMAIL" {
		va.Props = append(va.Props, &ical.Prop{Name: "SUMMARY", Value: ical.EscapeText(title)})
	}
	if t, err := time.Parse(time.RFC3339, str(a["acknowledged"])); err == nil {
		va.Props = append(va.Props, &ical.Prop{Name: "ACKNOWLEDGED", Value: t.UTC().Format("20060102T150405Z")})
	}
	rel, _ := a["relatedTo"].(map[string]any)
	for _, key := range slices.Sorted(maps.Keys(rel)) {
		r, _ := rel[key].(map[string]any)
		p := &ical.Prop{Name: "RELATED-TO", Value: ical.EscapeText(key)}
		for _, t := range keys(r["relation"]) {
			if t != "parent" {
				p.Params = append(p.Params, ical.Param{Name: "RELTYPE", Values: []string{strings.ToUpper(t)}})
			}
		}
		va.Props = append(va.Props, p)
	}
	for _, f := range slices.Sorted(maps.Keys(a)) {
		if !slices.Contains([]string{"@type", "action", "trigger", "acknowledged", "relatedTo"}, f) {
			extra = append(extra, jsprop(ptr+"/"+escapePointer(f), a[f]))
		}
	}
	return va, extra
}

// iCalProps renders jCal arrays back into lines.
func (m *merger) iCalProps(v any) []*ical.Prop {
	list, _ := v.([]any)
	var out []*ical.Prop
	for _, item := range list {
		a, _ := item.([]any)
		if len(a) != 4 {
			continue
		}
		p := &ical.Prop{Name: strings.ToUpper(str(a[0])), Value: str(a[3])}
		params, _ := a[1].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(params)) {
			p.Params = append(p.Params, ical.Param{Name: strings.ToUpper(name), Values: paramStrings(params[name])})
		}
		if typ := str(a[2]); typ != "" && typ != "unknown" {
			p.Params = append(p.Params, ical.Param{Name: "VALUE", Values: []string{strings.ToUpper(typ)}})
		}
		out = append(out, p)
	}
	return out
}

// rest turns the fields of obj not in used into JSPROP lines.
func (m *merger) rest(ptr string, obj map[string]any, used ...string) []*ical.Prop {
	var out []*ical.Prop
	for _, f := range slices.Sorted(maps.Keys(obj)) {
		if f != "@type" && !slices.Contains(used, f) {
			out = append(out, jsprop(ptr+"/"+escapePointer(f), obj[f]))
		}
	}
	return out
}

// jsprop is a line keeping a value at ptr that iCalendar can't express.
func jsprop(ptr string, v any) *ical.Prop {
	b, _ := json.Marshal(v)
	return &ical.Prop{Name: "JSPROP", Params: []ical.Param{{Name: "JSPTR", Values: []string{ptr}}}, Value: ical.EscapeText(string(b))}
}

func setProp(c *ical.Component, p *ical.Prop) {
	for i, q := range c.Props {
		if q.Name == p.Name {
			c.Props[i] = p
			return
		}
	}
	c.Props = append(c.Props, p)
}

func normalize(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func normalizeAny(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

func union(a, b map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// same compares normalized values, ignoring "@type" (clients may leave
// it out; it carries nothing iCalendar keeps).
func same(a, b any) bool { return reflect.DeepEqual(untyped(a), untyped(b)) }

func untyped(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if k != "@type" {
				out[k] = untyped(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = untyped(e)
		}
		return out
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), n == float64(int(n))
	}
	return 0, false
}

func keys(v any) []string {
	m, _ := v.(map[string]any)
	return slices.Sorted(maps.Keys(m))
}

func paramStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

// PatchFor returns the recurrenceOverrides patch that turns the
// occurrence of master at rid into inst.
func PatchFor(master map[string]any, rid string, inst map[string]any) map[string]any {
	base := normalize(master)
	delete(base, "recurrenceOverrides")
	delete(base, "recurrenceRule")
	base["start"] = rid
	return diff(base, normalize(inst), "")
}
