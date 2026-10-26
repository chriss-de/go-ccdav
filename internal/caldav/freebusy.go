package caldav

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/rrule"
)

// Free/busy types (RFC 5545 §3.2.9).
const (
	FBBusy          = "BUSY"
	FBBusyTentative = "BUSY-TENTATIVE"
)

// Period is a busy interval.
type Period struct {
	Start, End time.Time
	Type       string
}

// MaxFreeBusyPeriods bounds the busy periods of one free/busy answer
// after merging.
const MaxFreeBusyPeriods = 50_000

// ErrFreeBusyTooLarge is returned when there are more busy periods than
// MaxFreeBusyPeriods.
var ErrFreeBusyTooLarge = errors.New("caldav: too many busy periods")

// FreeBusy computes the busy periods of the given calendars within
// [from, to) from their VEVENTs, recurrences expanded (RFC 4791 §7.10).
// Transparent, cancelled and zero-length instances don't block time;
// tentative ones are BUSY-TENTATIVE. Periods of the same type are merged.
// When ctx is done, FreeBusy stops and fails with its error.
func FreeBusy(ctx context.Context, cals []*ical.Component, from, to time.Time, floating *time.Location) ([]Period, error) {
	var ps []Period
	for _, cal := range cals {
		for o := range rrule.ExpandContext(ctx, cal, "VEVENT", "", from, to, floating) {
			c := o.Comp
			if strings.EqualFold(c.Value("TRANSP"), "TRANSPARENT") || strings.EqualFold(c.Value("STATUS"), "CANCELLED") {
				continue
			}
			start, end := o.Start, o.End
			if start.Before(from) {
				start = from
			}
			if end.After(to) {
				end = to
			}
			if !end.After(start) {
				continue
			}
			typ := FBBusy
			if strings.EqualFold(c.Value("STATUS"), "TENTATIVE") {
				typ = FBBusyTentative
			}
			ps = append(ps, Period{start.UTC(), end.UTC(), typ})
			if len(ps) >= 2*MaxFreeBusyPeriods {
				if ps = mergePeriods(ps); len(ps) > MaxFreeBusyPeriods {
					return nil, ErrFreeBusyTooLarge
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ps = mergePeriods(ps); len(ps) > MaxFreeBusyPeriods {
		return nil, ErrFreeBusyTooLarge
	}
	return ps, nil
}

func mergePeriods(ps []Period) []Period {
	slices.SortFunc(ps, func(a, b Period) int {
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return a.Start.Compare(b.Start)
	})
	var out []Period
	for _, p := range ps {
		if n := len(out); n > 0 && out[n-1].Type == p.Type && !p.Start.After(out[n-1].End) {
			if p.End.After(out[n-1].End) {
				out[n-1].End = p.End
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

// FreeBusyCalendar renders periods as a VCALENDAR with one VFREEBUSY.
func FreeBusyCalendar(ps []Period, from, to, now time.Time) *ical.Component {
	fb := &ical.Component{Name: "VFREEBUSY", Props: []*ical.Prop{
		{Name: "DTSTAMP", Value: now.UTC().Format(utcFormat)},
		{Name: "DTSTART", Value: from.UTC().Format(utcFormat)},
		{Name: "DTEND", Value: to.UTC().Format(utcFormat)},
	}}
	byType := map[string][]string{}
	var types []string
	for _, p := range ps {
		if _, ok := byType[p.Type]; !ok {
			types = append(types, p.Type)
		}
		byType[p.Type] = append(byType[p.Type], p.Start.Format(utcFormat)+"/"+p.End.Format(utcFormat))
	}
	for _, t := range types {
		fb.Props = append(fb.Props, &ical.Prop{Name: "FREEBUSY",
			Params: []ical.Param{{Name: "FBTYPE", Values: []string{t}}}, Value: strings.Join(byType[t], ",")})
	}
	return &ical.Component{Name: "VCALENDAR", Props: []*ical.Prop{
		{Name: "VERSION", Value: "2.0"},
		{Name: "PRODID", Value: "-//ccdav//free-busy//EN"},
	}, Children: []*ical.Component{fb}}
}
