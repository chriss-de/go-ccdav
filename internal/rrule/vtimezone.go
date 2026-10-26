package rrule

import (
	"fmt"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// VTimezone builds a VTIMEZONE for an IANA zone from Go's zone data,
// valid from the start of from's year on (earlier history is left out to
// keep it small). Transitions that repeat every year on the same rule
// ("last Sunday of March at 01:00 UTC") become one observance with an
// RRULE (with UNTIL if the rule ended); others are listed one by one.
func VTimezone(name string, from time.Time) (*ical.Component, error) {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	startYear := from.Year()
	endYear := max(time.Now().Year(), startYear) + 10
	// A little before the year, so that it is covered in every zone.
	start := time.Date(startYear, 1, 1, 0, 0, 0, 0, loc).AddDate(0, 0, -2)
	end := time.Date(endYear, 12, 31, 23, 59, 59, 0, loc)
	trans := transitions(start, end)

	vtz := &ical.Component{Name: "VTIMEZONE", Props: []*ical.Prop{{Name: "TZID", Value: name}}}
	// The offset in force at the start, as an observance of its own.
	abbr, off := start.Zone()
	initial := observance(start.IsDST(), off, off, abbr, wall(start.UTC(), off), "")
	vtz.Children = append(vtz.Children, initial)
	for i := 0; i < len(trans); {
		n := run(trans, i, endYear)
		t := trans[i]
		rule := ""
		if n > 1 {
			rule = yearlyRule(t)
			if last := trans[i+(n-1)*2]; last.at.Year() < endYear-1 {
				rule += ";UNTIL=" + last.at.UTC().Format("20060102T150405Z")
			}
		}
		vtz.Children = append(vtz.Children, observance(t.isDST, t.from, t.to, t.abbr, wall(t.at, t.from), rule))
		if n <= 1 {
			i++
			continue
		}
		// The run's members are every other transition (DST on, DST off);
		// mark them done and continue with the first one not in it.
		for k := 0; k < n; k++ {
			trans[i+k*2].done = true
		}
		for i < len(trans) && trans[i].done {
			i++
		}
	}
	return vtz, nil
}

type zoneTransition struct {
	at       time.Time // UTC instant
	from, to int       // offsets in seconds
	abbr     string    // after the transition
	isDST    bool
	done     bool
}

// transitions finds the offset changes in [start, end]: a daily scan,
// then a binary search to the second.
func transitions(start, end time.Time) []zoneTransition {
	var out []zoneTransition
	_, prevOff := start.Zone()
	prev := start
	for t := start.Add(24 * time.Hour); !t.After(end.Add(24 * time.Hour)); t = t.Add(24 * time.Hour) {
		_, off := t.Zone()
		if off == prevOff {
			prev = t
			continue
		}
		lo, hi := prev, t // offset changes in (lo, hi]
		for hi.Sub(lo) > time.Second {
			mid := lo.Add(hi.Sub(lo) / 2).Truncate(time.Second)
			if _, o := mid.Zone(); o == prevOff {
				lo = mid
			} else {
				hi = mid
			}
		}
		abbr, _ := hi.Zone()
		out = append(out, zoneTransition{at: hi.UTC(), from: prevOff, to: off, abbr: abbr, isDST: hi.IsDST()})
		prevOff, prev = off, t
	}
	return out
}

// run counts how many transitions, starting at i and taking every other
// one, follow the same yearly rule in consecutive years.
func run(trans []zoneTransition, i, endYear int) int {
	n := 1
	for j := i + 2; j < len(trans); j += 2 {
		a, b := trans[j-2], trans[j]
		if b.at.Year() != a.at.Year()+1 || b.from != a.from || b.to != a.to || b.isDST != a.isDST ||
			yearlyRule(a) != yearlyRule(b) || wall(a.at, a.from).Format("150405") != wall(b.at, b.from).Format("150405") {
			break
		}
		n++
	}
	return n
}

var icalDays = [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

// yearlyRule describes a transition's date as a yearly RRULE: its month
// and weekday, counted from the end of the month in its last week.
func yearlyRule(t zoneTransition) string {
	w := wall(t.at, t.from)
	nth := (w.Day()-1)/7 + 1
	if w.Day() > daysIn(w.Year(), int(w.Month()))-7 {
		nth = -1
	}
	return fmt.Sprintf("FREQ=YEARLY;BYMONTH=%d;BYDAY=%d%s", int(w.Month()), nth, icalDays[w.Weekday()])
}

// wall is the wall clock time at instant t in a zone with offset off,
// as fields of a UTC time.
func wall(t time.Time, off int) time.Time { return t.UTC().Add(time.Duration(off) * time.Second) }

func observance(isDST bool, from, to int, abbr string, dtstart time.Time, rule string) *ical.Component {
	name := "STANDARD"
	if isDST {
		name = "DAYLIGHT"
	}
	o := &ical.Component{Name: name, Props: []*ical.Prop{
		{Name: "DTSTART", Value: dtstart.Format("20060102T150405")},
		{Name: "TZOFFSETFROM", Value: offsetString(from)},
		{Name: "TZOFFSETTO", Value: offsetString(to)},
	}}
	if rule != "" {
		o.Props = append(o.Props, &ical.Prop{Name: "RRULE", Value: rule})
	}
	if abbr != "" {
		o.Props = append(o.Props, &ical.Prop{Name: "TZNAME", Value: ical.EscapeText(abbr)})
	}
	return o
}

// offsetString renders a UTC offset as +HHMM, or +HHMMSS when it has
// seconds.
func offsetString(off int) string {
	sign := '+'
	if off < 0 {
		sign, off = '-', -off
	}
	if off%60 != 0 {
		return fmt.Sprintf("%c%02d%02d%02d", sign, off/3600, off/60%60, off%60)
	}
	return fmt.Sprintf("%c%02d%02d", sign, off/3600, off/60%60)
}
