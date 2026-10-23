package rrule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davd/internal/ical"
)

var fuzzRuleSeeds = []string{
	"FREQ=DAILY",
	"FREQ=MONTHLY;BYDAY=-1FR",
	"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29",
	"FREQ=YEARLY;BYWEEKNO=-1,53;BYDAY=MO;WKST=SU",
	"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1,2",
	"FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,SU;WKST=SU;COUNT=10",
	"FREQ=HOURLY;INTERVAL=5;BYHOUR=1,3;BYMINUTE=0,30",
	"FREQ=SECONDLY;BYSECOND=0,30;BYMINUTE=0;BYHOUR=12",
	"FREQ=YEARLY;BYYEARDAY=-366,1,366;UNTIL=20300101",
	"FREQ=DAILY;UNTIL=20251231T235959Z",
	"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=31",
	"FREQ=MINUTELY;INTERVAL=1440;BYHOUR=3",
}

func FuzzParse(f *testing.F) {
	for _, s := range fuzzRuleSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := Parse(s)
		if err != nil {
			return
		}
		canon := r.String()
		r2, err := Parse(canon)
		if err != nil {
			t.Fatalf("canonical form %q of %q does not parse: %v", canon, s, err)
		}
		if c2 := r2.String(); c2 != canon {
			t.Fatalf("not stable: %q -> %q -> %q", s, canon, c2)
		}
	})
}

var fuzzLocs = func() []*time.Location {
	out := []*time.Location{time.UTC}
	for _, n := range []string{"Europe/Berlin", "America/New_York", "Australia/Lord_Howe", "Pacific/Apia"} {
		l, err := time.LoadLocation(n)
		if err != nil {
			panic(err)
		}
		out = append(out, l)
	}
	return out
}()

func FuzzIter(f *testing.F) {
	for i, s := range fuzzRuleSeeds {
		f.Add(s, int64(1735725600+i*86400*37), uint8(i))
	}
	f.Fuzz(func(t *testing.T, s string, start int64, locIdx uint8) {
		r, err := Parse(s)
		if err != nil {
			return
		}
		// Keep DTSTART within years 1..9999.
		const lo, hi = -62135596800, 253402300799
		if start < lo || start > hi {
			start = lo + ((start%(hi-lo))+(hi-lo))%(hi-lo)
		}
		loc := fuzzLocs[int(locIdx)%len(fuzzLocs)]
		dtstart := time.Unix(start, 0).In(loc)
		var prev time.Time
		n := 0
		deadline := time.Now().Add(10 * time.Second)
		for v := range r.Iter(dtstart) {
			if n == 0 && !v.Equal(dtstart) {
				t.Fatalf("first %v != dtstart %v", v, dtstart)
			}
			if n > 0 && !v.After(prev) {
				t.Fatalf("not ascending: %v then %v", prev, v)
			}
			if v.Location() != loc {
				t.Fatalf("location %v", v.Location())
			}
			if r.Count > 0 && n >= r.Count {
				t.Fatalf("more than COUNT=%d", r.Count)
			}
			prev = v
			n++
			if n >= 300 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("too slow")
			}
		}
	})
}

const fuzzICS = `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:x
DTSTART;TZID=Europe/Berlin:20250105T100000
DTEND;TZID=Europe/Berlin:20250105T110000
RRULE:FREQ=WEEKLY;BYDAY=MO,WE
EXDATE;TZID=Europe/Berlin:20250108T100000
RDATE;VALUE=PERIOD:20250301T100000Z/PT2H
END:VEVENT
BEGIN:VEVENT
UID:x
RECURRENCE-ID;TZID=Europe/Berlin;RANGE=THISANDFUTURE:20250113T100000
DTSTART;TZID=Europe/Berlin:20250112T090000
DURATION:PT30M
END:VEVENT
END:VCALENDAR
`

func FuzzExpand(f *testing.F) {
	f.Add([]byte(fuzzICS))
	if ents, err := os.ReadDir("testdata"); err == nil {
		for _, e := range ents {
			if filepath.Ext(e.Name()) == ".ics" {
				if b, err := os.ReadFile(filepath.Join("testdata", e.Name())); err == nil {
					f.Add(b)
				}
			}
		}
	}
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, data []byte) {
		cal, err := ical.ParseBytes(data)
		if err != nil {
			return
		}
		deadline := time.Now().Add(20 * time.Second)
		for _, name := range []string{"VEVENT", "VTODO", "VJOURNAL"} {
			var prev time.Time
			n := 0
			for o := range Expand(cal, name, "", from, to, fuzzLocs[1]) {
				if n > 0 && o.Start.Before(prev) {
					t.Fatalf("not sorted: %v after %v", o.Start, prev)
				}
				if o.End.Before(o.Start) {
					t.Fatalf("end %v before start %v", o.End, o.Start)
				}
				if !(o.Start.Before(to) && (o.End.After(from) || (o.End.Equal(o.Start) && !o.Start.Before(from)))) {
					t.Fatalf("outside range: %v-%v", o.Start, o.End)
				}
				if o.Comp == nil {
					t.Fatal("nil component")
				}
				prev = o.Start
				n++
				if n >= 1000 {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("too slow")
			}
		}
	})
}

// FuzzExpandFields fuzzes the property values of a template series, which
// reaches the expansion logic far more often than mutating raw bytes.
func FuzzExpandFields(f *testing.F) {
	f.Add("FREQ=WEEKLY;BYDAY=MO,WE", "20250105T100000", "Europe/Berlin", "20250108T100000", "20250113T100000", "20250112T090000", "PT1H", true)
	f.Add("FREQ=DAILY;COUNT=10", "20250105", "", "20250107", "20250108", "20250109", "P2D", false)
	f.Add("FREQ=MONTHLY;BYDAY=-1FR", "20241231T235959Z", "W. Europe Standard Time", "20250131T235959Z", "20250228T235959Z", "20250101T000000Z", "-PT1H", true)
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, rule, dtstart, tzid, exdate, rid, ovStart, dur string, future bool) {
		for _, s := range []string{rule, dtstart, tzid, exdate, rid, ovStart, dur} {
			if strings.ContainsAny(s, "\r\n\":;") && s != rule || strings.ContainsAny(s, "\r\n") {
				return
			}
		}
		tz := ""
		if tzid != "" {
			tz = ";TZID=" + tzid
		}
		rng := ""
		if future {
			rng = ";RANGE=THISANDFUTURE"
		}
		ics := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTART" + tz + ":" + dtstart + "\r\nDURATION:" + dur +
			"\r\nRRULE:" + rule + "\r\nEXDATE" + tz + ":" + exdate + "\r\nRDATE" + tz + ":" + exdate + "," + ovStart +
			"\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:u\r\nRECURRENCE-ID" + rng + tz + ":" + rid + "\r\nDTSTART" + tz + ":" + ovStart +
			"\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		cal, err := ical.ParseBytes([]byte(ics))
		if err != nil {
			return
		}
		var prev time.Time
		n := 0
		for o := range Expand(cal, "VEVENT", "u", from, to, fuzzLocs[2]) {
			if n > 0 && o.Start.Before(prev) {
				t.Fatalf("not sorted: %v after %v", o.Start, prev)
			}
			if o.End.Before(o.Start) {
				t.Fatalf("end %v before start %v", o.End, o.Start)
			}
			if !(o.Start.Before(to) && (o.End.After(from) || (o.End.Equal(o.Start) && !o.Start.Before(from)))) {
				t.Fatalf("outside range: %v-%v", o.Start, o.End)
			}
			prev = o.Start
			if n++; n >= 2000 {
				break
			}
		}
	})
}
