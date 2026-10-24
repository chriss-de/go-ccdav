package caldav

import (
	"strings"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/ical"
)

const fuzzICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\nBEGIN:STANDARD\r\nDTSTART:19701025T030000\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\nUID:a\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;TZID=Europe/Berlin:20260105T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY;COUNT=5\r\nSUMMARY:Weekly\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\nEND:VALARM\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:a\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID;TZID=Europe/Berlin:20260112T090000\r\nDTSTART;TZID=Europe/Berlin:20260112T100000\r\nDURATION:PT1H\r\nEND:VEVENT\r\n" +
	"BEGIN:VTODO\r\nUID:t\r\nDTSTAMP:20260101T000000Z\r\nDUE:20260110\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"

var fuzzFilters = []string{
	`<c:filter xmlns:c="urn:ietf:params:xml:ns:caldav"><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="20260110T000000Z" end="20260120T000000Z"/></c:comp-filter></c:comp-filter></c:filter>`,
	`<c:filter xmlns:c="urn:ietf:params:xml:ns:caldav"><c:comp-filter name="VCALENDAR"><c:comp-filter name="VTODO"><c:prop-filter name="DUE"><c:is-not-defined/></c:prop-filter></c:comp-filter></c:comp-filter></c:filter>`,
	`<c:filter xmlns:c="urn:ietf:params:xml:ns:caldav"><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:prop-filter name="SUMMARY"><c:text-match collation="i;unicode-casemap" negate-condition="yes">week</c:text-match></c:prop-filter><c:comp-filter name="VALARM"><c:time-range start="20260105T000000Z"/></c:comp-filter></c:comp-filter></c:comp-filter></c:filter>`,
	`<c:filter xmlns:c="urn:ietf:params:xml:ns:caldav"><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:prop-filter name="DTSTART"><c:param-filter name="TZID"><c:text-match>Berlin</c:text-match></c:param-filter></c:prop-filter></c:comp-filter></c:comp-filter></c:filter>`,
}

// FuzzQuery runs calendar-query filters against calendar objects.
func FuzzQuery(f *testing.F) {
	for _, flt := range fuzzFilters {
		f.Add(flt, fuzzICS)
	}
	f.Fuzz(func(t *testing.T, filter, ics string) {
		fe, err := dav.ParseXML(strings.NewReader(filter))
		if err != nil || fe == nil {
			return
		}
		cal, err := ical.ParseBytes([]byte(ics))
		if err != nil {
			return
		}
		(&Query{Filter: fe, Floating: time.UTC}).Match(cal)
	})
}

// FuzzRenderData exercises calendar-data partial retrieval and expand.
func FuzzRenderData(f *testing.F) {
	for _, spec := range []string{
		`<c:calendar-data xmlns:c="urn:ietf:params:xml:ns:caldav"/>`,
		`<c:calendar-data xmlns:c="urn:ietf:params:xml:ns:caldav"><c:expand start="20260101T000000Z" end="20260201T000000Z"/></c:calendar-data>`,
		`<c:calendar-data xmlns:c="urn:ietf:params:xml:ns:caldav"><c:comp name="VCALENDAR"><c:prop name="VERSION"/><c:comp name="VEVENT"><c:prop name="SUMMARY"/><c:prop name="UID"/></c:comp></c:comp></c:calendar-data>`,
		`<c:calendar-data xmlns:c="urn:ietf:params:xml:ns:caldav"><c:limit-recurrence-set start="20260110T000000Z" end="20260111T000000Z"/></c:calendar-data>`,
	} {
		f.Add(spec, fuzzICS)
	}
	f.Fuzz(func(t *testing.T, spec, ics string) {
		se, err := dav.ParseXML(strings.NewReader(spec))
		if err != nil || se == nil {
			return
		}
		RenderData([]byte(ics), se, time.UTC)
	})
}

// FuzzSplit checks that every object an import produces carries the UID
// it was filed under.
func FuzzSplit(f *testing.F) {
	f.Add([]byte(fuzzICS))
	f.Add([]byte(fuzzICS + fuzzICS))
	f.Fuzz(func(t *testing.T, b []byte) {
		objs, _, err := Split(b)
		if err != nil {
			return
		}
		for _, o := range objs {
			if uid, _ := UIDOf(o.Data); uid != o.UID {
				t.Fatalf("object filed as %q has UID %q:\n%s", o.UID, uid, o.Data)
			}
			if cal, err := ical.ParseBytes(o.Data); err == nil {
				Validate(cal, nil)
			}
		}
	})
}
