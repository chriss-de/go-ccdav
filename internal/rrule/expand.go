package rrule

import (
	"container/heap"
	"iter"
	"slices"
	"strings"
	"time"

	"davd/internal/ical"
)

// Occurrence is one instance of a (possibly recurring) component.
type Occurrence struct {
	RecurrenceID time.Time       // the instance's original start (as generated / as in the override's RECURRENCE-ID)
	Start, End   time.Time       // End >= Start
	AllDay       bool            // DTSTART is a DATE
	Comp         *ical.Component // master component, or the override (RECURRENCE-ID) component for that instance
}

// maxScan bounds the number of generated instances examined per series, so
// that e.g. a SECONDLY rule starting decades before the query range cannot
// stall a request.
const maxScan = 1_000_000

// Expand yields, in ascending Start order, all instances of the components
// named compName ("VEVENT", "VTODO", "VJOURNAL") with the given uid (uid ==
// "" means every UID in cal) that overlap [from, to). A zero from or to
// means unbounded on that side.
//
// Instance end: VEVENT DTEND, else DTSTART+DURATION, else (all-day: +1
// day, otherwise == Start). VTODO: DUE, else DTSTART+DURATION, else Start.
// VJOURNAL: all-day +1 day, else Start. A DTEND/DUE is applied as an exact
// duration for timed and as a number of days for all-day components; a
// DURATION keeps nominal days on the wall clock.
//
// Overlap: Start < to && (End > from || (End == Start && Start >= from)).
//
// The recurrence set is DTSTART ∪ RRULEs ∪ RDATEs (a PERIOD's end overrides
// the duration) minus EXDATEs (EXRULE is not supported). EXDATE and
// RECURRENCE-ID values are matched to generated instances by UTC instant,
// or by date for all-day series (a DATE EXDATE on a timed series removes
// every instance on that local date).
//
// Overrides (same UID with RECURRENCE-ID) replace the generated instance and
// use their own DTSTART/DTEND/DURATION (if an override has neither DTEND/DUE
// nor DURATION, the master's duration is used). Overrides are always
// yielded, even if their RECURRENCE-ID is not part of the generated set or
// is excluded by EXDATE (like sabre/vobject). RANGE=THISANDFUTURE applies
// the override's start shift, duration and component to all later
// generated instances, until a later THISANDFUTURE override.
//
// Components without DTSTART yield nothing; a master that cannot be parsed
// yields nothing but its overrides are still yielded. An unparsable RRULE is
// ignored (the master is treated as non-recurring).
func Expand(cal *ical.Component, compName, uid string, from, to time.Time, floating *time.Location) iter.Seq[Occurrence] {
	return func(yield func(Occurrence) bool) {
		if cal == nil {
			return
		}
		if floating == nil {
			floating = time.UTC
		}
		groups := groupComponents(cal, compName, uid)
		x := &expansion{cal: cal, name: compName, from: from, to: to, floating: floating}
		if len(groups) == 1 {
			for o := range x.group(groups[0]) {
				if !yield(o) {
					return
				}
			}
			return
		}
		// k-way merge of the per-series streams.
		var h mergeHeap
		defer func() {
			for _, s := range h {
				s.stop()
			}
		}()
		for i, g := range groups {
			next, stop := iter.Pull(x.group(g))
			if o, ok := next(); ok {
				h = append(h, &mergeSrc{next: next, stop: stop, cur: o, idx: i})
			} else {
				stop()
			}
		}
		heap.Init(&h)
		for len(h) > 0 {
			s := h[0]
			if !yield(s.cur) {
				return
			}
			if o, ok := s.next(); ok {
				s.cur = o
				heap.Fix(&h, 0)
			} else {
				s.stop()
				heap.Pop(&h)
			}
		}
	}
}

type mergeSrc struct {
	next func() (Occurrence, bool)
	stop func()
	cur  Occurrence
	idx  int
}

type mergeHeap []*mergeSrc

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if c := h[i].cur.Start.Compare(h[j].cur.Start); c != 0 {
		return c < 0
	}
	return h[i].idx < h[j].idx
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(*mergeSrc)) }
func (h *mergeHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// series is one UID: a master (may be nil) and its overrides.
type series struct {
	master    *ical.Component
	overrides []*ical.Component
}

func groupComponents(cal *ical.Component, name, uid string) []*series {
	var out []*series
	byUID := map[string]*series{}
	for _, c := range cal.Children {
		if c.Name != name {
			continue
		}
		u := strings.TrimSpace(c.Value("UID"))
		if uid != "" && u != uid {
			continue
		}
		isOverride := c.Prop("RECURRENCE-ID") != nil
		s := byUID[u]
		if s == nil || u == "" || (!isOverride && s.master != nil) {
			// New series; duplicate masters become their own series.
			s = &series{}
			out = append(out, s)
			if u != "" && byUID[u] == nil {
				byUID[u] = s
			}
		}
		if isOverride {
			s.overrides = append(s.overrides, c)
		} else {
			s.master = c
		}
	}
	return out
}

type expansion struct {
	cal      *ical.Component
	name     string
	from, to time.Time
	floating *time.Location
}

func (x *expansion) overlaps(o Occurrence) bool {
	if !x.to.IsZero() && !o.Start.Before(x.to) {
		return false
	}
	if x.from.IsZero() {
		return true
	}
	return o.End.After(x.from) || (o.End.Equal(o.Start) && !o.Start.Before(x.from))
}

// span describes how a component's end follows from its start.
type span struct {
	start  DateTime
	end    func(time.Time) time.Time
	hasEnd bool // DTEND/DUE/DURATION present
}

func (x *expansion) span(c *ical.Component) (span, bool) {
	sp, err := ParseDateTime(c.Prop("DTSTART"), x.cal, x.floating)
	if err != nil {
		return span{}, false
	}
	s := span{start: sp, end: func(t time.Time) time.Time { return t }}
	oneDay := func(t time.Time) time.Time { return Duration{Days: 1}.AddTo(t) }
	endProp := ""
	switch x.name {
	case "VTODO":
		endProp = "DUE"
	case "VJOURNAL":
	default:
		endProp = "DTEND"
	}
	if endProp != "" {
		if ep := c.Prop(endProp); ep != nil {
			if e, err := ParseDateTime(ep, x.cal, x.floating); err == nil {
				s.hasEnd = true
				if sp.AllDay {
					ys, ms, ds := sp.Time.Date()
					ye, me, de := e.Time.Date()
					days := max(daysFromCivil(ye, int(me), de)-daysFromCivil(ys, int(ms), ds), 0)
					s.end = func(t time.Time) time.Time { return Duration{Days: days}.AddTo(t) }
				} else {
					d := max(e.Time.Sub(sp.Time), 0)
					s.end = func(t time.Time) time.Time { return t.Add(d) }
				}
				return s, true
			}
		}
	}
	if x.name != "VJOURNAL" {
		if dp := c.Prop("DURATION"); dp != nil {
			if d, err := ParseDuration(dp.Value); err == nil && !d.Neg {
				s.hasEnd = true
				s.end = d.AddTo
				return s, true
			}
		}
	}
	if sp.AllDay && x.name != "VTODO" {
		s.end = oneDay
	}
	return s, true
}

// override is a parsed RECURRENCE-ID component.
type override struct {
	comp    *ical.Component
	rid     DateTime
	future  bool // RANGE=THISANDFUTURE
	occ     Occurrence
	hasOcc  bool
	shift   Duration // THISANDFUTURE: RECURRENCE-ID -> DTSTART
	shiftAt time.Time
	end     func(time.Time) time.Time
}

func dateKey(t time.Time) int64 {
	y, m, d := t.Date()
	return int64(daysFromCivil(y, int(m), d))
}

func (x *expansion) group(s *series) iter.Seq[Occurrence] {
	return func(yield func(Occurrence) bool) {
		var (
			master     span
			haveMaster bool
		)
		if s.master != nil {
			master, haveMaster = x.span(s.master)
		}
		allDay := haveMaster && master.start.AllDay

		// Pre-collect overrides: they are yielded on their own, and the
		// generated instances they replace are skipped.
		var h occHeap
		byInstant := map[int64]bool{}
		byDate := map[int64]bool{}
		var future []*override
		for _, c := range s.overrides {
			rid, err := ParseDateTime(c.Prop("RECURRENCE-ID"), x.cal, x.floating)
			if err != nil {
				continue
			}
			o := &override{comp: c, rid: rid, future: strings.EqualFold(c.Prop("RECURRENCE-ID").Param("RANGE"), "THISANDFUTURE")}
			sp, ok := x.span(c)
			if !ok {
				// No DTSTART: the instance keeps its original start.
				sp = span{start: rid, end: func(t time.Time) time.Time { return t }}
			}
			o.end = sp.end
			if !sp.hasEnd && haveMaster {
				o.end = master.end
			}
			start := sp.start.Time
			o.occ = Occurrence{RecurrenceID: rid.Time, Start: start, End: maxTime(start, o.end(start)), AllDay: sp.start.AllDay, Comp: c}
			heap.Push(&h, o.occ)
			if allDay || rid.AllDay {
				byDate[dateKey(rid.Time)] = true
			} else {
				byInstant[rid.Time.Unix()] = true
			}
			if o.future && haveMaster {
				loc := master.start.Time.Location()
				a, b := civilOf(rid.Time.In(loc)), civilOf(start.In(loc))
				o.shift = Duration{
					Days:  daysFromCivil(b.y, b.m, b.d) - daysFromCivil(a.y, a.m, a.d),
					Clock: time.Duration((b.h*3600+b.mi*60+b.s)-(a.h*3600+a.mi*60+a.s)) * time.Second,
				}
				o.shiftAt = rid.Time
				future = append(future, o)
			}
		}
		slices.SortFunc(future, func(a, b *override) int { return a.shiftAt.Compare(b.shiftAt) })
		// Lower bound of how far THISANDFUTURE moves instances back in time
		// (with slack for DST), so the scan does not stop too early.
		var minShift time.Duration
		for _, f := range future {
			minShift = min(minShift, f.occ.Start.Sub(f.rid.Time)-3*time.Hour)
		}

		// emit yields the pending occurrences that start before bound (all
		// of them if all is set). It returns false when the consumer
		// stopped or the end of the range was reached.
		emit := func(bound time.Time, all bool) bool {
			for h.Len() > 0 && (all || h[0].Start.Before(bound)) {
				o := heap.Pop(&h).(Occurrence)
				if !x.to.IsZero() && !o.Start.Before(x.to) {
					return false
				}
				if x.overlaps(o) && !yield(o) {
					return false
				}
			}
			return true
		}

		if haveMaster {
			// Exclusions.
			exInstant := map[int64]bool{}
			exDate := map[int64]bool{}
			for _, p := range s.master.PropsNamed("EXDATE") {
				list, _, err := ParseDateTimeList(p, x.cal, x.floating)
				if err != nil {
					continue
				}
				for _, d := range list {
					if allDay || d.AllDay {
						exDate[dateKey(d.Time)] = true
					} else {
						exInstant[d.Time.Unix()] = true
					}
				}
			}
			n := 0
			for g, periodEnd := range x.generate(s.master, master.start) {
				if n++; n > maxScan {
					break
				}
				bound := g.Add(minShift)
				if !emit(bound, false) {
					return
				}
				if !x.to.IsZero() && !bound.Before(x.to) {
					break
				}
				dk := dateKey(g)
				if exInstant[g.Unix()] || exDate[dk] {
					continue
				}
				if byDate[dk] || (!allDay && byInstant[g.Unix()]) {
					continue
				}
				occ := Occurrence{RecurrenceID: g, Start: g, AllDay: allDay, Comp: s.master}
				if !periodEnd.IsZero() {
					occ.End = periodEnd
				} else {
					occ.End = master.end(g)
				}
				// Latest THISANDFUTURE override at or before g.
				for i := len(future) - 1; i >= 0; i-- {
					if !future[i].shiftAt.After(g) {
						f := future[i]
						occ.Start = f.shift.AddTo(g)
						occ.End = f.end(occ.Start)
						occ.Comp = f.comp
						occ.AllDay = f.occ.AllDay
						break
					}
				}
				occ.End = maxTime(occ.Start, occ.End)
				heap.Push(&h, occ)
			}
		}
		emit(time.Time{}, true)
	}
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// generate yields the recurrence set of a master in ascending order:
// DTSTART, RRULE instances and RDATEs (with the RDATE PERIOD end, or zero).
func (x *expansion) generate(c *ical.Component, start DateTime) iter.Seq2[time.Time, time.Time] {
	return func(yield func(time.Time, time.Time) bool) {
		type rdate struct{ start, end time.Time }
		var rdates []rdate
		for _, p := range c.PropsNamed("RDATE") {
			starts, ends, err := ParseDateTimeList(p, x.cal, x.floating)
			if err != nil {
				continue
			}
			for i, s := range starts {
				t := s.Time
				if start.AllDay && !s.AllDay {
					// Keep all-day series on midnight of the given date.
					t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, start.Time.Location())
				}
				rdates = append(rdates, rdate{t, ends[i]})
			}
		}
		slices.SortStableFunc(rdates, func(a, b rdate) int { return a.start.Compare(b.start) })

		var (
			pulls []func() (time.Time, bool)
			heads []time.Time
			alive []bool // a zero time.Time is a valid instant (0001-01-01 UTC)
		)
		for _, p := range c.PropsNamed("RRULE") {
			r, err := Parse(p.Value)
			if err != nil {
				continue
			}
			next, stop := iter.Pull(r.Iter(start.Time))
			defer stop()
			if t, ok := next(); ok {
				pulls = append(pulls, next)
				heads = append(heads, t)
				alive = append(alive, true)
			}
		}
		if len(pulls) == 0 {
			// Not recurring by rule: DTSTART is still part of the set.
			// Appended last so that an RDATE PERIOD at the same instant
			// (sorted before it) keeps its end.
			rdates = append(rdates, rdate{start: start.Time})
			slices.SortStableFunc(rdates, func(a, b rdate) int { return a.start.Compare(b.start) })
		}
		var last time.Time
		first := true
		for {
			// Pick the smallest head among rules and RDATEs.
			best := -1
			for i, t := range heads {
				if alive[i] && (best < 0 || t.Before(heads[best])) {
					best = i
				}
			}
			var t, end time.Time
			switch {
			case len(rdates) > 0 && (best < 0 || !heads[best].Before(rdates[0].start)):
				t, end = rdates[0].start, rdates[0].end
				rdates = rdates[1:]
			case best >= 0:
				t = heads[best]
				heads[best], alive[best] = pulls[best]()
			default:
				return
			}
			if !first && !t.After(last) {
				continue // duplicate
			}
			first, last = false, t
			if !yield(t, end) {
				return
			}
		}
	}
}

// occHeap orders occurrences by start, then recurrence id.
type occHeap []Occurrence

func (h occHeap) Len() int { return len(h) }
func (h occHeap) Less(i, j int) bool {
	if c := h[i].Start.Compare(h[j].Start); c != 0 {
		return c < 0
	}
	return h[i].RecurrenceID.Before(h[j].RecurrenceID)
}
func (h occHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *occHeap) Push(x any)   { *h = append(*h, x.(Occurrence)) }
func (h *occHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
