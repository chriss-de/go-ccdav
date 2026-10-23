// Package rrule implements RFC 5545 recurrence rules, date/time value
// parsing, time zone resolution (IANA, Windows names, embedded VTIMEZONE)
// and expansion of recurring iCalendar components into instances.
//
// The expansion core is a port of the algorithm used by python-dateutil
// (day masks per year, filtered by the BYxxx parts, then BYSETPOS), adapted
// to RFC 5545 semantics: DTSTART is always the first instance, and
// occurrences are built from wall-clock fields in the DTSTART location so
// that recurring events keep their local time across DST transitions.
package rrule

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Frequency is the FREQ rule part. The order matches python-dateutil, so
// "smaller" means "coarser".
type Frequency int

const (
	Yearly Frequency = iota
	Monthly
	Weekly
	Daily
	Hourly
	Minutely
	Secondly
)

var freqNames = [...]string{"YEARLY", "MONTHLY", "WEEKLY", "DAILY", "HOURLY", "MINUTELY", "SECONDLY"}

func (f Frequency) String() string {
	if f >= 0 && int(f) < len(freqNames) {
		return freqNames[f]
	}
	return "Frequency(" + strconv.Itoa(int(f)) + ")"
}

// WeekdayNum is one BYDAY entry, e.g. -1FR is {N: -1, Day: time.Friday}.
// N == 0 means "every such weekday in the period".
type WeekdayNum struct {
	N   int
	Day time.Weekday
}

var dayNames = [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

func (w WeekdayNum) String() string {
	if w.N != 0 {
		return strconv.Itoa(w.N) + dayNames[w.Day]
	}
	return dayNames[w.Day]
}

// Rule is a parsed RRULE value.
type Rule struct {
	Freq     Frequency
	Interval int // >= 1
	Count    int // 0 = unbounded by count

	// Until is the inclusive UNTIL bound (zero if absent). When UntilUTC is
	// false the value is floating (or a DATE when UntilIsDate is set): its
	// wall-clock fields are stored in time.UTC and interpreted in the
	// location of DTSTART. A DATE includes the whole day (python-dateutil
	// would stop at its midnight), which errs on the side of returning an
	// instance rather than losing one.
	Until       time.Time
	UntilUTC    bool
	UntilIsDate bool

	BySecond   []int
	ByMinute   []int
	ByHour     []int
	ByDay      []WeekdayNum
	ByMonthDay []int
	ByYearDay  []int
	ByWeekNo   []int
	ByMonth    []int
	BySetPos   []int
	Wkst       time.Weekday // default Monday
}

// ErrInvalid is wrapped by all Parse errors.
var ErrInvalid = errors.New("rrule: invalid rule")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Parse parses an RRULE value such as "FREQ=MONTHLY;BYDAY=-1FR". A leading
// "RRULE:" is tolerated, names are case-insensitive, unknown (X-) parts are
// ignored. COUNT together with UNTIL is accepted (whichever ends first wins).
func Parse(s string) (*Rule, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 6 && strings.EqualFold(s[:6], "RRULE:") {
		s = s[6:]
	}
	r := &Rule{Freq: -1, Interval: 1, Wkst: time.Monday}
	seen := map[string]bool{}
	for part := range strings.SplitSeq(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			return nil, invalidf("part %q without '='", part)
		}
		name = strings.ToUpper(strings.TrimSpace(name))
		val = strings.TrimSpace(val)
		if seen[name] {
			return nil, invalidf("duplicate %s", name)
		}
		seen[name] = true
		var err error
		switch name {
		case "FREQ":
			i := slices.Index(freqNames[:], strings.ToUpper(val))
			if i < 0 {
				return nil, invalidf("unknown FREQ %q", val)
			}
			r.Freq = Frequency(i)
		case "INTERVAL":
			r.Interval, err = atoiRange(val, 1, 1<<20)
		case "COUNT":
			r.Count, err = atoiRange(val, 1, 1<<30)
		case "UNTIL":
			err = r.parseUntil(val)
		case "BYSECOND":
			r.BySecond, err = intList(val, 0, 60, false)
		case "BYMINUTE":
			r.ByMinute, err = intList(val, 0, 59, false)
		case "BYHOUR":
			r.ByHour, err = intList(val, 0, 23, false)
		case "BYMONTHDAY":
			r.ByMonthDay, err = intList(val, 1, 31, true)
		case "BYYEARDAY":
			r.ByYearDay, err = intList(val, 1, 366, true)
		case "BYWEEKNO":
			r.ByWeekNo, err = intList(val, 1, 53, true)
		case "BYMONTH":
			r.ByMonth, err = intList(val, 1, 12, false)
		case "BYSETPOS":
			r.BySetPos, err = intList(val, 1, 366, true)
		case "BYDAY":
			r.ByDay, err = parseByDay(val)
		case "WKST":
			var d time.Weekday
			d, err = parseWeekday(val)
			r.Wkst = d
		default:
			if !strings.HasPrefix(name, "X-") && !validPartName(name) {
				return nil, invalidf("bad part name %q", name)
			}
			// Unknown / extension part: ignore.
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if r.Freq < 0 {
		return nil, invalidf("missing FREQ")
	}
	return r, nil
}

func validPartName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func atoiRange(s string, lo, hi int) (int, error) {
	if s != "" && s[0] == '+' {
		s = s[1:]
	}
	n, err := strconv.Atoi(s)
	if err != nil || s == "" || s[0] == '-' || s[0] == '+' {
		return 0, invalidf("bad number %q", s)
	}
	if n < lo || n > hi {
		return 0, invalidf("%d out of range [%d,%d]", n, lo, hi)
	}
	return n, nil
}

// intList parses a comma separated list of integers in [lo,hi] (or
// [-hi,-lo] when neg is set). Duplicates are removed, order is kept.
func intList(s string, lo, hi int, neg bool) ([]int, error) {
	var out []int
	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		sign := 1
		if neg && strings.HasPrefix(f, "-") {
			sign, f = -1, f[1:]
		}
		n, err := atoiRange(f, lo, hi)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, sign*n) {
			out = append(out, sign*n)
		}
	}
	return out, nil
}

func parseWeekday(s string) (time.Weekday, error) {
	i := slices.Index(dayNames[:], strings.ToUpper(strings.TrimSpace(s)))
	if i < 0 {
		return 0, invalidf("bad weekday %q", s)
	}
	return time.Weekday(i), nil
}

func parseByDay(s string) ([]WeekdayNum, error) {
	var out []WeekdayNum
	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if len(f) < 2 {
			return nil, invalidf("bad BYDAY %q", f)
		}
		d, err := parseWeekday(f[len(f)-2:])
		if err != nil {
			return nil, err
		}
		w := WeekdayNum{Day: d}
		if num := f[:len(f)-2]; num != "" {
			sign := 1
			switch num[0] {
			case '-':
				sign, num = -1, num[1:]
			case '+':
				num = num[1:]
			}
			n, err := atoiRange(num, 1, 53)
			if err != nil {
				return nil, err
			}
			w.N = sign * n
		}
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out, nil
}

func (r *Rule) parseUntil(v string) error {
	v = strings.ToUpper(v)
	utc := strings.HasSuffix(v, "Z")
	core := strings.TrimSuffix(v, "Z")
	var (
		t   time.Time
		err error
	)
	switch len(core) {
	case 8:
		if utc {
			return invalidf("bad UNTIL %q", v)
		}
		t, err = time.Parse("20060102", core)
		r.UntilIsDate = true
	case 15:
		t, err = time.Parse("20060102T150405", core)
	default:
		return invalidf("bad UNTIL %q", v)
	}
	if err != nil {
		return invalidf("bad UNTIL %q", v)
	}
	r.Until, r.UntilUTC = t, utc
	return nil
}

// String formats the rule in canonical RFC 5545 form.
func (r *Rule) String() string {
	var sb strings.Builder
	sb.WriteString("FREQ=")
	sb.WriteString(r.Freq.String())
	if r.Interval > 1 {
		fmt.Fprintf(&sb, ";INTERVAL=%d", r.Interval)
	}
	if r.Count > 0 {
		fmt.Fprintf(&sb, ";COUNT=%d", r.Count)
	}
	if !r.Until.IsZero() {
		switch {
		case r.UntilIsDate:
			sb.WriteString(";UNTIL=" + r.Until.Format("20060102"))
		case r.UntilUTC:
			sb.WriteString(";UNTIL=" + r.Until.UTC().Format("20060102T150405Z"))
		default:
			sb.WriteString(";UNTIL=" + r.Until.Format("20060102T150405"))
		}
	}
	list := func(name string, v []int) {
		if len(v) == 0 {
			return
		}
		sb.WriteString(";" + name + "=")
		for i, n := range v {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(strconv.Itoa(n))
		}
	}
	list("BYSECOND", r.BySecond)
	list("BYMINUTE", r.ByMinute)
	list("BYHOUR", r.ByHour)
	if len(r.ByDay) > 0 {
		sb.WriteString(";BYDAY=")
		for i, d := range r.ByDay {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(d.String())
		}
	}
	list("BYMONTHDAY", r.ByMonthDay)
	list("BYYEARDAY", r.ByYearDay)
	list("BYWEEKNO", r.ByWeekNo)
	list("BYMONTH", r.ByMonth)
	list("BYSETPOS", r.BySetPos)
	if r.Wkst != time.Monday {
		sb.WriteString(";WKST=" + dayNames[r.Wkst])
	}
	return sb.String()
}
