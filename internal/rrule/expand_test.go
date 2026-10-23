package rrule

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"davd/internal/ical"
)

func loadCal(t testing.TB, name string) *ical.Component {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return parseCal(t, string(b))
}

func parseCal(t testing.TB, s string) *ical.Component {
	t.Helper()
	s = strings.ReplaceAll(s, "\r\n", "\n")
	c, err := ical.ParseBytes([]byte(strings.ReplaceAll(s, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func utc(s string) time.Time {
	t, err := time.Parse("20060102T150405Z", s)
	if err != nil {
		panic(err)
	}
	return t
}

// fmtOcc renders an occurrence as "start/end rid summary" in UTC.
func fmtOcc(o Occurrence) string {
	f := func(t time.Time) string { return t.UTC().Format("20060102T150405Z") }
	s := f(o.Start) + "/" + f(o.End) + " rid=" + f(o.RecurrenceID)
	if o.AllDay {
		s += " allday"
	}
	return s + " " + o.Comp.Prop("SUMMARY").Text()
}

func collect(cal *ical.Component, comp, uid string, from, to time.Time, floating *time.Location) []string {
	var out []string
	for o := range Expand(cal, comp, uid, from, to, floating) {
		out = append(out, fmtOcc(o))
	}
	return out
}

func check(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("got:\n\t%s\nwant:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
}

var (
	wide0 = utc("19000101T000000Z")
	wide1 = utc("21000101T000000Z")
)

func TestExpandMovedAndDeleted(t *testing.T) {
	cal := loadCal(t, "moved_deleted.ics")
	check(t, collect(cal, "VEVENT", "weekly-standup@example.com", wide0, wide1, nil), []string{
		"20251006T080000Z/20251006T083000Z rid=20251006T080000Z Standup",
		"20251014T130000Z/20251014T140000Z rid=20251013T080000Z Standup (moved)",
		// 20 Oct is excluded; after the DST change the wall clock stays 10:00.
		"20251027T090000Z/20251027T093000Z rid=20251027T090000Z Standup",
		"20251031T080000Z/20251031T083000Z rid=20251103T090000Z Standup (moved earlier, RECURRENCE-ID in UTC)",
		"20251110T090000Z/20251110T093000Z rid=20251110T090000Z Standup",
	})
	// The moved instance is found by its new time, not its original one.
	check(t, collect(cal, "VEVENT", "", utc("20251013T000000Z"), utc("20251014T000000Z"), nil), nil)
	check(t, collect(cal, "VEVENT", "", utc("20251014T000000Z"), utc("20251015T000000Z"), nil), []string{
		"20251014T130000Z/20251014T140000Z rid=20251013T080000Z Standup (moved)",
	})
	// An override moved into the range from a later instance.
	check(t, collect(cal, "VEVENT", "", utc("20251030T000000Z"), utc("20251101T000000Z"), nil), []string{
		"20251031T080000Z/20251031T083000Z rid=20251103T090000Z Standup (moved earlier, RECURRENCE-ID in UTC)",
	})
	check(t, collect(cal, "VEVENT", "other-uid", wide0, wide1, nil), nil)
}

func TestExpandThisAndFuture(t *testing.T) {
	cal := loadCal(t, "thisandfuture.ics")
	check(t, collect(cal, "VEVENT", "", wide0, wide1, nil), []string{
		"20250303T140000Z/20250303T150000Z rid=20250303T140000Z Daily",
		"20250304T140000Z/20250304T150000Z rid=20250304T140000Z Daily",
		"20250305T140000Z/20250305T150000Z rid=20250305T140000Z Daily",
		"20250306T160000Z/20250306T163000Z rid=20250306T140000Z Daily (later, shorter)",
		"20250307T160000Z/20250307T163000Z rid=20250307T140000Z Daily (later, shorter)",
		"20250308T190000Z/20250308T200000Z rid=20250308T140000Z Single override inside the future range",
		// NY switches to EDT on 9 March: still 11:00 local.
		"20250309T150000Z/20250309T153000Z rid=20250309T130000Z Daily (later, shorter)",
		"20250310T150000Z/20250310T153000Z rid=20250310T130000Z Daily (later, shorter)",
	})
	// Querying only the shifted time finds the shifted instance.
	check(t, collect(cal, "VEVENT", "", utc("20250307T155900Z"), utc("20250307T160100Z"), nil), []string{
		"20250307T160000Z/20250307T163000Z rid=20250307T140000Z Daily (later, shorter)",
	})
	check(t, collect(cal, "VEVENT", "", utc("20250307T140000Z"), utc("20250307T150000Z"), nil), nil)
}

func TestExpandThisAndFutureEarlier(t *testing.T) {
	// A THISANDFUTURE override that moves the rest of the series a day
	// earlier must still be found when the scan bound is close.
	cal := parseCal(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:x
DTSTART:20250101T100000Z
DTEND:20250101T110000Z
RRULE:FREQ=WEEKLY;COUNT=5
SUMMARY:A
END:VEVENT
BEGIN:VEVENT
UID:x
RECURRENCE-ID;RANGE=THISANDFUTURE:20250115T100000Z
DTSTART:20250114T100000Z
DTEND:20250114T110000Z
SUMMARY:B
END:VEVENT
END:VCALENDAR
`)
	check(t, collect(cal, "VEVENT", "", utc("20250120T000000Z"), utc("20250122T000000Z"), nil), []string{
		"20250121T100000Z/20250121T110000Z rid=20250122T100000Z B",
	})
	check(t, collect(cal, "VEVENT", "", wide0, wide1, nil), []string{
		"20250101T100000Z/20250101T110000Z rid=20250101T100000Z A",
		"20250108T100000Z/20250108T110000Z rid=20250108T100000Z A",
		"20250114T100000Z/20250114T110000Z rid=20250115T100000Z B",
		"20250121T100000Z/20250121T110000Z rid=20250122T100000Z B",
		"20250128T100000Z/20250128T110000Z rid=20250129T100000Z B",
	})
}

func TestExpandAllDay(t *testing.T) {
	cal := loadCal(t, "allday.ics")
	check(t, collect(cal, "VEVENT", "leap-birthday@example.com", utc("20240101T000000Z"), utc("20330101T000000Z"), nil), []string{
		"20240229T000000Z/20240301T000000Z rid=20240229T000000Z allday Leap day birthday",
		"20280229T000000Z/20280301T000000Z rid=20280229T000000Z allday Leap day birthday",
		"20320229T000000Z/20320301T000000Z rid=20320229T000000Z allday Leap day birthday",
	})
	check(t, collect(cal, "VEVENT", "vacation@example.com", wide0, wide1, nil), []string{
		"20251020T000000Z/20251022T000000Z rid=20251020T000000Z allday Two-day block every week",
		"20251104T000000Z/20251107T000000Z rid=20251103T000000Z allday Longer block",
		"20251110T000000Z/20251112T000000Z rid=20251110T000000Z allday Two-day block every week",
	})
	// All-day values are midnight in the floating location.
	berlin, _ := time.LoadLocation("Europe/Berlin")
	var got []Occurrence
	for o := range Expand(cal, "VEVENT", "vacation@example.com", wide0, wide1, berlin) {
		got = append(got, o)
	}
	if len(got) != 3 || !got[0].Start.Equal(time.Date(2025, 10, 20, 0, 0, 0, 0, berlin)) ||
		!got[2].End.Equal(time.Date(2025, 11, 12, 0, 0, 0, 0, berlin)) || !got[0].AllDay {
		t.Errorf("floating all-day: %+v", got)
	}
	// The second day of a two-day block overlaps.
	check(t, collect(cal, "VEVENT", "vacation@example.com", utc("20251021T120000Z"), utc("20251021T130000Z"), nil), []string{
		"20251020T000000Z/20251022T000000Z rid=20251020T000000Z allday Two-day block every week",
	})
}

func TestExpandDST(t *testing.T) {
	cal := loadCal(t, "dst.ics")
	check(t, collect(cal, "VEVENT", "ny-weekly@example.com", wide0, wide1, nil), []string{
		"20251020T130000Z/20251020T140000Z rid=20251020T130000Z NY weekly",
		"20251027T130000Z/20251027T140000Z rid=20251027T130000Z NY weekly",
		"20251103T140000Z/20251103T150000Z rid=20251103T140000Z NY weekly",
		"20251110T140000Z/20251110T150000Z rid=20251110T140000Z NY weekly",
	})
	// 02:00 on 30 March does not exist in Berlin: shifted to 03:00 CEST.
	check(t, collect(cal, "VEVENT", "berlin-daily-night@example.com", wide0, wide1, nil), []string{
		"20250329T010000Z/20250329T030000Z rid=20250329T010000Z Night shift (crosses the spring gap)",
		"20250330T010000Z/20250330T030000Z rid=20250330T010000Z Night shift (crosses the spring gap)",
		"20250331T000000Z/20250331T020000Z rid=20250331T000000Z Night shift (crosses the spring gap)",
	})
}

func TestExpandRDatePeriod(t *testing.T) {
	cal := loadCal(t, "rdate_period.ics")
	check(t, collect(cal, "VEVENT", "", wide0, wide1, nil), []string{
		"20250101T100000Z/20250101T110000Z rid=20250101T100000Z Irregular",
		"20250103T080000Z/20250103T081500Z rid=20250103T080000Z Irregular",
		"20250105T120000Z/20250105T180000Z rid=20250105T120000Z Irregular",
	})
	check(t, collect(cal, "VEVENT", "", utc("20250105T170000Z"), utc("20250106T000000Z"), nil), []string{
		"20250105T120000Z/20250105T180000Z rid=20250105T120000Z Irregular",
	})
}

func TestExpandFloating(t *testing.T) {
	cal := loadCal(t, "floating.ics")
	check(t, collect(cal, "VJOURNAL", "", wide0, wide1, nil), []string{
		"20250310T000000Z/20250311T000000Z rid=20250310T000000Z allday All-day journal",
		"20250310T083000Z/20250310T083000Z rid=20250310T083000Z Floating journal",
		"20250312T083000Z/20250312T083000Z rid=20250312T083000Z Floating journal",
	})
	berlin, _ := time.LoadLocation("Europe/Berlin")
	check(t, collect(cal, "VJOURNAL", "journal@example.com", wide0, wide1, berlin), []string{
		"20250310T073000Z/20250310T073000Z rid=20250310T073000Z Floating journal",
		"20250312T073000Z/20250312T073000Z rid=20250312T073000Z Floating journal",
	})
	ny, _ := time.LoadLocation("America/New_York")
	check(t, collect(cal, "VJOURNAL", "journal@example.com", wide0, wide1, ny), []string{
		"20250310T123000Z/20250310T123000Z rid=20250310T123000Z Floating journal",
		"20250312T123000Z/20250312T123000Z rid=20250312T123000Z Floating journal",
	})
}

func TestExpandOutlookWindowsTZID(t *testing.T) {
	cal := loadCal(t, "outlook.ics")
	check(t, collect(cal, "VEVENT", "", wide0, wide1, nil), []string{
		"20250127T150000Z/20250127T160000Z rid=20250127T150000Z Monthly review",
		"20250224T150000Z/20250224T160000Z rid=20250224T150000Z Monthly review",
		"20250331T140000Z/20250331T150000Z rid=20250331T140000Z Monthly review",
		"20250526T140000Z/20250526T150000Z rid=20250526T140000Z Monthly review",
		"20250630T140000Z/20250630T150000Z rid=20250630T140000Z Monthly review",
	})
}

func TestExpandCustomVTimezone(t *testing.T) {
	cal := loadCal(t, "customtz.ics")
	check(t, collect(cal, "VEVENT", "", wide0, wide1, nil), []string{
		"20250320T060000Z/20250320T063000Z rid=20250320T060000Z Custom zone weekly",
		"20250327T060000Z/20250327T063000Z rid=20250327T060000Z Custom zone weekly",
		"20250403T050000Z/20250403T053000Z rid=20250403T050000Z Custom zone weekly",
	})
	// Without the VTIMEZONE the TZID cannot be resolved: UTC.
	loc, ok := LoadLocation("My Zone", nil)
	if ok || loc != time.UTC {
		t.Errorf("unknown zone: %v %v", loc, ok)
	}
}

func TestExpandVTodo(t *testing.T) {
	cal := loadCal(t, "vtodo.ics")
	check(t, collect(cal, "VTODO", "", wide0, wide1, nil), []string{
		"20251020T070000Z/20251022T150000Z rid=20251020T070000Z Weekly report",
		"20251021T120000Z/20251021T120000Z rid=20251021T120000Z Start only",
		"20251027T080000Z/20251029T160000Z rid=20251027T080000Z Weekly report",
		"20251103T080000Z/20251105T160000Z rid=20251103T080000Z Weekly report",
	})
	// VTODO components are not VEVENTs.
	check(t, collect(cal, "VEVENT", "", wide0, wide1, nil), nil)
}

func TestExpandOverlapEdges(t *testing.T) {
	cal := parseCal(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:zero
DTSTART:20250101T100000Z
SUMMARY:zero-length
END:VEVENT
BEGIN:VEVENT
UID:hour
DTSTART:20250101T100000Z
DTEND:20250101T110000Z
SUMMARY:one hour
END:VEVENT
END:VCALENDAR
`)
	tests := []struct {
		from, to string
		want     []string
	}{
		// Zero-length at from: included. One hour starting at from: included.
		{"20250101T100000Z", "20250101T120000Z", []string{"zero", "hour"}},
		// Range ends exactly at the start: neither.
		{"20250101T090000Z", "20250101T100000Z", nil},
		// Range starts at the end of the hour: hour excluded (End == from), zero-length excluded (Start < from).
		{"20250101T110000Z", "20250101T120000Z", nil},
		// Range inside the hour, after the zero-length event.
		{"20250101T103000Z", "20250101T104000Z", []string{"hour"}},
		// Range starting one second after the zero-length event.
		{"20250101T100001Z", "20250101T110000Z", []string{"hour"}},
		// Unbounded ranges.
		{"", "", []string{"zero", "hour"}},
		{"20250101T100001Z", "", []string{"hour"}},
		{"", "20250101T100000Z", nil},
	}
	for _, tc := range tests {
		var from, to time.Time
		if tc.from != "" {
			from = utc(tc.from)
		}
		if tc.to != "" {
			to = utc(tc.to)
		}
		var got []string
		for o := range Expand(cal, "VEVENT", "", from, to, nil) {
			got = append(got, o.Comp.Value("UID"))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("[%s,%s): got %v want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestExpandMisc(t *testing.T) {
	cal := parseCal(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:a
DTSTART:20250101T090000Z
DURATION:PT1H
RRULE:FREQ=DAILY
SUMMARY:A daily forever
END:VEVENT
BEGIN:VEVENT
UID:b
DTSTART:20250101T093000Z
DURATION:PT1H
RRULE:FREQ=DAILY;INTERVAL=2;COUNT=3
EXDATE;VALUE=DATE:20250103
SUMMARY:B
END:VEVENT
BEGIN:VEVENT
UID:c
RECURRENCE-ID:20250102T120000Z
DTSTART:20250102T120000Z
SUMMARY:C orphan override
END:VEVENT
BEGIN:VEVENT
UID:d
DTSTART:20250101T000000Z
RRULE:FREQ=MONTHLY;BYMONTHDAY=31;BYMONTH=2
SUMMARY:D impossible
END:VEVENT
BEGIN:VEVENT
UID:e
DTSTART:20250101T000000Z
RRULE:THIS IS NOT A RULE
SUMMARY:E broken rule
END:VEVENT
BEGIN:VEVENT
UID:f
SUMMARY:F no dtstart
RRULE:FREQ=DAILY
END:VEVENT
BEGIN:VEVENT
UID:g
DTSTART:20250101T080000Z
RRULE:FREQ=DAILY;COUNT=2
RRULE:FREQ=DAILY;INTERVAL=2;BYHOUR=7;COUNT=2
SUMMARY:G two rules
END:VEVENT
END:VCALENDAR
`)
	// Multiple UIDs are merged in start order; an unbounded rule ends at to.
	check(t, collect(cal, "VEVENT", "", utc("20250101T000000Z"), utc("20250104T000000Z"), nil), []string{
		"20250101T000000Z/20250101T000000Z rid=20250101T000000Z D impossible",
		"20250101T000000Z/20250101T000000Z rid=20250101T000000Z E broken rule",
		"20250101T080000Z/20250101T080000Z rid=20250101T080000Z G two rules",
		"20250101T090000Z/20250101T100000Z rid=20250101T090000Z A daily forever",
		"20250101T093000Z/20250101T103000Z rid=20250101T093000Z B",
		"20250102T080000Z/20250102T080000Z rid=20250102T080000Z G two rules",
		"20250102T090000Z/20250102T100000Z rid=20250102T090000Z A daily forever",
		"20250102T120000Z/20250102T120000Z rid=20250102T120000Z C orphan override",
		"20250103T070000Z/20250103T070000Z rid=20250103T070000Z G two rules",
		"20250103T090000Z/20250103T100000Z rid=20250103T090000Z A daily forever",
	})
	// A far-away range on an unbounded series terminates quickly.
	t0 := time.Now()
	n := 0
	for range Expand(cal, "VEVENT", "a", utc("20990101T000000Z"), utc("20990103T000000Z"), nil) {
		n++
	}
	if n != 2 || time.Since(t0) > 2*time.Second {
		t.Errorf("far range: %d occurrences in %v", n, time.Since(t0))
	}
	// Early stop by the consumer.
	n = 0
	for range Expand(cal, "VEVENT", "", time.Time{}, time.Time{}, nil) {
		n++
		if n == 50 {
			break
		}
	}
	if n != 50 {
		t.Errorf("early stop: %d", n)
	}
	if got := collect(nil, "VEVENT", "", wide0, wide1, nil); got != nil {
		t.Errorf("nil calendar: %v", got)
	}
}

// TestExpandYearOne guards against treating time.Time{} (0001-01-01 UTC,
// a valid DTSTART) as "no value".
func TestExpandYearOne(t *testing.T) {
	cal := parseCal(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:y1
DTSTART:00010101T000000Z
RRULE:FREQ=YEARLY
SUMMARY:Y1
END:VEVENT
END:VCALENDAR
`)
	check(t, collect(cal, "VEVENT", "", utc("20250101T000000Z"), utc("20260101T000000Z"), nil), []string{
		"20250101T000000Z/20250101T000000Z rid=20250101T000000Z Y1",
	})
	check(t, collect(cal, "VEVENT", "", time.Time{}, utc("00020101T000000Z"), nil), []string{
		"00010101T000000Z/00010101T000000Z rid=00010101T000000Z Y1",
	})
}

func TestLoadLocation(t *testing.T) {
	tests := []struct{ tzid, want string }{
		{"Europe/Berlin", "Europe/Berlin"},
		{"/mozilla.org/20050126_1/Europe/Berlin", "Europe/Berlin"},
		{"/softwarestudio.org/Olson_20011030_5/America/New_York", "America/New_York"},
		{"/Europe/Paris", "Europe/Paris"},
		{`"America/Chicago"`, "America/Chicago"},
		{"W. Europe Standard Time", "Europe/Berlin"},
		{"w. europe standard time", "Europe/Berlin"},
		{"Eastern Standard Time", "America/New_York"},
		{"Pacific Standard Time", "America/Los_Angeles"},
		{"GMT Standard Time", "Europe/London"},
		{"Tokyo Standard Time", "Asia/Tokyo"},
		{"(UTC+01:00) Amsterdam, Berlin, Bern, Rome, Stockholm, Vienna", "Europe/Berlin"},
		{"UTC", "UTC"},
	}
	for _, tc := range tests {
		loc, ok := LoadLocation(tc.tzid, nil)
		if !ok || loc.String() != tc.want {
			t.Errorf("LoadLocation(%q) = %v, %v; want %s", tc.tzid, loc, ok, tc.want)
		}
	}
	for _, s := range []string{"", "Local", "Nowhere/Special", "../../etc/passwd", "My Zone"} {
		if loc, ok := LoadLocation(s, nil); ok || loc != time.UTC {
			t.Errorf("LoadLocation(%q) = %v, %v; want UTC, false", s, loc, ok)
		}
	}
	for _, tc := range []struct {
		s   string
		off int
	}{{"UTC+01:00", 3600}, {"GMT-0500", -5 * 3600}, {"UTC+5", 5 * 3600}, {"(UTC-03:30) Somewhere", -(3*3600 + 1800)}} {
		loc, ok := LoadLocation(tc.s, nil)
		_, off := time.Date(2025, 1, 1, 0, 0, 0, 0, loc).Zone()
		if !ok || off != tc.off {
			t.Errorf("LoadLocation(%q): offset %d ok %v", tc.s, off, ok)
		}
	}
}

func TestWindowsZonesLoad(t *testing.T) {
	for win, iana := range windowsZones {
		if _, err := time.LoadLocation(iana); err != nil {
			t.Errorf("%s -> %s: %v", win, iana, err)
		}
	}
	if len(windowsZones) < 130 {
		t.Errorf("only %d windows zones", len(windowsZones))
	}
}

// TestVTimezoneMatchesIANA converts VTIMEZONE blocks with modern rules and
// compares their offsets with the IANA zone over many years.
func TestVTimezoneMatchesIANA(t *testing.T) {
	for _, tc := range []struct{ file, tzid, iana string }{
		{"outlook.ics", "W. Europe Standard Time", "Europe/Berlin"},
		{"moved_deleted.ics", "Europe/Berlin", "Europe/Berlin"},
	} {
		cal := loadCal(t, tc.file)
		var vtz *ical.Component
		for _, c := range cal.ChildrenNamed("VTIMEZONE") {
			if c.Value("TZID") == tc.tzid {
				vtz = c
			}
		}
		loc, err := LocationFromVTimezone(vtz)
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := time.LoadLocation(tc.iana)
		for ts := utc("19970101T000000Z"); ts.Year() < 2100; ts = ts.Add(7*time.Hour + 13*time.Minute) {
			_, a := ts.In(loc).Zone()
			_, b := ts.In(ref).Zone()
			if a != b {
				t.Fatalf("%s at %v: offset %d, IANA %d", tc.tzid, ts, a, b)
			}
		}
	}
	// Custom zone: +03 standard, +04 summer with EU switch dates.
	cal := loadCal(t, "customtz.ics")
	loc, ok := LoadLocation("My Zone", cal)
	if !ok {
		t.Fatal("My Zone not resolved")
	}
	for _, tc := range []struct {
		t    string
		off  int
		name string
	}{
		{"20250329T225959Z", 3 * 3600, "MYST"},
		{"20250329T230000Z", 4 * 3600, "MYDT"},
		{"20251025T225959Z", 4 * 3600, "MYDT"},
		{"20251025T230000Z", 3 * 3600, "MYST"},
		{"19600101T000000Z", 3 * 3600, "MYST"},
		{"21500701T000000Z", 4 * 3600, "MYDT"},
	} {
		name, off := utc(tc.t).In(loc).Zone()
		if off != tc.off || name != tc.name {
			t.Errorf("%s: %s %d, want %s %d", tc.t, name, off, tc.name, tc.off)
		}
	}
	// Same block again hits the cache and returns the same location.
	if loc2, _ := LoadLocation("My Zone", cal); loc2 != loc {
		t.Error("VTIMEZONE location not cached")
	}
}

func TestVTimezoneRDateAndUntil(t *testing.T) {
	cal := parseCal(t, `BEGIN:VCALENDAR
BEGIN:VTIMEZONE
TZID:Old Rules
BEGIN:DAYLIGHT
DTSTART:19800427T020000
RRULE:FREQ=YEARLY;BYMONTH=4;BYDAY=-1SU;UNTIL=19860427T070000Z
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
TZNAME:EDT
END:DAYLIGHT
BEGIN:DAYLIGHT
DTSTART:19870405T020000
RDATE:19870405T020000,19880403T020000
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
TZNAME:EDT
END:DAYLIGHT
BEGIN:STANDARD
DTSTART:19801026T020000
RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU;UNTIL=19881030T060000Z
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
TZNAME:EST
END:STANDARD
END:VTIMEZONE
END:VCALENDAR
`)
	loc, ok := LoadLocation("Old Rules", cal)
	if !ok {
		t.Fatal("not resolved")
	}
	ny, _ := time.LoadLocation("America/New_York")
	for ts := utc("19800101T000000Z"); ts.Year() < 1989; ts = ts.Add(time.Hour) {
		_, a := ts.In(loc).Zone()
		_, b := ts.In(ny).Zone()
		if a != b {
			t.Fatalf("%v: %d vs %d", ts, a, b)
		}
	}
	// After the last transition the last offset (EST) stays.
	if _, off := utc("19950701T000000Z").In(loc).Zone(); off != -5*3600 {
		t.Errorf("after last transition: %d", off)
	}
}

func TestParseDateTime(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	cases := []struct {
		line     string
		floating *time.Location
		want     time.Time
		allDay   bool
		float    bool
	}{
		{"DTSTART:20250101T100000Z", nil, utc("20250101T100000Z"), false, false},
		{"DTSTART:20250101T100000", nil, utc("20250101T100000Z"), false, true},
		{"DTSTART:20250101T100000", berlin, utc("20250101T090000Z"), false, true},
		{"DTSTART;TZID=Europe/Berlin:20250701T100000", nil, utc("20250701T080000Z"), false, false},
		{"DTSTART;VALUE=DATE:20250101", nil, utc("20250101T000000Z"), true, true},
		{"DTSTART:20250101", berlin, utc("20241231T230000Z"), true, true},
		{"DTSTART;TZID=Europe/Berlin;VALUE=DATE:20250101", nil, utc("20250101T000000Z"), true, true},
		{"DTSTART;TZID=Europe/Berlin:20250330T023000", nil, utc("20250330T013000Z"), false, false},
		{"DTSTART;TZID=Europe/Berlin:20251026T023000", nil, utc("20251026T003000Z"), false, false},
		{"DTSTART;TZID=Unknown/Zone:20250101T100000", nil, utc("20250101T100000Z"), false, false},
	}
	for _, c := range cases {
		cal := parseCal(t, "BEGIN:VCALENDAR\nBEGIN:VEVENT\n"+c.line+"\nEND:VEVENT\nEND:VCALENDAR\n")
		dt, err := ParseDateTime(cal.Children[0].Prop("DTSTART"), cal, c.floating)
		if err != nil {
			t.Errorf("%s: %v", c.line, err)
			continue
		}
		if !dt.Time.Equal(c.want) || dt.AllDay != c.allDay || dt.Floating != c.float {
			t.Errorf("%s: got %v allday=%v floating=%v", c.line, dt.Time, dt.AllDay, dt.Floating)
		}
	}
	for _, bad := range []string{"2025", "20251301", "20250230", "20250101T250000", "20250101X100000", "20250101T1000", "abcdefgh", "20250101T100000ZZ"} {
		if _, err := ParseDateTime(&ical.Prop{Name: "DTSTART", Value: bad}, nil, nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParseDateTime(nil, nil, nil); err == nil {
		t.Error("nil prop accepted")
	}
}

func TestParseDateTimeList(t *testing.T) {
	cal := parseCal(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
RDATE;VALUE=PERIOD:20250101T100000Z/20250101T120000Z,20250102T100000Z/PT90M
RDATE;TZID=America/New_York:20250103T090000,20250104T090000
EXDATE;VALUE=DATE:20250105,20250106
RDATE;VALUE=PERIOD:20250101T100000Z
END:VEVENT
END:VCALENDAR
`)
	ps := cal.Children[0].Props
	s, e, err := ParseDateTimeList(ps[0], cal, nil)
	if err != nil || len(s) != 2 || !s[0].Time.Equal(utc("20250101T100000Z")) || !e[0].Equal(utc("20250101T120000Z")) ||
		!s[1].Time.Equal(utc("20250102T100000Z")) || !e[1].Equal(utc("20250102T113000Z")) {
		t.Errorf("period: %v %v %v", s, e, err)
	}
	s, e, err = ParseDateTimeList(ps[1], cal, nil)
	if err != nil || len(s) != 2 || !s[1].Time.Equal(utc("20250104T140000Z")) || !e[1].IsZero() {
		t.Errorf("tzid list: %v %v %v", s, e, err)
	}
	s, _, err = ParseDateTimeList(ps[2], cal, nil)
	if err != nil || len(s) != 2 || !s[1].AllDay || !s[1].Time.Equal(utc("20250106T000000Z")) {
		t.Errorf("date list: %v %v", s, err)
	}
	if _, _, err := ParseDateTimeList(ps[3], cal, nil); err == nil {
		t.Error("period without end accepted")
	}
}

func TestDuration(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	cases := []struct {
		in    string
		want  Duration
		canon string
	}{
		{"P1W", Duration{Days: 7}, "P1W"},
		{"-PT15M", Duration{Neg: true, Clock: 15 * time.Minute}, "-PT15M"},
		{"P1DT2H", Duration{Days: 1, Clock: 2 * time.Hour}, "P1DT2H"},
		{"+PT1H30M5S", Duration{Clock: time.Hour + 30*time.Minute + 5*time.Second}, "PT1H30M5S"},
		{"PT0S", Duration{}, "PT0S"},
		{"P2W1D", Duration{Days: 15}, "P15D"},
		{"pt36h", Duration{Clock: 36 * time.Hour}, "PT36H"},
	}
	for _, c := range cases {
		d, err := ParseDuration(c.in)
		if err != nil || d != c.want {
			t.Errorf("ParseDuration(%q) = %+v, %v", c.in, d, err)
		}
		if d.String() != c.canon {
			t.Errorf("String(%q) = %q", c.in, d.String())
		}
	}
	for _, bad := range []string{"", "P", "PT", "1D", "P1H", "PT1D", "P1DT", "P-1D", "PT1.5H", "P99999999999D", "P1DT2H3", "PTT1H"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) accepted", bad)
		}
	}
	// Nominal days keep the wall clock across DST; hours are exact.
	start := time.Date(2025, 3, 29, 10, 0, 0, 0, berlin)
	if got := (Duration{Days: 1}).AddTo(start); !got.Equal(time.Date(2025, 3, 30, 10, 0, 0, 0, berlin)) {
		t.Errorf("P1D: %v", got)
	}
	if got := (Duration{Clock: 24 * time.Hour}).AddTo(start); !got.Equal(time.Date(2025, 3, 30, 11, 0, 0, 0, berlin)) {
		t.Errorf("PT24H: %v", got)
	}
	if got := (Duration{Neg: true, Days: 1, Clock: time.Hour}).AddTo(start); !got.Equal(time.Date(2025, 3, 28, 9, 0, 0, 0, berlin)) {
		t.Errorf("-P1DT1H: %v", got)
	}
}
