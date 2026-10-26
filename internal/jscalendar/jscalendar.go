// Package jscalendar converts iCalendar events (RFC 5545) to JSCalendar
// Event objects (draft-ietf-calext-jscalendarbis), following
// draft-ietf-calext-jscalendar-icalendar. Events are plain map[string]any
// trees, ready for JSON and JMAP property selection.
//
// One stored calendar object is one event: the master VEVENT with its
// overrides (same UID, RECURRENCE-ID) becomes one Event whose
// recurrenceOverrides hold each override as a patch against the master,
// RDATEs as additions ({}) and EXDATEs as {"excluded": true}.
//
// What has no JSCalendar equivalent is kept in iCalProps (jCal arrays,
// RFC 7265). Ids of entries (participants, alerts, locations, links)
// come from a JSID parameter when present, else from their position.
package jscalendar

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/rrule"
)

// Version is the JSCalendar version of the objects produced.
const Version = "2.0"

const localFmt = "2006-01-02T15:04:05"

// Codec converts stored iCalendar objects for JMAP.
type Codec struct{}

// ToJMAP parses one calendar object resource and converts its event.
func (Codec) ToJMAP(raw []byte) (map[string]any, error) {
	cal, err := ical.ParseBytes(raw)
	if err != nil {
		return nil, err
	}
	return FromICal(cal)
}

// ErrNoEvent means the calendar holds no VEVENT.
var ErrNoEvent = errors.New("jscalendar: no VEVENT")

// FromICal converts the event series of a VCALENDAR.
func FromICal(cal *ical.Component) (map[string]any, error) {
	if cal == nil || cal.Name != "VCALENDAR" {
		return nil, errors.New("jscalendar: not a VCALENDAR")
	}
	// The series of the first event's UID; a file should hold no other.
	events := cal.ChildrenNamed("VEVENT")
	if len(events) == 0 {
		return nil, ErrNoEvent
	}
	uid := events[0].Value("UID")
	var master *ical.Component
	var overrides []*ical.Component
	for _, ev := range events {
		if ev.Value("UID") != uid {
			continue
		}
		if ev.Prop("RECURRENCE-ID") == nil && master == nil {
			master = ev
		} else if ev.Prop("RECURRENCE-ID") != nil {
			overrides = append(overrides, ev)
		}
	}
	if master == nil {
		if len(overrides) == 0 {
			return nil, ErrNoEvent
		}
		// Only overrides (an invitation to one instance): the first one
		// stands for itself.
		e, _ := convertEvent(overrides[0], cal)
		if rid, ok := localValue(overrides[0].Prop("RECURRENCE-ID"), cal, e); ok {
			e["recurrenceId"] = rid
			if tz, ok := e["timeZone"].(string); ok {
				e["recurrenceIdTimeZone"] = tz
			}
		}
		addCalendarProps(e, cal)
		return e, nil
	}
	e, _ := convertEvent(master, cal)
	ro := map[string]any{}
	for _, p := range master.PropsNamed("RDATE") {
		starts, ends, err := rrule.ParseDateTimeList(p, cal, time.UTC)
		if err != nil {
			e["iCalProps"] = append(asList(e["iCalProps"]), jcal(p))
			continue
		}
		for i, s := range starts {
			key := localOf(s, e)
			patch := map[string]any{}
			if !ends[i].IsZero() {
				patch["duration"] = formatDuration(ends[i].Sub(s.Time))
			}
			ro[key] = patch
		}
	}
	for _, p := range master.PropsNamed("EXDATE") {
		starts, _, err := rrule.ParseDateTimeList(p, cal, time.UTC)
		if err != nil {
			e["iCalProps"] = append(asList(e["iCalProps"]), jcal(p))
			continue
		}
		for _, s := range starts {
			ro[localOf(s, e)] = map[string]any{"excluded": true}
		}
	}
	for _, ov := range overrides {
		key, ok := localValue(ov.Prop("RECURRENCE-ID"), cal, e)
		if !ok {
			continue
		}
		oe, _ := convertEvent(ov, cal)
		base := maps.Clone(e)
		base["start"] = key // an instance starts at its recurrence id unless patched
		ro[key] = diff(base, oe, "")
	}
	if len(ro) > 0 {
		e["recurrenceOverrides"] = ro
	}
	addCalendarProps(e, cal)
	return e, nil
}

func addCalendarProps(e map[string]any, cal *ical.Component) {
	if v := cal.Value("PRODID"); v != "" {
		e["prodId"] = ical.UnescapeText(v)
	}
	if v := cal.Value("METHOD"); v != "" {
		e["method"] = strings.ToLower(v)
	}
}

// overrideSkip are properties that never go into an override's patch.
var overrideSkip = map[string]bool{
	"@type": true, "version": true, "uid": true, "prodId": true, "method": true,
	"recurrenceRule": true, "recurrenceOverrides": true, "recurrenceId": true, "recurrenceIdTimeZone": true,
	"excludedRecurrenceRules": true,
	// Bookkeeping each VEVENT carries for itself.
	"created": true, "updated": true, "sequence": true,
}

// diff returns the PatchObject turning base into ov. Objects present on
// both sides are diffed member by member, so patches stay small (e.g.
// "participants/p2/participationStatus").
func diff(base, ov map[string]any, prefix string) map[string]any {
	patch := map[string]any{}
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range ov {
		keys[k] = true
	}
	for k := range keys {
		if prefix == "" && overrideSkip[k] {
			continue
		}
		path := prefix + escapePointer(k)
		bv, inBase := base[k]
		ovv, inOv := ov[k]
		switch {
		case !inOv:
			patch[path] = nil
		case !inBase:
			patch[path] = ovv
		default:
			bm, bok := bv.(map[string]any)
			om, ook := ovv.(map[string]any)
			if bok && ook {
				maps.Copy(patch, diff(bm, om, path+"/"))
			} else if !jsonEqual(bv, ovv) {
				patch[path] = ovv
			}
		}
	}
	return patch
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func escapePointer(s string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(s) }

// known are the VEVENT properties convertEvent maps.
var known = map[string]bool{
	"JSPROP": true,
	"UID":    true, "SEQUENCE": true, "CREATED": true, "LAST-MODIFIED": true, "DTSTAMP": true,
	"SUMMARY": true, "DESCRIPTION": true, "STATUS": true, "TRANSP": true, "CLASS": true,
	"PRIORITY": true, "COLOR": true, "CATEGORIES": true, "DTSTART": true, "DTEND": true,
	"DURATION": true, "RRULE": true, "RDATE": true, "EXDATE": true, "RECURRENCE-ID": true,
	"LOCATION": true, "GEO": true, "CONFERENCE": true, "URL": true, "ATTACH": true,
	"ORGANIZER": true, "ATTENDEE": true, "RELATED-TO": true,
}

// eventMeta records which participant and alert came from which line
// or component, for Merge.
type eventMeta struct {
	partID  map[string]string          // lower-cased calendar address -> participant id
	alarmID map[*ical.Component]string // VALARM -> alert id
}

// convertEvent converts one VEVENT on its own (no RDATE/EXDATE, no
// RECURRENCE-ID handling).
func convertEvent(ev *ical.Component, cal *ical.Component) (map[string]any, *eventMeta) {
	meta := &eventMeta{partID: map[string]string{}, alarmID: map[*ical.Component]string{}}
	e := map[string]any{"@type": "Event", "version": Version}
	var unknown []any
	var jsprops []*ical.Prop
	for _, p := range ev.Props {
		switch {
		case p.Name == "JSPROP" && p.Param("JSPTR") != "":
			jsprops = append(jsprops, p)
		case !known[p.Name] || p.Name == "JSPROP":
			unknown = append(unknown, jcal(p))
		}
	}
	if v := ev.Value("UID"); v != "" {
		e["uid"] = ical.UnescapeText(v)
	}
	if n, err := strconv.Atoi(strings.TrimSpace(ev.Value("SEQUENCE"))); err == nil && n > 0 {
		e["sequence"] = n
	}
	if t, ok := utcValue(ev.Prop("CREATED")); ok {
		e["created"] = t
	}
	if t, ok := utcValue(ev.Prop("LAST-MODIFIED")); ok {
		e["updated"] = t
	} else if t, ok := utcValue(ev.Prop("DTSTAMP")); ok {
		e["updated"] = t
	}
	if p := ev.Prop("SUMMARY"); p != nil {
		e["title"] = p.Text()
	}
	if p := ev.Prop("DESCRIPTION"); p != nil {
		e["description"] = p.Text()
	}
	switch strings.ToUpper(ev.Value("STATUS")) {
	case "TENTATIVE":
		e["status"] = "tentative"
	case "CANCELLED":
		e["status"] = "cancelled"
	case "CONFIRMED":
		e["status"] = "confirmed"
	}
	switch strings.ToUpper(ev.Value("TRANSP")) {
	case "TRANSPARENT":
		e["freeBusyStatus"] = "free"
	case "OPAQUE":
		e["freeBusyStatus"] = "busy"
	}
	switch strings.ToUpper(ev.Value("CLASS")) {
	case "PUBLIC":
		e["privacy"] = "public"
	case "PRIVATE":
		e["privacy"] = "private"
	case "CONFIDENTIAL":
		e["privacy"] = "secret"
	}
	if n, err := strconv.Atoi(strings.TrimSpace(ev.Value("PRIORITY"))); err == nil && n > 0 && n <= 9 {
		e["priority"] = n
	}
	if p := ev.Prop("COLOR"); p != nil {
		e["color"] = p.Text()
	}
	for _, p := range ev.PropsNamed("CATEGORIES") {
		for _, k := range ical.SplitList(p.Value) {
			if k = ical.UnescapeText(k); k != "" {
				setKey(e, "keywords", k)
			}
		}
	}
	if err := timing(ev, cal, e); err != nil {
		unknown = append(unknown, jcal(ev.Prop("DTSTART")))
	}
	if rules := ev.PropsNamed("RRULE"); len(rules) > 0 {
		if r, ok := recurrenceRule(rules[0].Value, e, cal); ok {
			e["recurrenceRule"] = r
			rules = rules[1:]
		}
		for _, p := range rules {
			unknown = append(unknown, jcal(p))
		}
	}
	locations(ev, e)
	links(ev, e)
	participants(ev, e, meta)
	for _, p := range ev.PropsNamed("RELATED-TO") {
		rel := strings.ToLower(p.Param("RELTYPE"))
		if rel == "" {
			rel = "parent"
		}
		r := entryMap(e, "relatedTo")
		obj, _ := r[p.Text()].(map[string]any)
		if obj == nil {
			obj = map[string]any{"@type": "Relation"}
			r[p.Text()] = obj
		}
		setKey(obj, "relation", rel)
	}
	alerts(ev, e, meta)
	if len(unknown) > 0 {
		e["iCalProps"] = unknown
	}
	// JSCalendar values iCalendar can't hold, written by Merge.
	for _, p := range jsprops {
		var v any
		if usableJSProp(p.Param("JSPTR"), p.Text()) && json.Unmarshal([]byte(p.Text()), &v) == nil {
			setPointer(e, p.Param("JSPTR"), v)
		}
	}
	return e, meta
}

// setPointer sets the value at a JSON pointer-like path (without the
// leading "/"), creating objects on the way.
func setPointer(m map[string]any, ptr string, v any) {
	segs := strings.Split(ptr, "/")
	for _, sg := range segs[:len(segs)-1] {
		sg = unescapePointer(sg)
		next, ok := m[sg].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[sg] = next
		}
		m = next
	}
	m[unescapePointer(segs[len(segs)-1])] = v
}

func unescapePointer(s string) string { return strings.NewReplacer("~1", "/", "~0", "~").Replace(s) }

// Bounds for JSPROP lines: the converted object must stay encodable
// (encoding/json refuses deep nesting) and cheap to build.
const (
	maxPointerSegs = 16
	maxValueDepth  = 64
)

// jsonDepth returns the nesting depth of arrays and objects in a JSON text.
func jsonDepth(b []byte) int {
	depth, deepest, inStr, esc := 0, 0, false, false
	for _, c := range b {
		switch {
		case esc:
			esc = false
		case inStr:
			esc, inStr = c == '\\', c != '"'
		case c == '"':
			inStr = true
		case c == '[' || c == '{':
			depth++
			deepest = max(deepest, depth)
		case c == ']' || c == '}':
			depth--
		}
	}
	return deepest
}

// usableJSProp reports whether a JSPROP line's pointer and value are
// within bounds.
func usableJSProp(ptr, value string) bool {
	return strings.Count(ptr, "/") < maxPointerSegs && jsonDepth([]byte(value)) <= maxValueDepth
}

// timing sets start, timeZone, showWithoutTime, duration and endTimeZone.
func timing(ev, cal *ical.Component, e map[string]any) error {
	sp := ev.Prop("DTSTART")
	if sp == nil {
		return nil
	}
	start, err := rrule.ParseDateTime(sp, cal, time.UTC)
	if err != nil {
		return err
	}
	var loc *time.Location
	switch {
	case start.AllDay:
		e["showWithoutTime"] = true
		e["start"] = start.Time.Format("2006-01-02") + "T00:00:00"
	case start.Floating:
		e["start"] = start.Time.Format(localFmt)
	default:
		var name string
		loc, name = zoneOf(sp, cal)
		e["timeZone"] = name
		e["start"] = start.Time.In(loc).Format(localFmt)
	}
	switch {
	case ev.Prop("DTEND") != nil:
		end, err := rrule.ParseDateTime(ev.Prop("DTEND"), cal, time.UTC)
		if err != nil {
			return nil
		}
		switch {
		case start.AllDay:
			days := int(end.Time.Sub(start.Time).Round(24*time.Hour) / (24 * time.Hour))
			e["duration"] = formatParts(false, max(days, 0), 0)
		case loc == nil || end.Floating:
			e["duration"] = formatDuration(end.Time.Sub(start.Time))
		default:
			endLoc, endName := zoneOf(ev.Prop("DTEND"), cal)
			if endName != e["timeZone"] {
				e["endTimeZone"] = endName
				e["duration"] = formatDuration(end.Time.Sub(start.Time))
			} else {
				e["duration"] = wallDuration(start.Time.In(loc), end.Time.In(endLoc))
			}
		}
	case ev.Prop("DURATION") != nil:
		if d, err := rrule.ParseDuration(ev.Value("DURATION")); err == nil && !d.Neg {
			e["duration"] = formatParts(false, d.Days, d.Clock)
		}
	case start.AllDay:
		e["duration"] = "P1D"
	}
	if d, ok := e["duration"]; ok && d == "PT0S" {
		delete(e, "duration")
	}
	return nil
}

// zoneOf resolves the TZID (or UTC) of a date-time to a location and an
// IANA name; zones without one become Etc/UTC.
func zoneOf(p *ical.Prop, cal *ical.Component) (*time.Location, string) {
	if strings.HasSuffix(strings.TrimSpace(p.Value), "Z") || p.Param("TZID") == "" {
		return time.UTC, "Etc/UTC"
	}
	loc, ok := rrule.LoadLocation(p.Param("TZID"), cal)
	if !ok {
		return time.UTC, "Etc/UTC"
	}
	if name := loc.String(); name != "" && name != "Local" {
		if _, err := time.LoadLocation(name); err == nil {
			if name == "UTC" {
				name = "Etc/UTC"
			}
			return loc, name
		}
	}
	// A custom VTIMEZONE without an IANA name: keep the instant, in UTC.
	return time.UTC, "Etc/UTC"
}

// wallDuration is the nominal (wall clock) duration between two times of
// one zone: whole days plus the rest.
func wallDuration(a, b time.Time) string {
	wa := time.Date(a.Year(), a.Month(), a.Day(), a.Hour(), a.Minute(), a.Second(), 0, time.UTC)
	wb := time.Date(b.Year(), b.Month(), b.Day(), b.Hour(), b.Minute(), b.Second(), 0, time.UTC)
	d := wb.Sub(wa)
	if d < 0 {
		return formatDuration(b.Sub(a))
	}
	return formatParts(false, int(d/(24*time.Hour)), d%(24*time.Hour))
}

func formatDuration(d time.Duration) string {
	neg := d < 0
	if neg {
		d = -d
	}
	return formatParts(neg, int(d/(24*time.Hour)), d%(24*time.Hour))
}

// formatParts renders a JSCalendar (signed) duration: "P1DT2H", "-PT15M".
func formatParts(neg bool, days int, clock time.Duration) string {
	var sb strings.Builder
	if neg {
		sb.WriteByte('-')
	}
	sb.WriteByte('P')
	days += int(clock / (24 * time.Hour))
	clock %= 24 * time.Hour
	if days > 0 {
		sb.WriteString(strconv.Itoa(days) + "D")
	}
	h, m, s := int(clock/time.Hour), int(clock%time.Hour/time.Minute), int(clock%time.Minute/time.Second)
	if h > 0 || m > 0 || s > 0 || days == 0 {
		sb.WriteByte('T')
		if h > 0 {
			sb.WriteString(strconv.Itoa(h) + "H")
		}
		if m > 0 {
			sb.WriteString(strconv.Itoa(m) + "M")
		}
		if s > 0 || h == 0 && m == 0 {
			sb.WriteString(strconv.Itoa(s) + "S")
		}
	}
	return sb.String()
}

// localOf renders a parsed date-time as a LocalDateTime of event e: in
// its time zone, as a date for all-day events, as is when floating.
func localOf(dt rrule.DateTime, e map[string]any) string {
	if e["showWithoutTime"] == true || dt.AllDay {
		return dt.Time.Format("2006-01-02") + "T00:00:00"
	}
	if dt.Floating {
		return dt.Time.Format(localFmt)
	}
	loc := time.UTC
	if name, ok := e["timeZone"].(string); ok {
		if l, err := rrule.LoadZone(name); err == nil {
			loc = l
		}
	}
	return dt.Time.In(loc).Format(localFmt)
}

func localValue(p *ical.Prop, cal *ical.Component, e map[string]any) (string, bool) {
	if p == nil {
		return "", false
	}
	dt, err := rrule.ParseDateTime(p, cal, time.UTC)
	if err != nil {
		return "", false
	}
	return localOf(dt, e), true
}

// utcValue reads a UTC date-time property (CREATED, DTSTAMP, ...).
func utcValue(p *ical.Prop) (string, bool) {
	if p == nil {
		return "", false
	}
	for _, layout := range []string{"20060102T150405Z", "20060102T150405"} {
		if t, err := time.Parse(layout, strings.TrimSpace(p.Value)); err == nil {
			return t.UTC().Format(time.RFC3339), true
		}
	}
	return "", false
}

var freqNames = map[rrule.Frequency]string{
	rrule.Yearly: "yearly", rrule.Monthly: "monthly", rrule.Weekly: "weekly", rrule.Daily: "daily",
	rrule.Hourly: "hourly", rrule.Minutely: "minutely", rrule.Secondly: "secondly",
}

var dayNames = [...]string{"su", "mo", "tu", "we", "th", "fr", "sa"}

// recurrenceRule converts an RRULE value. Parts it doesn't know (RSCALE)
// make it fail, and the RRULE goes to iCalProps instead.
func recurrenceRule(v string, e map[string]any, cal *ical.Component) (map[string]any, bool) {
	if strings.Contains(strings.ToUpper(v), "RSCALE") {
		return nil, false
	}
	r, err := rrule.Parse(v)
	if err != nil {
		return nil, false
	}
	out := map[string]any{"@type": "RecurrenceRule", "frequency": freqNames[r.Freq]}
	if r.Interval > 1 {
		out["interval"] = r.Interval
	}
	if r.Count > 0 {
		out["count"] = r.Count
	}
	if !r.Until.IsZero() {
		switch {
		case r.UntilIsDate && e["showWithoutTime"] == true:
			out["until"] = r.Until.Format("2006-01-02") + "T00:00:00"
		case r.UntilIsDate:
			out["until"] = r.Until.Format("2006-01-02") + "T23:59:59"
		case r.UntilUTC:
			out["until"] = localOf(rrule.DateTime{Time: r.Until}, e)
		default:
			out["until"] = r.Until.Format(localFmt)
		}
	}
	if r.Wkst != time.Monday {
		out["firstDayOfWeek"] = dayNames[r.Wkst]
	}
	if len(r.ByDay) > 0 {
		var days []any
		for _, d := range r.ByDay {
			nd := map[string]any{"@type": "NDay", "day": dayNames[d.Day]}
			if d.N != 0 {
				nd["nthOfPeriod"] = d.N
			}
			days = append(days, nd)
		}
		out["byDay"] = days
	}
	ints := func(name string, v []int) {
		if len(v) > 0 {
			out[name] = slices.Clone(v)
		}
	}
	ints("byMonthDay", r.ByMonthDay)
	ints("byYearDay", r.ByYearDay)
	ints("byWeekNo", r.ByWeekNo)
	ints("byHour", r.ByHour)
	ints("byMinute", r.ByMinute)
	ints("bySecond", r.BySecond)
	ints("bySetPosition", r.BySetPos)
	if len(r.ByMonth) > 0 {
		var ms []any
		for _, m := range r.ByMonth {
			ms = append(ms, strconv.Itoa(m))
		}
		out["byMonth"] = ms
	}
	return out, true
}

// entryMap returns the map property prop of e, creating it.
func entryMap(e map[string]any, prop string) map[string]any {
	m, _ := e[prop].(map[string]any)
	if m == nil {
		m = map[string]any{}
		e[prop] = m
	}
	return m
}

// entry adds obj to the map property prop under the JSID of p, or a
// positional id.
func entry(e map[string]any, prop, prefix string, p *ical.Prop, obj map[string]any) string {
	id := ""
	if p != nil {
		id = p.Param("JSID")
	}
	return entryWithID(e, prop, prefix, id, obj)
}

// entryWithID adds obj under id if usable, else under a positional id.
func entryWithID(e map[string]any, prop, prefix, id string, obj map[string]any) string {
	m := entryMap(e, prop)
	if !validID(id) || m[id] != nil {
		for n := len(m) + 1; ; n++ {
			if id = prefix + strconv.Itoa(n); m[id] == nil {
				break
			}
		}
	}
	m[id] = obj
	return id
}

func validID(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func locations(ev *ical.Component, e map[string]any) {
	var first map[string]any
	for _, p := range ev.PropsNamed("LOCATION") {
		obj := map[string]any{"@type": "Location", "name": p.Text()}
		entry(e, "locations", "l", p, obj)
		if first == nil {
			first = obj
		}
	}
	if p := ev.Prop("GEO"); p != nil {
		if lat, lon, ok := strings.Cut(p.Value, ";"); ok {
			geo := "geo:" + strings.TrimSpace(lat) + "," + strings.TrimSpace(lon)
			if first != nil {
				first["coordinates"] = geo
			} else {
				entry(e, "locations", "l", p, map[string]any{"@type": "Location", "coordinates": geo})
			}
		}
	}
	for _, p := range ev.PropsNamed("CONFERENCE") {
		obj := map[string]any{"@type": "VirtualLocation", "uri": p.Text()}
		if l := p.Param("LABEL"); l != "" {
			obj["name"] = l
		}
		for _, f := range paramList(p, "FEATURE") {
			setKey(obj, "features", strings.ToLower(f))
		}
		entry(e, "virtualLocations", "v", p, obj)
	}
}

func links(ev *ical.Component, e map[string]any) {
	for _, p := range ev.PropsNamed("URL") {
		entry(e, "links", "k", p, map[string]any{"@type": "Link", "href": p.Text(), "rel": "describedby"})
	}
	for _, p := range ev.PropsNamed("ATTACH") {
		obj := map[string]any{"@type": "Link", "href": p.Text()}
		mt := p.Param("FMTTYPE")
		if strings.EqualFold(p.Param("VALUE"), "BINARY") || strings.EqualFold(p.Param("ENCODING"), "BASE64") {
			if mt == "" {
				mt = "application/octet-stream"
			}
			obj["href"] = "data:" + mt + ";base64," + strings.Join(strings.Fields(p.Value), "")
		}
		if mt != "" {
			obj["contentType"] = mt
		}
		if n, err := strconv.Atoi(p.Param("SIZE")); err == nil {
			obj["size"] = n
		}
		entry(e, "links", "k", p, obj)
	}
}

var roles = map[string]string{"REQ-PARTICIPANT": "required", "OPT-PARTICIPANT": "optional", "NON-PARTICIPANT": "informational", "CHAIR": "chair"}

var kinds = map[string]string{"INDIVIDUAL": "individual", "GROUP": "group", "RESOURCE": "resource", "ROOM": "location"}

func participants(ev *ical.Component, e map[string]any, meta *eventMeta) {
	byAddr := map[string]map[string]any{}
	add := func(p *ical.Prop) map[string]any {
		addr := p.Text()
		if obj, ok := byAddr[strings.ToLower(addr)]; ok {
			return obj
		}
		obj := map[string]any{"@type": "Participant", "calendarAddress": addr}
		if cn := p.Param("CN"); cn != "" {
			obj["name"] = cn
		}
		if em := p.Param("EMAIL"); em != "" {
			obj["email"] = em
		} else if m, ok := cutPrefixFold(addr, "mailto:"); ok {
			obj["email"] = m
		}
		if s := p.Param("SENT-BY"); s != "" {
			v, _ := cutPrefixFold(s, "mailto:")
			obj["sentBy"] = v
		}
		meta.partID[strings.ToLower(addr)] = entry(e, "participants", "p", p, obj)
		byAddr[strings.ToLower(addr)] = obj
		return obj
	}
	if p := ev.Prop("ORGANIZER"); p != nil {
		e["organizerCalendarAddress"] = p.Text()
		setKey(add(p), "roles", "owner")
	}
	for _, p := range ev.PropsNamed("ATTENDEE") {
		obj := add(p)
		role := roles[strings.ToUpper(p.Param("ROLE"))]
		if role == "" {
			role = "required"
		}
		setKey(obj, "roles", role)
		if k := kinds[strings.ToUpper(p.Param("CUTYPE"))]; k != "" {
			obj["kind"] = k
		}
		if ps := strings.ToLower(p.Param("PARTSTAT")); ps != "" && ps != "needs-action" {
			obj["participationStatus"] = ps
		}
		if strings.EqualFold(p.Param("RSVP"), "TRUE") {
			obj["expectReply"] = true
		}
		for _, f := range []struct{ param, prop string }{{"DELEGATED-TO", "delegatedTo"}, {"DELEGATED-FROM", "delegatedFrom"}, {"MEMBER", "memberOf"}} {
			for _, v := range paramList(p, f.param) {
				setKey(obj, f.prop, v)
			}
		}
	}
}

func alerts(ev *ical.Component, e map[string]any, meta *eventMeta) {
	for _, va := range ev.ChildrenNamed("VALARM") {
		tp := va.Prop("TRIGGER")
		if tp == nil {
			continue
		}
		obj := map[string]any{"@type": "Alert"}
		if strings.EqualFold(tp.Param("VALUE"), "DATE-TIME") {
			t, ok := utcValue(tp)
			if !ok {
				continue
			}
			obj["trigger"] = map[string]any{"@type": "AbsoluteTrigger", "when": t}
		} else {
			d, err := rrule.ParseDuration(tp.Value)
			if err != nil {
				continue
			}
			trig := map[string]any{"@type": "OffsetTrigger", "offset": formatParts(d.Neg, d.Days, d.Clock)}
			if strings.EqualFold(tp.Param("RELATED"), "END") {
				trig["relativeTo"] = "end"
			}
			obj["trigger"] = trig
		}
		if strings.EqualFold(va.Value("ACTION"), "EMAIL") {
			obj["action"] = "email"
		}
		if t, ok := utcValue(va.Prop("ACKNOWLEDGED")); ok {
			obj["acknowledged"] = t
		}
		for _, p := range va.PropsNamed("RELATED-TO") {
			rel := strings.ToLower(p.Param("RELTYPE"))
			if rel == "" {
				rel = "parent"
			}
			r := entryMap(obj, "relatedTo")
			ro, _ := r[p.Text()].(map[string]any)
			if ro == nil {
				ro = map[string]any{"@type": "Relation"}
				r[p.Text()] = ro
			}
			setKey(ro, "relation", rel)
		}
		// Lines JSCalendar has no place for (DESCRIPTION, Apple's
		// X-WR-ALARMUID and X-APPLE-DEFAULT-ALARM, ...) stay with the alert.
		var rest []any
		for _, p := range va.Props {
			switch p.Name {
			// DESCRIPTION and SUMMARY are written from the title.
			case "UID", "ACTION", "TRIGGER", "ACKNOWLEDGED", "RELATED-TO", "DESCRIPTION", "SUMMARY":
			default:
				rest = append(rest, jcal(p))
			}
		}
		if len(rest) > 0 {
			obj["iCalProps"] = rest
		}
		// The id: the VALARM's UID (RFC 9074) if usable, else positional.
		meta.alarmID[va] = entryWithID(e, "alerts", "a", va.Value("UID"), obj)
	}
}

func paramList(p *ical.Prop, name string) []string {
	var out []string
	for _, pa := range p.Params {
		if pa.Name == name {
			out = append(out, pa.Values...)
		}
	}
	return out
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

func setKey(m map[string]any, prop, key string) {
	set, _ := m[prop].(map[string]any)
	if set == nil {
		set = map[string]any{}
		m[prop] = set
	}
	set[key] = true
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// jcal renders a property as a jCal array (RFC 7265): name, parameters,
// value type, raw value.
func jcal(p *ical.Prop) []any {
	params := map[string]any{}
	for _, pa := range p.Params {
		if len(pa.Values) == 1 {
			params[strings.ToLower(pa.Name)] = pa.Values[0]
		} else {
			vals := make([]any, len(pa.Values))
			for i, v := range pa.Values {
				vals[i] = v
			}
			params[strings.ToLower(pa.Name)] = vals
		}
	}
	typ := "unknown"
	if v := p.Param("VALUE"); v != "" {
		typ = strings.ToLower(v)
		delete(params, "value")
	}
	return []any{strings.ToLower(p.Name), params, typ, p.Value}
}
