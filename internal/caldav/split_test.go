package caldav

import (
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

const exportIcs = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Google Inc//Google Calendar 70.9054//EN\r\nMETHOD:PUBLISH\r\n" +
	"BEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\nBEGIN:STANDARD\r\nDTSTART:19701025T030000\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
	"BEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nBEGIN:STANDARD\r\nDTSTART:19701101T020000\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\nUID:series\r\nDTSTART;TZID=Europe/Berlin:20260105T090000\r\nRRULE:FREQ=WEEKLY;COUNT=3\r\nSUMMARY:S\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:single\r\nDTSTART:20260110T100000Z\r\nSUMMARY:One\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:series\r\nRECURRENCE-ID;TZID=Europe/Berlin:20260112T090000\r\nDTSTART;TZID=Europe/Berlin:20260112T110000\r\nSUMMARY:S moved\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nDTSTART:20260110T100000Z\r\nSUMMARY:no uid\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestSplitMerge(t *testing.T) {
	objs, skipped, err := Split([]byte(exportIcs))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || skipped != 1 || objs[0].UID != "series" || objs[1].UID != "single" {
		t.Fatalf("%d objects, %d skipped: %+v", len(objs), skipped, objs)
	}
	series := string(objs[0].Data)
	if strings.Count(series, "BEGIN:VEVENT") != 2 || !strings.Contains(series, "TZID:Europe/Berlin") ||
		strings.Contains(series, "New_York") || strings.Contains(series, "METHOD") {
		t.Fatalf("series object:\n%s", series)
	}
	if strings.Contains(string(objs[1].Data), "VTIMEZONE") {
		t.Fatal("UTC event got a VTIMEZONE")
	}
	for _, o := range objs {
		cal, err := ical.ParseBytes(o.Data)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Validate(cal, nil); err != nil {
			t.Fatalf("%s: %v", o.UID, err)
		}
	}

	merged := string(Merge([][]byte{objs[0].Data, objs[1].Data, objs[0].Data, []byte("junk")}, "My, cal"))
	if strings.Count(merged, "BEGIN:VTIMEZONE") != 1 || strings.Count(merged, "BEGIN:VEVENT") != 5 ||
		!strings.Contains(merged, `X-WR-CALNAME:My\, cal`) {
		t.Fatalf("merged:\n%s", merged)
	}
	if _, err := ical.ParseBytes([]byte(merged)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Split([]byte("garbage")); err == nil {
		t.Fatal("garbage accepted")
	}
}
