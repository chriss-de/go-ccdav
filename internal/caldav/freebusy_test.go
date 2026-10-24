package caldav

import (
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
	ps := FreeBusy(cals, from, to, nil)
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
