// Package caldav implements the CalDAV-specific logic on top of parsed
// iCalendar data: object validation (RFC 4791 §4.1), calendar-query filters
// (§9.7) including time-range on recurring components (§9.9), and
// calendar-data retrieval with partial components and expand (§9.6).
package caldav

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"davd/internal/dav"
	"davd/internal/ical"
	"davd/internal/rrule"
)

// Validation errors map to CalDAV preconditions.
var (
	ErrInvalidData          = errors.New("caldav: invalid calendar data")            // valid-calendar-data
	ErrInvalidObject        = errors.New("caldav: invalid calendar object resource") // valid-calendar-object-resource
	ErrUnsupportedComponent = errors.New("caldav: unsupported component")            // supported-calendar-component
)

// CalendarComponents are the component types a calendar collection can hold.
var CalendarComponents = []string{"VEVENT", "VTODO", "VJOURNAL"}

// Validate checks a calendar object resource and returns its UID and
// component type. allowed lists the collection's supported components
// (empty means all).
func Validate(cal *ical.Component, allowed []string) (uid, comp string, err error) {
	if cal.Name != "VCALENDAR" {
		return "", "", fmt.Errorf("%w: top-level component is %s", ErrInvalidData, cal.Name)
	}
	masters := 0
	for _, c := range cal.Children {
		switch c.Name {
		case "VTIMEZONE":
			continue
		case "VEVENT", "VTODO", "VJOURNAL", "VFREEBUSY":
		default:
			if strings.HasPrefix(c.Name, "X-") {
				continue
			}
			return "", "", fmt.Errorf("%w: unexpected component %s", ErrInvalidObject, c.Name)
		}
		if comp == "" {
			comp = c.Name
		} else if comp != c.Name {
			return "", "", fmt.Errorf("%w: mixed component types", ErrInvalidObject)
		}
		u := c.Value("UID")
		if u == "" {
			return "", "", fmt.Errorf("%w: missing UID", ErrInvalidObject)
		}
		if uid == "" {
			uid = u
		} else if uid != u {
			return "", "", fmt.Errorf("%w: multiple UIDs", ErrInvalidObject)
		}
		if c.Prop("RECURRENCE-ID") == nil {
			masters++
		}
	}
	if comp == "" {
		return "", "", fmt.Errorf("%w: no component", ErrInvalidObject)
	}
	if masters > 1 {
		return "", "", fmt.Errorf("%w: more than one master component", ErrInvalidObject)
	}
	if len(allowed) > 0 && !slices.Contains(allowed, comp) {
		return "", "", fmt.Errorf("%w: %s", ErrUnsupportedComponent, comp)
	}
	return uid, comp, nil
}

// UIDOf returns the UID of a calendar object or vCard, "" if unparsable.
func UIDOf(data []byte) string {
	c, err := ical.ParseBytes(data)
	if err != nil {
		return ""
	}
	if c.Name == "VCARD" {
		return c.Value("UID")
	}
	for _, ch := range c.Children {
		if ch.Name != "VTIMEZONE" {
			if u := ch.Value("UID"); u != "" {
				return u
			}
		}
	}
	return ""
}

// ParseTimeRange reads start/end attributes of a CALDAV:time-range or
// expand element. Missing bounds are open.
func ParseTimeRange(e *dav.Element) (from, to time.Time, err error) {
	from = time.Unix(-1<<40, 0) // effectively -inf
	to = time.Unix(1<<40, 0)
	if s := e.Attr("start"); s != "" {
		if from, err = time.Parse("20060102T150405Z", s); err != nil {
			return from, to, fmt.Errorf("caldav: bad time-range start %q", s)
		}
	}
	if s := e.Attr("end"); s != "" {
		if to, err = time.Parse("20060102T150405Z", s); err != nil {
			return from, to, fmt.Errorf("caldav: bad time-range end %q", s)
		}
	}
	return from, to, nil
}

// Query evaluates CALDAV:filter elements against one calendar object.
type Query struct {
	Filter   *dav.Element   // the CALDAV:filter element
	Floating *time.Location // zone for floating times (calendar-timezone), nil = UTC
}

// ErrBadFilter is returned for malformed filters (→ 400 / valid-filter).
var ErrBadFilter = errors.New("caldav: invalid filter")

// Match reports whether cal matches the filter.
func (q *Query) Match(cal *ical.Component) (bool, error) {
	if q.Filter == nil {
		return true, nil
	}
	cfs := q.Filter.ChildrenNamed(dav.NSCalDAV, "comp-filter")
	if len(cfs) != 1 {
		return false, fmt.Errorf("%w: filter needs exactly one comp-filter", ErrBadFilter)
	}
	return q.compFilter(cal, nil, []*ical.Component{cal}, cfs[0])
}

// compFilter checks whether any of comps matches f. root is the VCALENDAR
// (nil while matching the VCALENDAR itself).
func (q *Query) compFilter(root, parent *ical.Component, comps []*ical.Component, f *dav.Element) (bool, error) {
	name := strings.ToUpper(f.Attr("name"))
	if name == "" {
		return false, fmt.Errorf("%w: comp-filter without name", ErrBadFilter)
	}
	var cands []*ical.Component
	for _, c := range comps {
		if c.Name == name {
			cands = append(cands, c)
		}
	}
	if f.Child(dav.NSCalDAV, "is-not-defined") != nil {
		return len(cands) == 0, nil
	}
	for _, c := range cands {
		ok, err := q.compMatches(root, parent, c, f)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

func (q *Query) compMatches(root, parent, c *ical.Component, f *dav.Element) (bool, error) {
	if root == nil {
		root = c // c is the VCALENDAR
	}
	if tr := f.Child(dav.NSCalDAV, "time-range"); tr != nil {
		from, to, err := ParseTimeRange(tr)
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrBadFilter, err)
		}
		if !q.overlaps(root, parent, c, from, to) {
			return false, nil
		}
	}
	for _, pf := range f.ChildrenNamed(dav.NSCalDAV, "prop-filter") {
		ok, err := q.propFilter(root, c, pf)
		if err != nil || !ok {
			return false, err
		}
	}
	for _, cf := range f.ChildrenNamed(dav.NSCalDAV, "comp-filter") {
		ok, err := q.compFilter(root, c, c.Children, cf)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// overlaps implements the time-range test for a component, expanding
// recurrences. Over-inclusion is safe; under-inclusion loses data on clients.
func (q *Query) overlaps(root, parent, c *ical.Component, from, to time.Time) bool {
	switch c.Name {
	case "VEVENT", "VJOURNAL", "VTODO":
		if c.Prop("DTSTART") == nil {
			if c.Name == "VTODO" {
				return q.todoNoStart(root, c, from, to)
			}
			return false
		}
		for o := range rrule.Expand(root, c.Name, c.Value("UID"), from, to, q.Floating) {
			if o.Comp == c {
				return true
			}
		}
		return false
	case "VALARM":
		return q.alarmOverlaps(root, parent, c, from, to)
	default:
		// VFREEBUSY, VAVAILABILITY etc.: be inclusive.
		return true
	}
}

// todoNoStart covers the RFC 4791 §9.9 VTODO rows without DTSTART.
func (q *Query) todoNoStart(root, c *ical.Component, from, to time.Time) bool {
	get := func(name string) (time.Time, bool) {
		p := c.Prop(name)
		if p == nil {
			return time.Time{}, false
		}
		dt, err := rrule.ParseDateTime(p, root, q.Floating)
		if err != nil {
			return time.Time{}, false
		}
		return dt.Time, true
	}
	if due, ok := get("DUE"); ok {
		return from.Before(due) && !to.Before(due)
	}
	completed, hasCompleted := get("COMPLETED")
	created, hasCreated := get("CREATED")
	switch {
	case hasCompleted && hasCreated:
		return (!from.After(created) || !from.After(completed)) &&
			(!to.Before(created) || !to.Before(completed))
	case hasCompleted:
		return !from.After(completed) && !to.Before(completed)
	case hasCreated:
		return to.After(created)
	}
	return true
}

// alarmOverlaps checks whether any trigger of the alarm (for any instance
// of its parent, including repeats) falls into [from, to).
func (q *Query) alarmOverlaps(root, parent, alarm *ical.Component, from, to time.Time) bool {
	trig := alarm.Prop("TRIGGER")
	if trig == nil || parent == nil {
		return false
	}
	repeat, gap := 0, rrule.Duration{}
	if r := alarm.Value("REPEAT"); r != "" {
		fmt.Sscanf(r, "%d", &repeat)
		gap, _ = rrule.ParseDuration(alarm.Value("DURATION"))
	}
	fire := func(base time.Time) bool {
		for i := 0; i <= repeat && i < 1000; i++ {
			if !base.Before(from) && base.Before(to) {
				return true
			}
			base = gap.AddTo(base)
		}
		return false
	}
	if trig.Param("VALUE") == "DATE-TIME" {
		dt, err := rrule.ParseDateTime(trig, root, q.Floating)
		return err == nil && fire(dt.Time)
	}
	d, err := rrule.ParseDuration(trig.Value)
	if err != nil {
		return false
	}
	// Widen the window by the trigger offset so that instances whose alarm
	// lands in range are found, then test each alarm time exactly.
	span := time.Duration(d.Days)*24*time.Hour + d.Clock
	if span < 0 {
		span = -span
	}
	span += 48 * time.Hour
	if repeat > 0 {
		span += time.Duration(repeat) * (time.Duration(gap.Days)*24*time.Hour + gap.Clock)
	}
	for o := range rrule.Expand(root, parent.Name, parent.Value("UID"), from.Add(-span), to.Add(span), q.Floating) {
		if o.Comp != parent {
			continue
		}
		base := o.Start
		if trig.Param("RELATED") == "END" {
			base = o.End
		}
		if fire(d.AddTo(base)) {
			return true
		}
	}
	return false
}

func (q *Query) propFilter(root, c *ical.Component, f *dav.Element) (bool, error) {
	name := strings.ToUpper(f.Attr("name"))
	if name == "" {
		return false, fmt.Errorf("%w: prop-filter without name", ErrBadFilter)
	}
	props := c.PropsNamed(name)
	if f.Child(dav.NSCalDAV, "is-not-defined") != nil {
		return len(props) == 0, nil
	}
	for _, p := range props {
		ok, err := q.propMatches(root, p, f)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

func (q *Query) propMatches(root *ical.Component, p *ical.Prop, f *dav.Element) (bool, error) {
	if tr := f.Child(dav.NSCalDAV, "time-range"); tr != nil {
		from, to, err := ParseTimeRange(tr)
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrBadFilter, err)
		}
		dt, err := rrule.ParseDateTime(p, root, q.Floating)
		if err != nil || dt.Time.Before(from) || !dt.Time.Before(to) {
			return false, nil
		}
	}
	if tm := f.Child(dav.NSCalDAV, "text-match"); tm != nil {
		ok, err := textMatch(tm, p.Text())
		if err != nil || !ok {
			return false, err
		}
	}
	for _, pf := range f.ChildrenNamed(dav.NSCalDAV, "param-filter") {
		pname := strings.ToUpper(pf.Attr("name"))
		var vals []string
		for _, pa := range p.Params {
			if pa.Name == pname {
				vals = append(vals, pa.Values...)
			}
		}
		if pf.Child(dav.NSCalDAV, "is-not-defined") != nil {
			if len(vals) > 0 {
				return false, nil
			}
			continue
		}
		if len(vals) == 0 {
			return false, nil
		}
		if tm := pf.Child(dav.NSCalDAV, "text-match"); tm != nil {
			any := false
			for _, v := range vals {
				ok, err := textMatch(tm, v)
				if err != nil {
					return false, err
				}
				any = any || ok
			}
			if !any {
				return false, nil
			}
		}
	}
	return true, nil
}

// textMatch implements CALDAV:text-match (substring, collations
// i;ascii-casemap (default), i;unicode-casemap and i;octet).
func textMatch(tm *dav.Element, value string) (bool, error) {
	needle := tm.Text
	var ok bool
	switch tm.Attr("collation") {
	case "", "i;ascii-casemap":
		ok = strings.Contains(asciiLower(value), asciiLower(needle))
	case "i;unicode-casemap":
		ok = strings.Contains(strings.ToLower(value), strings.ToLower(needle))
	case "i;octet":
		ok = strings.Contains(value, needle)
	default:
		return false, fmt.Errorf("%w: unsupported collation %q", ErrBadFilter, tm.Attr("collation"))
	}
	if tm.Attr("negate-condition") == "yes" {
		ok = !ok
	}
	return ok, nil
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
