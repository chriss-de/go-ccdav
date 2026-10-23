package rrule

import "time"

// Proleptic Gregorian calendar helpers working on plain integers, so the
// expansion loop never has to round-trip through time.Time.

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

func yearLen(y int) int {
	if isLeap(y) {
		return 366
	}
	return 365
}

var monthDays = [13]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func daysIn(y, m int) int {
	if m == 2 && isLeap(y) {
		return 29
	}
	return monthDays[m]
}

// daysFromCivil returns the number of days since 1970-01-01 (H. Hinnant).
func daysFromCivil(y, m, d int) int {
	if m <= 2 {
		y--
	}
	era := floorDiv(y, 400)
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// civilFromDays is the inverse of daysFromCivil.
func civilFromDays(z int) (y, m, d int) {
	z += 719468
	era := floorDiv(z, 146097)
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if m > 12 {
		m -= 12
	}
	if m <= 2 {
		y++
	}
	return
}

// weekdayOf returns the weekday of a day number with Monday = 0 (the
// python-dateutil convention used throughout the expansion code).
func weekdayOf(days int) int { return pyMod(days+3, 7) }

// monWeekday converts time.Weekday (Sunday = 0) to Monday = 0.
func monWeekday(w time.Weekday) int { return (int(w) + 6) % 7 }

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func pyMod(a, b int) int {
	m := a % b
	if m != 0 && (m < 0) != (b < 0) {
		m += b
	}
	return m
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// civil is a wall-clock date and time without a location.
type civil struct {
	y, m, d, h, mi, s int
}

func civilOf(t time.Time) civil {
	y, m, d := t.Date()
	h, mi, s := t.Clock()
	return civil{y, int(m), d, h, mi, s}
}

// key orders civil values; it is seconds since 1970-01-01 wall clock.
func (c civil) key() int64 {
	return int64(daysFromCivil(c.y, c.m, c.d))*86400 + int64(c.h*3600+c.mi*60+c.s)
}

// resolve converts wall-clock fields to an instant in loc following RFC 5545
// section 3.3.5: a time that falls into a DST gap is interpreted with the
// offset in effect before the gap (so 02:30 on a spring-forward day becomes
// 03:30 after the jump); an ambiguous time during fall-back refers to its
// first occurrence. time.Date alone does not guarantee either.
func resolve(c civil, loc *time.Location) time.Time {
	t := time.Date(c.y, time.Month(c.m), c.d, c.h, c.mi, c.s, 0, loc)
	if loc == time.UTC {
		return t
	}
	naive := c.key()
	_, offBefore := time.Unix(naive-86400, 0).In(loc).Zone()
	_, offAfter := time.Unix(naive+86400, 0).In(loc).Zone()
	if offBefore == offAfter {
		_, off := t.Zone()
		if off == offBefore {
			return t // no transition nearby: fast path
		}
	}
	valid := func(off int) (int64, bool) {
		u := naive - int64(off)
		_, o := time.Unix(u, 0).In(loc).Zone()
		return u, o == off
	}
	u1, ok1 := valid(offBefore)
	u2, ok2 := valid(offAfter)
	var u int64
	switch {
	case ok1 && ok2:
		u = min(u1, u2) // ambiguous: first occurrence
	case ok1:
		u = u1
	case ok2:
		u = u2
	default:
		u = u1 // gap: offset before the gap
	}
	return time.Unix(u, 0).In(loc)
}
