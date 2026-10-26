package rrule

import (
	"cmp"
	"context"
	"iter"
	"slices"
	"time"
)

const (
	maxYear = 9999
	// horizonYears bounds how far the expansion runs past the last
	// produced instance before concluding the rule is exhausted. 400 years
	// is a full Gregorian cycle, so any date/weekday pattern recurs within
	// it.
	horizonYears = 400
	// maxEmptySteps bounds the number of consecutive periods that produce
	// nothing (protects sub-daily rules that can never match).
	maxEmptySteps = 1 << 20
	// maxWork bounds the fruitless steps of one expansion: periods that
	// produce nothing (weighted by their days) and the inner searches of
	// sub-daily rules. A budget is shared by all rules of an Expand call or
	// a VTIMEZONE, so rules that rarely or never match can't search for
	// centuries each. Productive periods are bounded by the consumer.
	maxWork = 1 << 21
)

// budget is the work an expansion may still do, and the context it does
// it for (nil: none).
type budget struct {
	left  int
	ctx   context.Context
	ticks int
}

func newBudget() *budget { return &budget{left: maxWork} }

// alive reports whether the expansion may go on: it checks the context
// every 1024 calls.
func (b *budget) alive() bool {
	if b.ticks++; b.ctx == nil || b.ticks%1024 != 0 {
		return true
	}
	return b.ctx.Err() == nil
}

// spend uses n steps and reports whether the budget still holds.
func (b *budget) spend(n int) bool {
	b.left -= n
	return b.left >= 0
}

// Iter yields the occurrences of the rule for the given DTSTART in ascending
// order, in dtstart.Location(). Per RFC 5545, DTSTART is always the first
// instance (even if it does not match the rule) and counts towards COUNT.
// Sub-second precision of dtstart is dropped.
//
// Occurrences are built from wall-clock fields; see resolve for the handling
// of DST gaps and overlaps. Non-existent dates (Feb 30, the 5th Monday of a
// month that has four) are skipped. Output is strictly increasing: if two
// wall-clock times map to the same instant (DST gap) only one is yielded.
//
// The sequence ends when COUNT or UNTIL is reached, at year 9999, or when
// no occurrence was found for 400 years (impossible rules such as
// BYMONTH=2;BYMONTHDAY=30). Unbounded rules are otherwise infinite; the
// consumer must stop pulling.
func (r *Rule) Iter(dtstart time.Time) iter.Seq[time.Time] {
	return r.iter(dtstart, newBudget())
}

// iter is Iter drawing on work, which the caller may share between rules.
func (r *Rule) iter(dtstart time.Time, work *budget) iter.Seq[time.Time] {
	return func(yield func(time.Time) bool) {
		dtstart = dtstart.Truncate(time.Second)
		if !yield(dtstart) {
			return
		}
		n := 1
		if r.Count > 0 && n >= r.Count {
			return
		}
		for t := range r.expandWithin(dtstart, 0, work) {
			if !t.After(dtstart) {
				continue
			}
			if !yield(t) {
				return
			}
			n++
			if r.Count > 0 && n >= r.Count {
				return
			}
		}
	}
}

// untilBefore reports whether t is after the UNTIL bound.
func (r *Rule) untilBefore(t time.Time) bool {
	if r.Until.IsZero() {
		return false
	}
	switch {
	case r.UntilIsDate:
		y, m, d := t.Date()
		return daysFromCivil(y, int(m), d) > daysFromCivil(r.Until.Year(), int(r.Until.Month()), r.Until.Day())
	case r.UntilUTC:
		return t.After(r.Until)
	default:
		return civilOf(t).key() > civilOf(r.Until).key()
	}
}

// expand runs the python-dateutil algorithm: candidates not before dtstart
// that match the rule, limited by count (0 = no limit) and UNTIL. DTSTART is
// only included if it matches. Used directly by the dateutil test vectors.
func (r *Rule) expand(dtstart time.Time, count int) iter.Seq[time.Time] {
	return r.expandWithin(dtstart, count, newBudget())
}

func (r *Rule) expandWithin(dtstart time.Time, count int, work *budget) iter.Seq[time.Time] {
	return func(yield func(time.Time) bool) {
		e := newExpander(r, dtstart)
		e.work = work
		e.run(count, yield)
	}
}

type hms struct{ h, mi, s int }

type expander struct {
	r        *Rule
	loc      *time.Location
	start    civil
	startKey int64
	interval int

	bymonth     []int
	byweekno    []int
	byyearday   []int
	bymonthday  []int
	bynmonthday []int
	byweekday   []int // Monday = 0
	bynweekday  []WeekdayNum
	byhour      []int
	byminute    []int
	bysecond    []int
	bysetpos    []int
	timeset     []hms
	impossible  bool
	work        *budget

	// year info
	year, yearLen, nextYearLen int
	yearDays                   int // day number of Jan 1
	mmask, mdaymask, nmdaymask []int
	wdaymask                   []int
	mrange                     [13]int
	wnomask                    []bool
	nwdaymask                  []bool
	lastYear, lastMonth        int
}

func newExpander(r *Rule, dtstart time.Time) *expander {
	e := &expander{r: r, loc: dtstart.Location(), start: civilOf(dtstart), interval: max(r.Interval, 1)}
	e.startKey = e.start.key()
	st := e.start
	stWday := weekdayOf(daysFromCivil(st.y, st.m, st.d))

	e.bymonth = slices.Clone(r.ByMonth)
	e.byweekno = slices.Clone(r.ByWeekNo)
	e.byyearday = slices.Clone(r.ByYearDay)
	e.bysetpos = slices.Clone(r.BySetPos)
	for _, d := range r.ByMonthDay {
		if d > 0 {
			e.bymonthday = append(e.bymonthday, d)
		} else {
			e.bynmonthday = append(e.bynmonthday, d)
		}
	}
	for _, w := range r.ByDay {
		wd := monWeekday(w.Day)
		if w.N == 0 || r.Freq > Monthly {
			if !slices.Contains(e.byweekday, wd) {
				e.byweekday = append(e.byweekday, wd)
			}
		} else {
			e.bynweekday = append(e.bynweekday, WeekdayNum{N: w.N, Day: time.Weekday(wd)})
		}
	}
	// Implied parts (RFC 5545: missing parts are taken from DTSTART).
	if len(r.ByWeekNo) == 0 && len(r.ByYearDay) == 0 && len(r.ByMonthDay) == 0 && len(r.ByDay) == 0 {
		switch r.Freq {
		case Yearly:
			if len(e.bymonth) == 0 {
				e.bymonth = []int{st.m}
			}
			e.bymonthday = []int{st.d}
		case Monthly:
			e.bymonthday = []int{st.d}
		case Weekly:
			e.byweekday = []int{stWday}
		}
	}
	bySet := func(given []int, freq Frequency, startVal, base int) []int {
		if len(given) == 0 {
			if r.Freq < freq {
				return []int{startVal}
			}
			return nil
		}
		out := slices.Clone(given)
		if r.Freq == freq {
			// Only values reachable from DTSTART with this interval.
			g := gcd(e.interval, base)
			out = slices.DeleteFunc(out, func(v int) bool { return g != 1 && pyMod(v-startVal, g) != 0 })
			if len(out) == 0 {
				e.impossible = true
			}
		}
		slices.Sort(out)
		return out
	}
	e.byhour = bySet(r.ByHour, Hourly, st.h, 24)
	e.byminute = bySet(r.ByMinute, Minutely, st.mi, 60)
	sec := slices.DeleteFunc(slices.Clone(r.BySecond), func(v int) bool { return v > 59 }) // leap seconds are ignored
	if len(r.BySecond) > 0 && len(sec) == 0 {
		e.impossible = true
	}
	e.bysecond = bySet(sec, Secondly, st.s, 60)

	if r.Freq < Hourly {
		for _, h := range e.byhour {
			for _, mi := range e.byminute {
				for _, s := range e.bysecond {
					e.timeset = append(e.timeset, hms{h, mi, s})
				}
			}
		}
	}
	return e
}

// rebuild recomputes the day masks for a new year and/or month.
func (e *expander) rebuild(year, month int) {
	r := e.r
	if year != e.lastYear {
		e.year = year
		e.yearLen = yearLen(year)
		e.nextYearLen = yearLen(year + 1)
		e.yearDays = daysFromCivil(year, 1, 1)
		n := e.yearLen + 7
		e.mmask = e.mmask[:0]
		e.mdaymask = e.mdaymask[:0]
		e.nmdaymask = e.nmdaymask[:0]
		e.wdaymask = e.wdaymask[:0]
		for i := range n {
			y, m, d := civilFromDays(e.yearDays + i)
			e.mmask = append(e.mmask, m)
			e.mdaymask = append(e.mdaymask, d)
			e.nmdaymask = append(e.nmdaymask, d-daysIn(y, m)-1)
			e.wdaymask = append(e.wdaymask, weekdayOf(e.yearDays+i))
		}
		e.mrange[0] = 0
		for m := 1; m <= 12; m++ {
			e.mrange[m] = e.mrange[m-1] + daysIn(year, m)
		}
		if len(e.byweekno) > 0 {
			e.wnomask = make([]bool, n)
			wkst := monWeekday(r.Wkst)
			for i := range n {
				wk, num := weekNo(e.yearDays+i, wkst)
				for _, b := range e.byweekno {
					if b == wk || b < 0 && b == wk-num-1 {
						e.wnomask[i] = true
						break
					}
				}
			}
		}
	}
	if len(e.bynweekday) > 0 && (month != e.lastMonth || year != e.lastYear) {
		var ranges [][2]int
		switch r.Freq {
		case Yearly:
			if len(e.bymonth) > 0 {
				for _, m := range e.bymonth {
					ranges = append(ranges, [2]int{e.mrange[m-1], e.mrange[m]})
				}
			} else {
				ranges = [][2]int{{0, e.yearLen}}
			}
		case Monthly:
			ranges = [][2]int{{e.mrange[month-1], e.mrange[month]}}
		}
		e.nwdaymask = nil
		if ranges != nil {
			e.nwdaymask = make([]bool, e.yearLen)
			for _, rg := range ranges {
				first, last := rg[0], rg[1]-1
				for _, w := range e.bynweekday {
					wd := int(w.Day)
					var i int
					if w.N < 0 {
						i = last + (w.N+1)*7
						if i < first {
							continue
						}
						i -= pyMod(e.wdaymask[i]-wd, 7)
					} else {
						i = first + (w.N-1)*7
						if i > last {
							continue
						}
						i += pyMod(7-e.wdaymask[i]+wd, 7)
					}
					if first <= i && i <= last {
						e.nwdaymask[i] = true
					}
				}
			}
		}
	}
	e.lastYear, e.lastMonth = year, month
}

// week1Start returns the day number on which week 1 of year y starts: the
// first week (beginning on wkst) that has at least four days in y.
func week1Start(y, wkst int) int {
	jan1 := daysFromCivil(y, 1, 1)
	off := pyMod(weekdayOf(jan1)-wkst, 7)
	if off <= 3 {
		return jan1 - off
	}
	return jan1 - off + 7
}

// weekNo returns the ISO-style week number (honoring wkst) of day number
// day, and the number of weeks in that week's year.
func weekNo(day, wkst int) (week, numWeeks int) {
	y, _, _ := civilFromDays(day)
	w1 := week1Start(y, wkst)
	switch {
	case day < w1:
		y--
	case day >= week1Start(y+1, wkst):
		y++
	}
	w1 = week1Start(y, wkst)
	return (day-w1)/7 + 1, (week1Start(y+1, wkst) - w1) / 7
}

// dayset returns the candidate day indexes (relative to Jan 1 of e.year,
// possibly reaching into the next year for weekly periods) of the period
// containing year/month/day.
func (e *expander) dayset(year, month, day int) (start, end int) {
	switch e.r.Freq {
	case Yearly:
		return 0, e.yearLen
	case Monthly:
		return e.mrange[month-1], e.mrange[month]
	case Weekly:
		i := daysFromCivil(year, month, day) - e.yearDays
		start = i
		wkst := monWeekday(e.r.Wkst)
		for range 7 {
			i++
			if e.wdaymask[i] == wkst {
				break
			}
		}
		return start, i
	default:
		i := daysFromCivil(year, month, day) - e.yearDays
		return i, i + 1
	}
}

func (e *expander) dayMatches(i int) bool {
	if len(e.bymonth) > 0 && !slices.Contains(e.bymonth, e.mmask[i]) {
		return false
	}
	if len(e.byweekno) > 0 && !e.wnomask[i] {
		return false
	}
	if len(e.byweekday) > 0 && !slices.Contains(e.byweekday, e.wdaymask[i]) {
		return false
	}
	if e.nwdaymask != nil && (i >= len(e.nwdaymask) || !e.nwdaymask[i]) {
		return false
	}
	if (len(e.bymonthday) > 0 || len(e.bynmonthday) > 0) &&
		!slices.Contains(e.bymonthday, e.mdaymask[i]) && !slices.Contains(e.bynmonthday, e.nmdaymask[i]) {
		return false
	}
	if len(e.byyearday) > 0 {
		if i < e.yearLen {
			if !slices.Contains(e.byyearday, i+1) && !slices.Contains(e.byyearday, -e.yearLen+i) {
				return false
			}
		} else if !slices.Contains(e.byyearday, i+1-e.yearLen) && !slices.Contains(e.byyearday, -e.nextYearLen+i-e.yearLen) {
			return false
		}
	}
	return true
}

// gettimeset returns the times of a sub-daily period.
func (e *expander) gettimeset(h, mi, s int) []hms {
	var out []hms
	switch e.r.Freq {
	case Hourly:
		for _, m := range e.byminute {
			for _, sec := range e.bysecond {
				out = append(out, hms{h, m, sec})
			}
		}
	case Minutely:
		for _, sec := range e.bysecond {
			out = append(out, hms{h, mi, sec})
		}
	default:
		out = []hms{{h, mi, s}}
	}
	return out
}

// modDistance advances value by interval steps (mod base) until it lands
// in set; it returns the number of base wraps and the new value.
func (e *expander) modDistance(value int, set []int, base int) (int, int, bool) {
	acc := 0
	for range base {
		value += e.interval
		acc += floorDiv(value, base)
		value = pyMod(value, base)
		if slices.Contains(set, value) {
			return acc, value, true
		}
	}
	return 0, 0, false
}

func (e *expander) run(count int, yield func(time.Time) bool) {
	r := e.r
	if e.impossible {
		return
	}
	st := e.start
	year, month, day := st.y, st.m, st.d
	hour, minute, second := st.h, st.mi, st.s
	weekday := weekdayOf(daysFromCivil(year, month, day))
	e.lastYear, e.lastMonth = -1<<31, -1
	e.rebuild(year, month)

	var timeset []hms
	if r.Freq < Hourly {
		timeset = e.timeset
	} else if !(len(e.byhour) > 0 && !slices.Contains(e.byhour, hour) ||
		r.Freq >= Minutely && len(e.byminute) > 0 && !slices.Contains(e.byminute, minute) ||
		r.Freq >= Secondly && len(e.bysecond) > 0 && !slices.Contains(e.bysecond, second)) {
		timeset = e.gettimeset(hour, minute, second)
	}

	var (
		last       time.Time
		haveLast   bool
		lastYield  = year
		emptySteps = 0
		buf        []civil
		days       []int
		inst       []time.Time
	)
	for {
		if !e.work.alive() {
			return
		}
		// Collect the matching days of this period.
		start, end := e.dayset(year, month, day)
		days = days[:0]
		filtered := false
		for i := start; i < end; i++ {
			if e.dayMatches(i) {
				days = append(days, i)
			} else {
				filtered = true
			}
		}
		// Combine with times, apply BYSETPOS.
		buf = buf[:0]
		mk := func(i int, t hms) civil {
			y, m, d := civilFromDays(e.yearDays + i)
			return civil{y, m, d, t.h, t.mi, t.s}
		}
		if len(e.bysetpos) > 0 && len(timeset) > 0 {
			n := len(days) * len(timeset)
			for _, pos := range e.bysetpos {
				idx := pos - 1
				if pos < 0 {
					idx = n + pos
				}
				if idx < 0 || idx >= n {
					continue
				}
				c := mk(days[idx/len(timeset)], timeset[idx%len(timeset)])
				if !slices.Contains(buf, c) {
					buf = append(buf, c)
				}
			}
			slices.SortFunc(buf, func(a, b civil) int { return cmp.Compare(a.key(), b.key()) })
		} else {
			for _, i := range days {
				for _, t := range timeset {
					buf = append(buf, mk(i, t))
				}
			}
		}
		// Emit.
		inst = inst[:0]
		untilHit := false
		for _, c := range buf {
			if c.key() < e.startKey {
				continue
			}
			t := resolve(c, e.loc)
			if r.untilBefore(t) {
				untilHit = true
				break
			}
			inst = append(inst, t)
		}
		slices.SortFunc(inst, func(a, b time.Time) int { return a.Compare(b) })
		produced := false
		for _, t := range inst {
			if haveLast && !t.After(last) {
				continue
			}
			if !yield(t) {
				return
			}
			last, haveLast, produced = t, true, true
			if count > 0 {
				count--
				if count == 0 {
					return
				}
			}
		}
		if untilHit {
			return
		}
		if produced {
			lastYield = year
			emptySteps = 0
		} else {
			emptySteps++
			if emptySteps > maxEmptySteps || year > lastYield+horizonYears+e.interval || !e.work.spend(1+end-start) {
				return
			}
		}

		// Advance to the next period.
		fixday := false
		switch r.Freq {
		case Yearly:
			year += e.interval
			if year > maxYear {
				return
			}
			e.rebuild(year, month)
		case Monthly:
			month += e.interval
			if month > 12 {
				year += floorDiv(month-1, 12)
				month = pyMod(month-1, 12) + 1
				if year > maxYear {
					return
				}
			}
			e.rebuild(year, month)
		case Weekly:
			wkst := monWeekday(r.Wkst)
			if wkst > weekday {
				day += -(weekday + 1 + (6 - wkst)) + e.interval*7
			} else {
				day += -(weekday - wkst) + e.interval*7
			}
			weekday = wkst
			fixday = true
		case Daily:
			day += e.interval
			fixday = true
		case Hourly:
			if filtered {
				// Jump to the last iteration of this day.
				hour += ((23 - hour) / e.interval) * e.interval
			}
			var ndays int
			if len(e.byhour) > 0 {
				var ok bool
				ndays, hour, ok = e.modDistance(hour, e.byhour, 24)
				if !ok {
					return
				}
			} else {
				ndays, hour = floorDiv(hour+e.interval, 24), pyMod(hour+e.interval, 24)
			}
			if ndays != 0 {
				day += ndays
				fixday = true
			}
			timeset = e.gettimeset(hour, minute, second)
		case Minutely:
			if filtered {
				minute += ((1439 - (hour*60 + minute)) / e.interval) * e.interval
			}
			valid := false
			rep := 24 * 60
			for range rep / gcd(e.interval, rep) {
				if !e.work.spend(1) {
					return
				}
				var nhours int
				if len(e.byminute) > 0 {
					var ok bool
					nhours, minute, ok = e.modDistance(minute, e.byminute, 60)
					if !ok {
						return
					}
				} else {
					nhours, minute = floorDiv(minute+e.interval, 60), pyMod(minute+e.interval, 60)
				}
				hour += nhours
				if div := floorDiv(hour, 24); div != 0 {
					hour = pyMod(hour, 24)
					day += div
					fixday = true
				}
				if len(e.byhour) == 0 || slices.Contains(e.byhour, hour) {
					valid = true
					break
				}
			}
			if !valid {
				return
			}
			timeset = e.gettimeset(hour, minute, second)
		case Secondly:
			if filtered {
				second += ((86399 - (hour*3600 + minute*60 + second)) / e.interval) * e.interval
			}
			valid := false
			rep := 24 * 3600
			for range rep / gcd(e.interval, rep) {
				if !e.work.spend(1) {
					return
				}
				var nminutes int
				if len(e.bysecond) > 0 {
					var ok bool
					nminutes, second, ok = e.modDistance(second, e.bysecond, 60)
					if !ok {
						return
					}
				} else {
					nminutes, second = floorDiv(second+e.interval, 60), pyMod(second+e.interval, 60)
				}
				minute += nminutes
				if div := floorDiv(minute, 60); div != 0 {
					minute = pyMod(minute, 60)
					hour += div
					if div := floorDiv(hour, 24); div != 0 {
						hour = pyMod(hour, 24)
						day += div
						fixday = true
					}
				}
				if (len(e.byhour) == 0 || slices.Contains(e.byhour, hour)) &&
					(len(e.byminute) == 0 || slices.Contains(e.byminute, minute)) &&
					(len(e.bysecond) == 0 || slices.Contains(e.bysecond, second)) {
					valid = true
					break
				}
			}
			if !valid {
				return
			}
			timeset = e.gettimeset(hour, minute, second)
		}
		if fixday && day > 28 {
			if dim := daysIn(year, month); day > dim {
				for day > dim {
					day -= dim
					month++
					if month == 13 {
						month = 1
						year++
						if year > maxYear {
							return
						}
					}
					dim = daysIn(year, month)
				}
				e.rebuild(year, month)
			}
		}
	}
}
