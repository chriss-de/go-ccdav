package caldav

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

func TestFreeBusy(t *testing.T) {
	src := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\n" +
		// Daily 09:00-10:00Z x4; instance 2 cancelled via override.
		"BEGIN:VEVENT\r\nUID:d\r\nDTSTART:20260105T090000Z\r\nDTEND:20260105T100000Z\r\nRRULE:FREQ=DAILY;COUNT=4\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:d\r\nRECURRENCE-ID:20260106T090000Z\r\nDTSTART:20260106T090000Z\r\nDTEND:20260106T100000Z\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	other := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:o\r\nDTSTART:20260105T093000Z\r\nDTEND:20260105T110000Z\r\nEND:VEVENT\r\n" + // overlaps → merged
		"BEGIN:VEVENT\r\nUID:t\r\nDTSTART:20260107T140000Z\r\nDTEND:20260107T150000Z\r\nSTATUS:TENTATIVE\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:x\r\nDTSTART:20260107T160000Z\r\nDTEND:20260107T170000Z\r\nTRANSP:TRANSPARENT\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:z\r\nDTSTART:20260107T180000Z\r\nEND:VEVENT\r\n" + // zero length
		"END:VCALENDAR\r\n"
	var cals []*ical.Component
	for _, s := range []string{src, other} {
		c, err := ical.ParseBytes([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		cals = append(cals, c)
	}
	from := time.Date(2026, 1, 5, 9, 45, 0, 0, time.UTC) // clips the first period
	to := time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC)
	ps, err := FreeBusy(context.Background(), cals, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ps {
		got = append(got, p.Type+" "+p.Start.Format("02T15:04")+"-"+p.End.Format("02T15:04"))
	}
	want := []string{"BUSY 05T09:45-05T11:00", "BUSY 07T09:00-07T10:00", "BUSY 08T09:00-08T10:00", "BUSY-TENTATIVE 07T14:00-07T15:00"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	out := string(FreeBusyCalendar(ps, from, to, from).Bytes())
	for _, w := range []string{"BEGIN:VFREEBUSY", "DTSTART:20260105T094500Z", "FREEBUSY;FBTYPE=BUSY:20260105T094500Z/20260105T110000Z,",
		"FREEBUSY;FBTYPE=BUSY-TENTATIVE:20260107T140000Z/20260107T150000Z"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in\n%s", w, out)
		}
	}
}

// Every instance became a period before merging: a minutely event with
// gaps gave half a million periods for one year, and gigabytes for a few
// such events.
func TestFreeBusyBounded(t *testing.T) {
	cal, err := ical.ParseBytes([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:m\r\nDTSTART:20260101T000000Z\r\n" +
		"DURATION:PT30S\r\nRRULE:FREQ=MINUTELY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	before := ms.TotalAlloc
	ps, err := FreeBusy(context.Background(), []*ical.Component{cal}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	runtime.ReadMemStats(&ms)
	if !errors.Is(err, ErrFreeBusyTooLarge) || len(ps) > MaxFreeBusyPeriods {
		t.Errorf("%d periods, err %v", len(ps), err)
	}
	if mb := (ms.TotalAlloc - before) >> 20; mb > 200 {
		t.Errorf("allocated %d MB", mb)
	}
}
