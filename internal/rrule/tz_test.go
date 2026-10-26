package rrule

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// A long slash-separated TZID was looked up suffix by suffix, joining the
// rest each time: quadratic, minutes of CPU for one DTSTART.
func TestLoadLocationLongTZID(t *testing.T) {
	start := time.Now()
	tzid := strings.Repeat("a/", 50_000) + "Europe/Berlin"
	if _, ok := LoadLocation(tzid, nil); ok {
		t.Error("a 100 KB TZID resolved")
	}
	if d := time.Since(start); d > timeScale*time.Second {
		t.Fatalf("took %v", d)
	}
	if loc, ok := LoadLocation("/mozilla.org/20050126_1/Europe/Berlin", nil); !ok || loc.String() != "Europe/Berlin" {
		t.Fatalf("vendor prefix: %v %v", loc, ok)
	}
}

// Every distinct TZID that resolves was cached forever.
func TestLocationCacheBounded(t *testing.T) {
	for i := range maxCachedZones + 100 {
		if _, ok := LoadLocation("x"+strconv.Itoa(i)+"/Europe/Berlin", nil); !ok {
			t.Fatal("prefixed name did not resolve")
		}
	}
	n := 0
	namedCache.Range(func(_, _ any) bool { n++; return true })
	if n > maxCachedZones {
		t.Fatalf("%d cached zones", n)
	}
}

// Clock parts beyond time.Duration's range wrapped around: a positive
// duration of 297 years came out negative.
func TestParseDurationClockOverflow(t *testing.T) {
	for _, s := range []string{"PT2600000H", "PT9999999999S", "-PT2600000H"} {
		d, err := ParseDuration(s)
		if err == nil && d.Clock < 0 {
			t.Errorf("%s: clock %v", s, d.Clock)
		}
	}
	if d, err := ParseDuration("PT2000000H"); err != nil || d.Clock != 2_000_000*time.Hour {
		t.Errorf("PT2000000H: %+v %v", d, err)
	}
}

// A rule that never matches searched day by day for 400 years, each day
// stepping through 86400 seconds: seconds of CPU, multiplied by every
// RRULE line and every VTIMEZONE observance.
func TestNeverMatchingRulesBounded(t *testing.T) {
	const never = "RRULE:FREQ=SECONDLY;INTERVAL=7;BYHOUR=1;BYMINUTE=1;BYSECOND=1;BYMONTHDAY=31;BYMONTH=2\r\n"
	cal, err := ical.ParseBytes([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:n\r\nDTSTART:20260101T000000Z\r\n" +
		strings.Repeat(never, 3) + "END:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n := 0
	for range Expand(cal, "VEVENT", "", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), nil) {
		n++
	}
	if d := time.Since(start); d > timeScale*time.Second {
		t.Errorf("Expand took %v (%d instances)", d, n)
	}

	vtz := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:Never\r\n" + strings.Repeat("BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\n"+
		"TZOFFSETFROM:+0100\r\nTZOFFSETTO:+0100\r\n"+never+"END:STANDARD\r\n", 3) + "END:VTIMEZONE\r\nEND:VCALENDAR\r\n"
	cal, err = ical.ParseBytes([]byte(vtz))
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	LocationFromVTimezone(cal.Children[0])
	if d := time.Since(start); d > timeScale*time.Second {
		t.Errorf("VTIMEZONE took %v", d)
	}
}

// A series scans up to maxScan instances to reach a window far from its
// start; a cancelled request must stop that work.
func TestExpandContextStops(t *testing.T) {
	cal, err := ical.ParseBytes([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:s\r\nDTSTART:20000101T000000Z\r\n" +
		"RRULE:FREQ=SECONDLY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	for range ExpandContext(ctx, cal, "VEVENT", "", from, to, nil) {
	}
	if d := time.Since(start); d > 20*time.Millisecond*timeScale {
		t.Fatalf("cancelled expansion took %v", d)
	}
}
