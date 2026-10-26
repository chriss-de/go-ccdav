package caldav

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/rrule"
)

const (
	utcFormat  = "20060102T150405Z"
	dateFormat = "20060102"
)

// MaxExpandBytes bounds what expand may produce for one request: a tiny
// FREQ=SECONDLY object could otherwise expand to gigabytes.
const MaxExpandBytes = 32 << 20

// ErrExpandTooLarge is returned when expanding exceeds an ExpandBudget.
var ErrExpandTooLarge = errors.New("caldav: expanded data too large")

// ExpandBudget is what expanding may still produce for one request, in
// bytes of the instances' source components, and the request's context,
// which stops expanding when done; shared by all objects of a REPORT.
type ExpandBudget struct {
	left int
	ctx  context.Context
}

// NewExpandBudget returns a budget of MaxExpandBytes for a request.
func NewExpandBudget(ctx context.Context) *ExpandBudget {
	return &ExpandBudget{left: MaxExpandBytes, ctx: ctx}
}

// RenderData applies a CALDAV:calendar-data request element to a stored
// object. With no expand and no comp selection the raw bytes are returned
// unchanged. Expanding draws on budget.
func RenderData(raw []byte, spec *dav.Element, floating *time.Location, budget *ExpandBudget) (string, error) {
	if spec == nil || len(spec.Children) == 0 {
		return string(raw), nil
	}
	cal, err := ical.ParseBytes(raw)
	if err != nil {
		return "", err
	}
	if ex := spec.Child(dav.NSCalDAV, "expand"); ex != nil {
		from, to, err := ParseTimeRange(ex)
		if err != nil {
			return "", err
		}
		if cal, err = Expand(cal, from, to, floating, budget); err != nil {
			return "", err
		}
	}
	if c := spec.Child(dav.NSCalDAV, "comp"); c != nil {
		cal = selectComp(cal, c)
	}
	return string(cal.Bytes()), nil
}

// Expand returns a copy of cal in which every recurring component is
// replaced by its instances in [from, to), each with a RECURRENCE-ID, and
// all date-times converted to UTC (RFC 4791 §9.6.5). Each instance costs
// budget the size of its source component; once the budget's context is
// done, Expand fails with its error.
func Expand(cal *ical.Component, from, to time.Time, floating *time.Location, budget *ExpandBudget) (*ical.Component, error) {
	out := &ical.Component{Name: cal.Name}
	for _, p := range cal.Props {
		if p.Name != "METHOD" {
			out.Props = append(out.Props, p)
		}
	}
	seen := map[*ical.Component]bool{}
	size := map[*ical.Component]int{}
	for _, name := range CalendarComponents {
		for o := range rrule.ExpandContext(budget.ctx, cal, name, "", from, to, floating) {
			n, ok := size[o.Comp]
			if !ok {
				n = len(o.Comp.Bytes())
				size[o.Comp] = n
			}
			if budget.left -= n; budget.left < 0 {
				return nil, ErrExpandTooLarge
			}
			seen[o.Comp] = true
			out.Children = append(out.Children, instance(cal, o, floating))
		}
	}
	// Components that the expander does not cover (e.g. VTODO without
	// DTSTART, VFREEBUSY) pass through with times converted to UTC.
	for _, c := range cal.Children {
		if c.Name == "VTIMEZONE" || seen[c] {
			continue
		}
		if c.Prop("DTSTART") != nil && slices.Contains(CalendarComponents, c.Name) {
			continue // has a start but no instance in range
		}
		out.Children = append(out.Children, toUTC(cal, cloneComp(c), floating))
	}
	if err := budget.ctx.Err(); err != nil {
		return nil, err // stopped early: the instances are incomplete
	}
	return out, nil
}

func recurring(cal *ical.Component, c *ical.Component) bool {
	if c.Prop("RECURRENCE-ID") != nil || c.Prop("RRULE") != nil || c.Prop("RDATE") != nil {
		return true
	}
	// A master with overrides elsewhere in the object is recurring too.
	uid := c.Value("UID")
	for _, o := range cal.Children {
		if o != c && o.Name == c.Name && o.Value("UID") == uid && o.Prop("RECURRENCE-ID") != nil {
			return true
		}
	}
	return false
}

func instance(cal *ical.Component, o rrule.Occurrence, floating *time.Location) *ical.Component {
	c := cloneComp(o.Comp)
	isRec := recurring(cal, o.Comp)
	c.Del("RRULE")
	c.Del("RDATE")
	c.Del("EXDATE")
	c.Del("EXRULE")
	hadEnd := c.Prop("DTEND") != nil
	hadDue := c.Prop("DUE") != nil
	c.Del("DTSTART")
	c.Del("DTEND")
	c.Del("DUE")
	c.Del("RECURRENCE-ID")
	start := dtProp("DTSTART", o.Start, o.AllDay)
	props := []*ical.Prop{start}
	if hadEnd {
		props = append(props, dtProp("DTEND", o.End, o.AllDay))
	}
	if hadDue {
		props = append(props, dtProp("DUE", o.End, o.AllDay))
	}
	if isRec {
		props = append(props, dtProp("RECURRENCE-ID", o.RecurrenceID, o.AllDay))
	}
	c.Props = append(props, c.Props...)
	return toUTC(cal, c, floating)
}

func dtProp(name string, t time.Time, allDay bool) *ical.Prop {
	if allDay {
		return &ical.Prop{Name: name, Params: []ical.Param{{Name: "VALUE", Values: []string{"DATE"}}}, Value: t.Format(dateFormat)}
	}
	return &ical.Prop{Name: name, Value: t.UTC().Format(utcFormat)}
}

var dateTimeProps = map[string]bool{
	"DTSTART": true, "DTEND": true, "DUE": true, "RECURRENCE-ID": true,
	"EXDATE": true, "RDATE": true, "COMPLETED": true,
}

// toUTC rewrites TZID-qualified and floating date-times to UTC, in place.
func toUTC(cal, c *ical.Component, floating *time.Location) *ical.Component {
	for i, p := range c.Props {
		if !dateTimeProps[p.Name] || p.Param("VALUE") == "DATE" || p.Param("VALUE") == "PERIOD" {
			continue
		}
		if p.Param("TZID") == "" && strings.HasSuffix(p.Value, "Z") {
			continue
		}
		starts, _, err := rrule.ParseDateTimeList(p, cal, floating)
		if err != nil || len(starts) == 0 || starts[0].AllDay {
			continue
		}
		vals := make([]string, len(starts))
		for j, s := range starts {
			vals[j] = s.Time.UTC().Format(utcFormat)
		}
		np := &ical.Prop{Name: p.Name, Value: strings.Join(vals, ",")}
		for _, pa := range p.Params {
			if pa.Name != "TZID" {
				np.Params = append(np.Params, pa)
			}
		}
		c.Props[i] = np
	}
	for _, ch := range c.Children {
		toUTC(cal, ch, floating)
	}
	return c
}

func cloneComp(c *ical.Component) *ical.Component {
	out := &ical.Component{Name: c.Name, Props: make([]*ical.Prop, len(c.Props))}
	for i, p := range c.Props {
		np := *p
		np.Params = append([]ical.Param(nil), p.Params...)
		out.Props[i] = &np
	}
	for _, ch := range c.Children {
		out.Children = append(out.Children, cloneComp(ch))
	}
	return out
}

// selectComp applies a CALDAV:comp element (partial retrieval, §9.6.1).
func selectComp(c *ical.Component, spec *dav.Element) *ical.Component {
	out := &ical.Component{Name: c.Name}
	if spec.Child(dav.NSCalDAV, "allprop") != nil || len(spec.ChildrenNamed(dav.NSCalDAV, "prop")) == 0 {
		out.Props = c.Props
	} else {
		for _, ps := range spec.ChildrenNamed(dav.NSCalDAV, "prop") {
			name := strings.ToUpper(ps.Attr("name"))
			for _, p := range c.PropsNamed(name) {
				if ps.Attr("novalue") == "yes" {
					np := *p
					np.Value = ""
					p = &np
				}
				out.Props = append(out.Props, p)
			}
		}
	}
	if spec.Child(dav.NSCalDAV, "allcomp") != nil {
		out.Children = c.Children
		return out
	}
	for _, cs := range spec.ChildrenNamed(dav.NSCalDAV, "comp") {
		name := strings.ToUpper(cs.Attr("name"))
		for _, ch := range c.ChildrenNamed(name) {
			out.Children = append(out.Children, selectComp(ch, cs))
		}
	}
	return out
}

// FloatingZone builds the location for floating times from a collection's
// calendar-timezone (a VCALENDAR with one VTIMEZONE). nil means UTC.
func FloatingZone(tzCal string) *time.Location {
	if tzCal == "" {
		return nil
	}
	cal, err := ical.ParseBytes([]byte(tzCal))
	if err != nil {
		return nil
	}
	for _, tz := range cal.ChildrenNamed("VTIMEZONE") {
		if loc, ok := rrule.LoadLocation(tz.Value("TZID"), cal); ok {
			return loc
		}
	}
	return nil
}

// ValidTimezone checks a calendar-timezone property value.
func ValidTimezone(s string) error {
	cal, err := ical.ParseBytes([]byte(s))
	if err != nil {
		return err
	}
	if cal.Name != "VCALENDAR" || len(cal.ChildrenNamed("VTIMEZONE")) != 1 {
		return fmt.Errorf("caldav: calendar-timezone must contain exactly one VTIMEZONE")
	}
	return nil
}
