package rrule

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// vector is one expansion test case. Times are wall-clock "20060102T150405"
// values in UTC unless the test says otherwise.
type vector struct {
	name    string
	dtstart string
	rule    string
	limit   int // maximum number of values pulled
	want    []string
}

const layout = "20060102T150405"

func mustTime(t testing.TB, s string, loc *time.Location) time.Time {
	t.Helper()
	v, err := time.ParseInLocation(layout, s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func take(seq func(func(time.Time) bool), n int) []string {
	var out []string
	for v := range seq {
		if len(out) >= n {
			break
		}
		out = append(out, v.Format(layout))
	}
	return out
}

// dateutilDeviations lists vectors where this package intentionally differs
// from python-dateutil:
//   - BYWEEKNO: dateutil does not mark days of the calendar year that belong
//     to the last week of the previous week-year unless it is week -1/the
//     exact count, nor days belonging to a negative week of the next
//     week-year, and in some years it attributes Jan 1/2 to week 53 of a
//     52-week year (it uses the current year's length when computing the
//     previous year's week count). Here every day is matched by its real
//     week number (and week count) honoring WKST; see TestWeekNo.
//   - UNTIL as a DATE: dateutil compares against midnight, this package
//     treats the whole day as included (safer for time-range queries).
var dateutilDeviations = map[string][]string{
	"WeekNo53WkstMO":     {"19921228T000000", "19921229T000000", "19921230T000000", "19921231T000000", "19930101T000000", "19930102T000000", "19930103T000000", "19981228T000000", "19981229T000000", "19981230T000000"},
	"WeekNo53WkstMODay":  {"19921228T000000", "19930103T000000", "19981228T000000", "19990103T000000", "20041227T000000", "20050102T000000"},
	"WeekNoN53WkstMO":    {"19911230T000000", "19911231T000000", "19920101T000000", "19920102T000000", "19920103T000000", "19920104T000000", "19920105T000000", "19971229T000000", "19971230T000000", "19971231T000000"},
	"WeekNoN53WkstMODay": {"19911230T000000", "19920105T000000", "19971229T000000", "19980104T000000", "20031229T000000", "20040104T000000"},
	"WeekNoN53WkstSU":    {"19911229T000000", "19911230T000000", "19911231T000000", "19920101T000000", "19920102T000000", "19920103T000000", "19920104T000000", "19961229T000000", "19961230T000000", "19961231T000000"},
	"WeekNoN53WkstSUDay": {"19911229T000000", "19911230T000000", "19961229T000000", "19961230T000000", "20021229T000000", "20021230T000000"},
	"WeekNo52WkstTH":     {"19901227T000000", "19901228T000000", "19901229T000000", "19901230T000000", "19901231T000000", "19910101T000000", "19910102T000000", "19911226T000000", "19911227T000000", "19911228T000000"},
	"WeekNo53WkstTH":     {"19900101T000000", "19900102T000000", "19900103T000000", "19951228T000000", "19951229T000000", "19951230T000000", "19951231T000000", "19960101T000000", "19960102T000000", "19960103T000000"},
	"WeekNoN53WkstTH":    {"19941229T000000", "19941230T000000", "19941231T000000", "19950101T000000", "19950102T000000", "19950103T000000", "19950104T000000", "19991230T000000", "19991231T000000", "20000101T000000"},
	"WeekNoN53WkstSA":    {"19901229T000000", "19901230T000000", "19901231T000000", "19910101T000000", "19910102T000000", "19910103T000000", "19910104T000000", "19951230T000000", "19951231T000000", "19960101T000000"},
	"WeekNoN53WkstSADay": {"19901230T000000", "19901231T000000", "19951231T000000", "19960101T000000", "20011230T000000", "20011231T000000"},
	"UntilWithDate":      {"19970902T090000", "19970903T090000", "19970904T090000", "19970905T090000"},
	"DailyByWeekNo53":    {"20201228T000000", "20201229T000000", "20201230T000000", "20201231T000000", "20210101T000000", "20210102T000000", "20210103T000000", "20261228T000000", "20261229T000000", "20261230T000000", "20261231T000000", "20270101T000000", "20270102T000000", "20270103T000000"},
}

// TestDateutilVectors checks the core algorithm against python-dateutil
// semantics: DTSTART is only included if it matches the rule.
func TestDateutilVectors(t *testing.T) {
	for _, v := range dateutilVectors {
		t.Run(v.name, func(t *testing.T) {
			r, err := Parse(v.rule)
			if err != nil {
				t.Fatal(err)
			}
			want := v.want
			if d, ok := dateutilDeviations[v.name]; ok {
				want = d
			}
			got := take(r.expand(mustTime(t, v.dtstart, time.UTC), r.Count), v.limit)
			if !slices.Equal(got, want) {
				t.Errorf("%s from %s\n got  %v\n want %v", v.rule, v.dtstart, got, want)
			}
		})
	}
}

// TestIterRFC checks RFC 5545 semantics: DTSTART first, counted by COUNT.
func TestIterRFC(t *testing.T) {
	tests := []vector{
		// DTSTART matches: same as dateutil.
		{"matching", "19970902T090000", "FREQ=DAILY;COUNT=3", 10,
			[]string{"19970902T090000", "19970903T090000", "19970904T090000"}},
		// DTSTART (a Tuesday) does not match BYDAY=MO: it is still the
		// first instance and consumes one COUNT.
		{"non-matching dtstart", "19970902T090000", "FREQ=WEEKLY;BYDAY=MO;COUNT=3", 10,
			[]string{"19970902T090000", "19970908T090000", "19970915T090000"}},
		{"non-matching dtstart until", "19970902T090000", "FREQ=WEEKLY;BYDAY=MO;UNTIL=19970915T090000Z", 10,
			[]string{"19970902T090000", "19970908T090000", "19970915T090000"}},
		{"count 1", "19970902T090000", "FREQ=DAILY;COUNT=1", 10, []string{"19970902T090000"}},
		// UNTIL before DTSTART: DTSTART is still an instance.
		{"until before start", "19970902T090000", "FREQ=DAILY;UNTIL=19970101T000000Z", 10, []string{"19970902T090000"}},
		// UNTIL as DATE is inclusive of the whole day.
		{"until date", "19970902T090000", "FREQ=DAILY;UNTIL=19970904", 10,
			[]string{"19970902T090000", "19970903T090000", "19970904T090000"}},
		// Floating UNTIL.
		{"until floating", "19970902T090000", "FREQ=DAILY;UNTIL=19970904T085959", 10,
			[]string{"19970902T090000", "19970903T090000"}},
		// Monthly on the 31st skips short months (no clamping).
		{"monthly 31st", "20250131T100000", "FREQ=MONTHLY;COUNT=5", 10,
			[]string{"20250131T100000", "20250331T100000", "20250531T100000", "20250731T100000", "20250831T100000"}},
		// Yearly on Feb 29: leap years only.
		{"yearly feb 29", "20240229T000000", "FREQ=YEARLY;COUNT=4", 10,
			[]string{"20240229T000000", "20280229T000000", "20320229T000000", "20360229T000000"}},
		{"yearly feb 29 1896", "18960229T000000", "FREQ=YEARLY;COUNT=3", 10,
			[]string{"18960229T000000", "19040229T000000", "19080229T000000"}},
		// Last Friday of the month.
		{"last friday", "20251031T170000", "FREQ=MONTHLY;BYDAY=-1FR;COUNT=4", 10,
			[]string{"20251031T170000", "20251128T170000", "20251226T170000", "20260130T170000"}},
		// Every weekday starting on a Saturday: Saturday is DTSTART only.
		{"weekdays from saturday", "20251025T080000", "FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR;COUNT=4", 10,
			[]string{"20251025T080000", "20251027T080000", "20251028T080000", "20251029T080000"}},
		// RFC 5545 examples (section 3.8.5.3), DTSTART in UTC here.
		{"rfc every other week mo we fr", "19970901T090000", "FREQ=WEEKLY;INTERVAL=2;UNTIL=19971224T000000Z;WKST=SU;BYDAY=MO,WE,FR", 100,
			[]string{"19970901T090000", "19970903T090000", "19970905T090000", "19970915T090000", "19970917T090000", "19970919T090000",
				"19970929T090000", "19971001T090000", "19971003T090000", "19971013T090000", "19971015T090000", "19971017T090000",
				"19971027T090000", "19971029T090000", "19971031T090000", "19971110T090000", "19971112T090000", "19971114T090000",
				"19971124T090000", "19971126T090000", "19971128T090000", "19971208T090000", "19971210T090000", "19971212T090000",
				"19971222T090000"}},
		{"rfc first friday x10", "19970905T090000", "FREQ=MONTHLY;COUNT=10;BYDAY=1FR", 100,
			[]string{"19970905T090000", "19971003T090000", "19971107T090000", "19971205T090000", "19980102T090000",
				"19980206T090000", "19980306T090000", "19980403T090000", "19980501T090000", "19980605T090000"}},
		{"rfc every other month first and last sunday", "19970907T090000", "FREQ=MONTHLY;INTERVAL=2;COUNT=10;BYDAY=1SU,-1SU", 100,
			[]string{"19970907T090000", "19970928T090000", "19971102T090000", "19971130T090000", "19980104T090000",
				"19980125T090000", "19980301T090000", "19980329T090000", "19980503T090000", "19980531T090000"}},
		{"rfc second to last monday x6", "19970922T090000", "FREQ=MONTHLY;COUNT=6;BYDAY=-2MO", 100,
			[]string{"19970922T090000", "19971020T090000", "19971117T090000", "19971222T090000", "19980119T090000", "19980216T090000"}},
		{"rfc third to last day", "19970928T090000", "FREQ=MONTHLY;BYMONTHDAY=-3", 6,
			[]string{"19970928T090000", "19971029T090000", "19971128T090000", "19971229T090000", "19980129T090000", "19980226T090000"}},
		{"rfc 2nd and 15th x10", "19970902T090000", "FREQ=MONTHLY;COUNT=10;BYMONTHDAY=2,15", 100,
			[]string{"19970902T090000", "19970915T090000", "19971002T090000", "19971015T090000", "19971102T090000",
				"19971115T090000", "19971202T090000", "19971215T090000", "19980102T090000", "19980115T090000"}},
		{"rfc yearly june july x10", "19970610T090000", "FREQ=YEARLY;COUNT=10;BYMONTH=6,7", 100,
			[]string{"19970610T090000", "19970710T090000", "19980610T090000", "19980710T090000", "19990610T090000",
				"19990710T090000", "20000610T090000", "20000710T090000", "20010610T090000", "20010710T090000"}},
		{"rfc every 3rd year day 1 100 200", "19970101T090000", "FREQ=YEARLY;INTERVAL=3;COUNT=10;BYYEARDAY=1,100,200", 100,
			[]string{"19970101T090000", "19970410T090000", "19970719T090000", "20000101T090000", "20000409T090000",
				"20000718T090000", "20030101T090000", "20030410T090000", "20030719T090000", "20060101T090000"}},
		{"rfc monday of week 20", "19970512T090000", "FREQ=YEARLY;BYWEEKNO=20;BYDAY=MO", 3,
			[]string{"19970512T090000", "19980511T090000", "19990517T090000"}},
		{"rfc friday 13th with non-matching dtstart", "19970902T090000", "FREQ=MONTHLY;BYDAY=FR;BYMONTHDAY=13", 5,
			[]string{"19970902T090000", "19980213T090000", "19980313T090000", "19981113T090000", "19990813T090000"}},
		{"rfc us election day", "19961105T090000", "FREQ=YEARLY;INTERVAL=4;BYMONTH=11;BYDAY=TU;BYMONTHDAY=2,3,4,5,6,7,8", 3,
			[]string{"19961105T090000", "20001107T090000", "20041102T090000"}},
		{"rfc 3rd tu we th x3", "19970904T090000", "FREQ=MONTHLY;COUNT=3;BYDAY=TU,WE,TH;BYSETPOS=3", 10,
			[]string{"19970904T090000", "19971007T090000", "19971106T090000"}},
		{"rfc 2nd to last weekday", "19970929T090000", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-2", 7,
			[]string{"19970929T090000", "19971030T090000", "19971127T090000", "19971230T090000", "19980129T090000",
				"19980226T090000", "19980330T090000"}},
		{"rfc every 3 hours until", "19970902T090000", "FREQ=HOURLY;INTERVAL=3;UNTIL=19970902T170000Z", 10,
			[]string{"19970902T090000", "19970902T120000", "19970902T150000"}},
		{"rfc every 20 min 9-16:40", "19970902T090000", "FREQ=DAILY;BYHOUR=9,10,11,12,13,14,15,16;BYMINUTE=0,20,40", 26,
			[]string{"19970902T090000", "19970902T092000", "19970902T094000", "19970902T100000", "19970902T102000",
				"19970902T104000", "19970902T110000", "19970902T112000", "19970902T114000", "19970902T120000",
				"19970902T122000", "19970902T124000", "19970902T130000", "19970902T132000", "19970902T134000",
				"19970902T140000", "19970902T142000", "19970902T144000", "19970902T150000", "19970902T152000",
				"19970902T154000", "19970902T160000", "19970902T162000", "19970902T164000", "19970903T090000",
				"19970903T092000"}},
		{"rfc wkst mo", "19970805T090000", "FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=MO", 10,
			[]string{"19970805T090000", "19970810T090000", "19970819T090000", "19970824T090000"}},
		{"rfc wkst su", "19970805T090000", "FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=SU", 10,
			[]string{"19970805T090000", "19970817T090000", "19970819T090000", "19970831T090000"}},
		{"rfc invalid 15th and 30th skip feb", "20070115T090000", "FREQ=MONTHLY;BYMONTHDAY=15,30;COUNT=5", 10,
			[]string{"20070115T090000", "20070130T090000", "20070215T090000", "20070315T090000", "20070330T090000"}},
	}
	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			r, err := Parse(v.rule)
			if err != nil {
				t.Fatal(err)
			}
			got := take(r.Iter(mustTime(t, v.dtstart, time.UTC)), v.limit)
			if !slices.Equal(got, v.want) {
				t.Errorf("%s from %s\n got  %v\n want %v", v.rule, v.dtstart, got, v.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	good := []struct{ in, canon string }{
		{"FREQ=DAILY", "FREQ=DAILY"},
		{"RRULE:freq=weekly;byday=mo,+2tu,-1fr;wkst=su", "FREQ=WEEKLY;BYDAY=MO,2TU,-1FR;WKST=SU"},
		{"FREQ=MONTHLY;BYDAY=-1FR;", "FREQ=MONTHLY;BYDAY=-1FR"},
		{"FREQ=YEARLY;UNTIL=20251231;BYMONTH=2;BYMONTHDAY=29", "FREQ=YEARLY;UNTIL=20251231;BYMONTHDAY=29;BYMONTH=2"},
		{"FREQ=DAILY;UNTIL=20251231T235959Z;INTERVAL=2", "FREQ=DAILY;INTERVAL=2;UNTIL=20251231T235959Z"},
		{"FREQ=DAILY;UNTIL=20251231T235959", "FREQ=DAILY;UNTIL=20251231T235959"},
		{"FREQ=DAILY;COUNT=5;UNTIL=20251231T235959Z", "FREQ=DAILY;COUNT=5;UNTIL=20251231T235959Z"},
		{"FREQ=YEARLY;BYWEEKNO=-1,53;BYYEARDAY=-366,1;BYSETPOS=-1", "FREQ=YEARLY;BYYEARDAY=-366,1;BYWEEKNO=-1,53;BYSETPOS=-1"},
		{"FREQ=SECONDLY;BYSECOND=0,60;BYMINUTE=59;BYHOUR=23", "FREQ=SECONDLY;BYSECOND=0,60;BYMINUTE=59;BYHOUR=23"},
		{"FREQ=DAILY;X-NAME=foo", "FREQ=DAILY"},
		{"FREQ=DAILY;INTERVAL=1", "FREQ=DAILY"},
	}
	for _, g := range good {
		r, err := Parse(g.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", g.in, err)
			continue
		}
		if s := r.String(); s != g.canon {
			t.Errorf("Parse(%q).String() = %q, want %q", g.in, s, g.canon)
		}
	}
	bad := []string{
		"", "BYDAY=MO", "FREQ=FORTNIGHTLY", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;COUNT=-1",
		"FREQ=DAILY;INTERVAL=0", "FREQ=DAILY;BYMONTH=13", "FREQ=DAILY;BYMONTH=0",
		"FREQ=DAILY;BYMONTHDAY=0", "FREQ=DAILY;BYMONTHDAY=32", "FREQ=DAILY;BYMONTHDAY=-32",
		"FREQ=DAILY;BYYEARDAY=367", "FREQ=DAILY;BYWEEKNO=54", "FREQ=DAILY;BYHOUR=24",
		"FREQ=DAILY;BYMINUTE=60", "FREQ=DAILY;BYSECOND=61", "FREQ=DAILY;BYDAY=XX",
		"FREQ=DAILY;BYDAY=0MO", "FREQ=DAILY;BYDAY=54MO", "FREQ=DAILY;WKST=XY",
		"FREQ=DAILY;UNTIL=2025", "FREQ=DAILY;UNTIL=20250101Z", "FREQ=DAILY;FREQ=WEEKLY",
		"FREQ=DAILY;BYHOUR=-1", "FREQ=DAILY;BYMONTH=1,,2", "FREQ=DAILY;garbage",
		"FREQ=DAILY;BYSETPOS=0", "FREQ=DAILY;COUNT=1e3",
	}
	for _, b := range bad {
		if _, err := Parse(b); err == nil {
			t.Errorf("Parse(%q) succeeded", b)
		}
	}
}

// TestImpossibleRulesTerminate checks that rules that can never match end
// quickly instead of looping forever.
func TestImpossibleRulesTerminate(t *testing.T) {
	rules := []string{
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=31",
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=MONTHLY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=WEEKLY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=DAILY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=HOURLY;BYMONTH=4;BYMONTHDAY=31",
		"FREQ=MINUTELY;BYMONTH=4;BYMONTHDAY=31",
		"FREQ=SECONDLY;BYMONTH=4;BYMONTHDAY=31",
		"FREQ=YEARLY;BYMONTH=6;BYDAY=5MO;BYMONTHDAY=1",
		"FREQ=MONTHLY;BYDAY=MO;BYSETPOS=6",
		"FREQ=HOURLY;INTERVAL=24;BYHOUR=5",
		"FREQ=MINUTELY;INTERVAL=60;BYMINUTE=5",
		"FREQ=MINUTELY;INTERVAL=1440;BYHOUR=3",
		"FREQ=SECONDLY;INTERVAL=86400;BYHOUR=3",
		"FREQ=SECONDLY;INTERVAL=60;BYSECOND=5",
		"FREQ=SECONDLY;BYSECOND=60",
		"FREQ=YEARLY;BYWEEKNO=53;BYMONTH=6",
		"FREQ=YEARLY;BYYEARDAY=366;BYMONTH=1",
		"FREQ=DAILY;BYDAY=MO;BYMONTHDAY=1;BYMONTH=1;BYYEARDAY=2",
		"FREQ=HOURLY;BYSETPOS=2",
	}
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range rules {
		r, err := Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		n := 0
		for range r.Iter(start) {
			n++
			if n > 10 {
				t.Errorf("%s: unexpected occurrences", s)
				break
			}
		}
		if n != 1 {
			t.Errorf("%s: got %d occurrences, want only DTSTART", s, n)
		}
		if d := time.Since(t0); d > 3*time.Second {
			t.Errorf("%s: took %v", s, d)
		}
	}
}

// TestSparseRules makes sure legitimately rare rules are not cut off by the
// termination heuristics.
func TestSparseRules(t *testing.T) {
	tests := []vector{
		{"feb 29 on monday", "20000101T000000", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;BYDAY=MO", 4,
			[]string{"20000101T000000", "20160229T000000", "20440229T000000", "20720229T000000"}},
		{"feb 29 on monday daily", "20000101T000000", "FREQ=DAILY;BYMONTH=2;BYMONTHDAY=29;BYDAY=MO", 3,
			[]string{"20000101T000000", "20160229T000000", "20440229T000000"}},
		{"feb 29 across 2100", "20960229T000000", "FREQ=YEARLY", 3,
			[]string{"20960229T000000", "21040229T000000", "21080229T000000"}},
		{"every 300 years", "20000101T000000", "FREQ=YEARLY;INTERVAL=300", 4,
			[]string{"20000101T000000", "23000101T000000", "26000101T000000", "29000101T000000"}},
		{"year 9999 end", "99980101T000000", "FREQ=YEARLY", 5,
			[]string{"99980101T000000", "99990101T000000"}},
	}
	for _, v := range tests {
		r, err := Parse(v.rule)
		if err != nil {
			t.Fatal(err)
		}
		got := take(r.Iter(mustTime(t, v.dtstart, time.UTC)), v.limit)
		if !slices.Equal(got, v.want) {
			t.Errorf("%s: got %v want %v", v.name, got, v.want)
		}
	}
}

func TestDST(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	ny, _ := time.LoadLocation("America/New_York")
	full := "2006-01-02 15:04 -0700"
	tests := []struct {
		name  string
		loc   *time.Location
		start time.Time
		rule  string
		limit int
		want  []string
	}{
		{"berlin weekly across spring", berlin, time.Date(2025, 3, 20, 9, 0, 0, 0, berlin), "FREQ=WEEKLY", 3,
			[]string{"2025-03-20 09:00 +0100", "2025-03-27 09:00 +0100", "2025-04-03 09:00 +0200"}},
		{"berlin weekly across autumn", berlin, time.Date(2025, 10, 16, 9, 0, 0, 0, berlin), "FREQ=WEEKLY", 3,
			[]string{"2025-10-16 09:00 +0200", "2025-10-23 09:00 +0200", "2025-10-30 09:00 +0100"}},
		{"berlin daily both transitions", berlin, time.Date(2025, 3, 29, 23, 30, 0, 0, berlin), "FREQ=DAILY;INTERVAL=7;COUNT=31", 31, nil},
		{"ny daily across spring", ny, time.Date(2025, 3, 8, 8, 0, 0, 0, ny), "FREQ=DAILY", 3,
			[]string{"2025-03-08 08:00 -0500", "2025-03-09 08:00 -0400", "2025-03-10 08:00 -0400"}},
		{"ny daily across autumn", ny, time.Date(2025, 11, 1, 8, 0, 0, 0, ny), "FREQ=DAILY", 3,
			[]string{"2025-11-01 08:00 -0400", "2025-11-02 08:00 -0500", "2025-11-03 08:00 -0500"}},
		// 02:30 does not exist on the spring-forward day: RFC 5545 says to
		// use the offset before the gap, i.e. 03:30 local.
		{"berlin gap", berlin, time.Date(2025, 3, 29, 2, 30, 0, 0, berlin), "FREQ=DAILY", 3,
			[]string{"2025-03-29 02:30 +0100", "2025-03-30 03:30 +0200", "2025-03-31 02:30 +0200"}},
		{"ny gap", ny, time.Date(2025, 3, 8, 2, 30, 0, 0, ny), "FREQ=DAILY", 3,
			[]string{"2025-03-08 02:30 -0500", "2025-03-09 03:30 -0400", "2025-03-10 02:30 -0400"}},
		// 02:30 occurs twice on the fall-back day: the first one is used.
		{"berlin overlap", berlin, time.Date(2025, 10, 25, 2, 30, 0, 0, berlin), "FREQ=DAILY", 3,
			[]string{"2025-10-25 02:30 +0200", "2025-10-26 02:30 +0200", "2025-10-27 02:30 +0100"}},
		{"ny overlap", ny, time.Date(2025, 11, 1, 1, 30, 0, 0, ny), "FREQ=DAILY", 3,
			[]string{"2025-11-01 01:30 -0400", "2025-11-02 01:30 -0400", "2025-11-03 01:30 -0500"}},
		// Hourly through the gap: 02:00 maps to 03:00 and is deduplicated.
		{"berlin hourly gap", berlin, time.Date(2025, 3, 30, 0, 0, 0, 0, berlin), "FREQ=HOURLY", 4,
			[]string{"2025-03-30 00:00 +0100", "2025-03-30 01:00 +0100", "2025-03-30 03:00 +0200", "2025-03-30 04:00 +0200"}},
		{"monthly last sunday 2:30", berlin, time.Date(2025, 1, 26, 2, 30, 0, 0, berlin), "FREQ=MONTHLY;BYDAY=-1SU;COUNT=4", 4,
			[]string{"2025-01-26 02:30 +0100", "2025-02-23 02:30 +0100", "2025-03-30 03:30 +0200", "2025-04-27 02:30 +0200"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(tc.rule)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			var prev time.Time
			for v := range r.Iter(tc.start) {
				if len(got) >= tc.limit {
					break
				}
				if v.Location() != tc.loc {
					t.Fatalf("location %v", v.Location())
				}
				if !prev.IsZero() && !v.After(prev) {
					t.Fatalf("not ascending: %v after %v", v, prev)
				}
				prev = v
				got = append(got, v.Format(full))
			}
			if tc.want == nil {
				// Wall clock must be preserved.
				for _, g := range got {
					if !strings.Contains(g, " 23:30 ") {
						t.Errorf("wall clock drifted: %s", g)
					}
				}
				if len(got) != tc.limit {
					t.Errorf("got %d", len(got))
				}
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestWeekNo(t *testing.T) {
	// ISO 8601 (WKST=MO) against the standard library.
	for d := daysFromCivil(1990, 1, 1); d < daysFromCivil(2040, 1, 1); d++ {
		y, m, dd := civilFromDays(d)
		_, isoW := time.Date(y, time.Month(m), dd, 0, 0, 0, 0, time.UTC).ISOWeek()
		if w, _ := weekNo(d, 0); w != isoW {
			t.Fatalf("%d-%02d-%02d: week %d, ISO %d", y, m, dd, w, isoW)
		}
	}
}

func TestWeekNoWkst(t *testing.T) {
	// Independent definition: a week belongs to the year that contains its
	// fourth day; the week number counts weeks from that year's first such
	// week.
	for wkst := range 7 {
		for d := daysFromCivil(1990, 1, 1); d < daysFromCivil(2040, 1, 1); d++ {
			ws := d - pyMod(weekdayOf(d)-wkst, 7)
			y, _, _ := civilFromDays(ws + 3)
			first := daysFromCivil(y, 1, 1)
			want := (ws+3-first)/7 + 1
			got, num := weekNo(d, wkst)
			if got != want {
				t.Fatalf("wkst %d day %d: week %d want %d", wkst, d, got, want)
			}
			ny, _, _ := civilFromDays(daysFromCivil(y, 12, 31) - pyMod(weekdayOf(daysFromCivil(y, 12, 31))-wkst, 7) + 3)
			lastWeeks := num
			if ny == y {
				w, _ := weekNo(daysFromCivil(y, 12, 31), wkst)
				if w != lastWeeks {
					t.Fatalf("wkst %d year %d: %d weeks, last day in week %d", wkst, y, num, w)
				}
			}
		}
	}
}

func TestCivil(t *testing.T) {
	for d := -800000; d < 3000000; d += 997 {
		y, m, dd := civilFromDays(d)
		if daysFromCivil(y, m, dd) != d {
			t.Fatalf("roundtrip %d", d)
		}
		tm := time.Unix(int64(d)*86400, 0).UTC()
		if tm.Year() != y || int(tm.Month()) != m || tm.Day() != dd {
			t.Fatalf("%d: %d-%d-%d vs %v", d, y, m, dd, tm)
		}
		if weekdayOf(d) != monWeekday(tm.Weekday()) {
			t.Fatalf("weekday %d", d)
		}
	}
}
