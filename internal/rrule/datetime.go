package rrule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"davd/internal/ical"
)

// DateTime is a parsed DATE or DATE-TIME property value.
type DateTime struct {
	Time     time.Time // resolved instant; DATE values are midnight in the floating location
	AllDay   bool      // VALUE=DATE
	Floating bool      // no TZID and no trailing Z (always true for DATE values)
}

// ErrValue is wrapped by date, date-time and duration parse errors.
var ErrValue = errors.New("rrule: invalid value")

// ParseDateTime parses DTSTART/DTEND/DUE/RECURRENCE-ID/EXDATE-style single
// values. cal is the enclosing VCALENDAR (for VTIMEZONE lookup) and may be
// nil. floating is the location used for floating and DATE values (nil =
// UTC). A TZID on a DATE value is ignored. An unresolvable TZID falls back to
// UTC (see LoadLocation).
func ParseDateTime(p *ical.Prop, cal *ical.Component, floating *time.Location) (DateTime, error) {
	if p == nil {
		return DateTime{}, fmt.Errorf("%w: missing property", ErrValue)
	}
	return parseDT(strings.TrimSpace(p.Value), p.Param("VALUE"), p.Param("TZID"), cal, floating)
}

// ParseDateTimeList parses multi-valued EXDATE/RDATE properties (comma
// separated). For PERIOD values (RDATE;VALUE=PERIOD, or any value containing
// '/') starts holds the period start and ends the period end (explicit or
// start+duration); for other values the end is the zero time.
func ParseDateTimeList(p *ical.Prop, cal *ical.Component, floating *time.Location) (starts []DateTime, ends []time.Time, err error) {
	if p == nil {
		return nil, nil, fmt.Errorf("%w: missing property", ErrValue)
	}
	vt, tzid := p.Param("VALUE"), p.Param("TZID")
	for _, item := range ical.SplitList(p.Value) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		var end time.Time
		if s, e, ok := strings.Cut(item, "/"); ok {
			start, err := parseDT(s, "DATE-TIME", tzid, cal, floating)
			if err != nil {
				return nil, nil, err
			}
			if strings.HasPrefix(e, "P") || strings.HasPrefix(e, "+P") || strings.HasPrefix(e, "-P") {
				d, err := ParseDuration(e)
				if err != nil {
					return nil, nil, err
				}
				end = d.AddTo(start.Time)
			} else {
				ed, err := parseDT(e, "DATE-TIME", tzid, cal, floating)
				if err != nil {
					return nil, nil, err
				}
				end = ed.Time
			}
			starts = append(starts, start)
			ends = append(ends, end)
			continue
		}
		if strings.EqualFold(vt, "PERIOD") {
			return nil, nil, fmt.Errorf("%w: bad period %q", ErrValue, item)
		}
		dt, err := parseDT(item, vt, tzid, cal, floating)
		if err != nil {
			return nil, nil, err
		}
		starts = append(starts, dt)
		ends = append(ends, end)
	}
	return starts, ends, nil
}

func parseDT(v, valueType, tzid string, cal *ical.Component, floating *time.Location) (DateTime, error) {
	if floating == nil {
		floating = time.UTC
	}
	bad := func() (DateTime, error) { return DateTime{}, fmt.Errorf("%w: bad date-time %q", ErrValue, v) }
	isDate := strings.EqualFold(valueType, "DATE") || len(v) == 8
	if isDate {
		if len(v) != 8 {
			return bad()
		}
		y, m, d, ok := parseDate(v)
		if !ok {
			return bad()
		}
		return DateTime{Time: time.Date(y, time.Month(m), d, 0, 0, 0, 0, floating), AllDay: true, Floating: true}, nil
	}
	utc := false
	switch {
	case len(v) == 16 && (v[15] == 'Z' || v[15] == 'z'):
		utc = true
	case len(v) == 15:
	default:
		return bad()
	}
	if v[8] != 'T' && v[8] != 't' {
		return bad()
	}
	y, m, d, ok := parseDate(v[:8])
	if !ok {
		return bad()
	}
	h, ok1 := atoi2(v[9:11])
	mi, ok2 := atoi2(v[11:13])
	s, ok3 := atoi2(v[13:15])
	if !ok1 || !ok2 || !ok3 || h > 23 || mi > 59 || s > 60 {
		return bad()
	}
	if s == 60 {
		s = 59 // leap second: clamp
	}
	c := civil{y, m, d, h, mi, s}
	switch {
	case utc:
		return DateTime{Time: resolve(c, time.UTC)}, nil
	case tzid != "":
		loc, _ := LoadLocation(tzid, cal)
		return DateTime{Time: resolve(c, loc)}, nil
	default:
		return DateTime{Time: resolve(c, floating), Floating: true}, nil
	}
}

func parseDate(v string) (y, m, d int, ok bool) {
	if len(v) != 8 {
		return
	}
	for i := range 8 {
		if v[i] < '0' || v[i] > '9' {
			return
		}
	}
	y, _ = strconv.Atoi(v[:4])
	m, _ = strconv.Atoi(v[4:6])
	d, _ = strconv.Atoi(v[6:8])
	if m < 1 || m > 12 || d < 1 || d > daysIn(y, m) {
		return 0, 0, 0, false
	}
	return y, m, d, true
}

func atoi2(s string) (int, bool) {
	if len(s) != 2 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}

// Duration is an RFC 5545 DURATION value. Weeks are folded into Days. Days
// are nominal (wall-clock) days, Clock is exact elapsed time.
type Duration struct {
	Neg   bool
	Days  int
	Clock time.Duration
}

// maxDurDays bounds parsed durations (about 27000 years) so arithmetic
// cannot overflow.
const maxDurDays = 10_000_000

// ParseDuration parses an RFC 5545 duration such as "P1W", "-PT15M" or
// "P1DT2H". It is lenient about combining weeks with other units.
func ParseDuration(s string) (Duration, error) {
	orig := s
	bad := func() (Duration, error) { return Duration{}, fmt.Errorf("%w: bad duration %q", ErrValue, orig) }
	var d Duration
	s = strings.ToUpper(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(s, "-"):
		d.Neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	if !strings.HasPrefix(s, "P") {
		return bad()
	}
	s = s[1:]
	if s == "" {
		return bad()
	}
	inTime, seen := false, false
	var secs int64
	for s != "" {
		if s[0] == 'T' {
			if inTime {
				return bad()
			}
			inTime, s = true, s[1:]
			if s == "" {
				return bad()
			}
			continue
		}
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i == len(s) || i > 9 {
			return bad()
		}
		n, _ := strconv.ParseInt(s[:i], 10, 64)
		unit := s[i]
		s = s[i+1:]
		seen = true
		switch {
		case !inTime && unit == 'W':
			d.Days += int(n) * 7
		case !inTime && unit == 'D':
			d.Days += int(n)
		case inTime && unit == 'H':
			secs += n * 3600
		case inTime && unit == 'M':
			secs += n * 60
		case inTime && unit == 'S':
			secs += n
		default:
			return bad()
		}
		if d.Days > maxDurDays || secs > maxDurDays*86400 {
			return bad()
		}
	}
	if !seen {
		return bad()
	}
	d.Clock = time.Duration(secs) * time.Second
	return d, nil
}

// AddTo adds the duration to t: days are added on the wall clock of t's
// location (so P1D across a DST change keeps the local time), then the
// clock part is added as elapsed time.
func (d Duration) AddTo(t time.Time) time.Time {
	sign := 1
	if d.Neg {
		sign = -1
	}
	if d.Days != 0 {
		c := civilOf(t)
		c.d += sign * d.Days
		n := daysFromCivil(c.y, c.m, c.d)
		c.y, c.m, c.d = civilFromDays(n)
		t = resolve(c, t.Location()).Add(time.Duration(t.Nanosecond()))
	}
	return t.Add(time.Duration(sign) * d.Clock)
}

// String formats the duration in RFC 5545 form.
func (d Duration) String() string {
	var sb strings.Builder
	if d.Neg {
		sb.WriteByte('-')
	}
	sb.WriteByte('P')
	if d.Days != 0 {
		if d.Days%7 == 0 && d.Clock == 0 {
			fmt.Fprintf(&sb, "%dW", d.Days/7)
			return sb.String()
		}
		fmt.Fprintf(&sb, "%dD", d.Days)
	}
	if d.Clock != 0 || d.Days == 0 {
		secs := int64(d.Clock / time.Second)
		sb.WriteByte('T')
		if h := secs / 3600; h != 0 {
			fmt.Fprintf(&sb, "%dH", h)
		}
		if m := secs / 60 % 60; m != 0 {
			fmt.Fprintf(&sb, "%dM", m)
		}
		if s := secs % 60; s != 0 || secs == 0 {
			fmt.Fprintf(&sb, "%dS", s)
		}
	}
	return sb.String()
}
