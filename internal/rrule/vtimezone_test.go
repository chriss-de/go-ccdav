package rrule

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// TestVTimezone checks generated VTIMEZONEs against Go's zone data: the
// offset must agree every hour (and around each transition) for decades.
func TestVTimezone(t *testing.T) {
	zones := []string{
		"Europe/Berlin", "Europe/London", "Europe/Moscow", "America/New_York", "America/Sao_Paulo",
		"America/Santiago", "Australia/Sydney", "Australia/Lord_Howe", "Pacific/Auckland", "Pacific/Chatham",
		"Asia/Tokyo", "Asia/Kolkata", "Asia/Kathmandu", "Asia/Tehran", "Africa/Casablanca", "America/Havana",
		"Europe/Dublin", "Pacific/Apia", "Asia/Gaza", "UTC",
	}
	from := time.Date(1996, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range zones {
		t.Run(name, func(t *testing.T) {
			vtz, err := VTimezone(name, from)
			if err != nil {
				t.Fatal(err)
			}
			// Round trip through text, as a client would see it.
			cal, err := ical.ParseBytes((&ical.Component{Name: "VCALENDAR", Children: []*ical.Component{vtz}}).Bytes())
			if err != nil {
				t.Fatal(err)
			}
			got, err := LocationFromVTimezone(cal.Children[0])
			if err != nil {
				t.Fatal(err)
			}
			want, _ := time.LoadLocation(name)
			end := time.Date(time.Now().Year()+8, 1, 1, 0, 0, 0, 0, time.UTC)
			for at := from; at.Before(end); at = at.Add(time.Hour) {
				_, w := at.In(want).Zone()
				_, g := at.In(got).Zone()
				if w != g {
					t.Fatalf("%s at %s: offset %d, want %d\n%s", name, at, g, w, cal.Bytes())
				}
			}
			if n := strings.Count(string(cal.Bytes()), "BEGIN:"); n > 120 {
				t.Errorf("%d components: rules not compressed", n)
			}
		})
	}
	if _, err := VTimezone("Mars/Olympus", from); err == nil {
		t.Error("unknown zone accepted")
	}
}

func TestVTimezoneBerlinIsCompact(t *testing.T) {
	vtz, _ := VTimezone("Europe/Berlin", time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC))
	got := string((&ical.Component{Name: "VCALENDAR", Children: []*ical.Component{vtz}}).Bytes())
	for _, want := range []string{"RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU", "RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU", "TZNAME:CEST", "TZOFFSETTO:+0200"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "BEGIN:DAYLIGHT")+strings.Count(got, "BEGIN:STANDARD") != 3 {
		t.Errorf("want initial + 2 rules:\n%s", got)
	}
}

// Many observances share one transition budget; without it each could
// add vtzMaxTransitions and a small VTIMEZONE would need gigabytes.
func TestVTimezoneTransitionBudget(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:Many\r\n")
	for i := range 50 {
		fmt.Fprintf(&sb, "BEGIN:STANDARD\r\nDTSTART:18000101T0000%02d\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0100\r\nRRULE:FREQ=DAILY\r\nEND:STANDARD\r\n", i)
	}
	sb.WriteString("END:VTIMEZONE\r\nEND:VCALENDAR\r\n")
	cal, err := ical.ParseBytes([]byte(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	before := ms.TotalAlloc
	if _, err := LocationFromVTimezone(cal.Children[0]); err == nil {
		t.Fatal("accepted a VTIMEZONE with 500000 transitions")
	}
	runtime.ReadMemStats(&ms)
	if mb := (ms.TotalAlloc - before) >> 20; mb > 50 {
		t.Fatalf("allocated %d MB", mb)
	}
}
