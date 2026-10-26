package caldav

import (
	"strings"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/ical"
)

// UIDOf feeds the store's UID index; it must agree with what Validate
// accepted, or a decoy in an X- component dodges the UID conflict check.
func TestUIDOfMatchesValidate(t *testing.T) {
	data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:X-FOO\r\nUID:decoy\r\nEND:X-FOO\r\n" +
		"BEGIN:VEVENT\r\nUID:real\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	cal, err := ical.ParseBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	vu, vc, err := Validate(cal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u, c := UIDOf(data); u != vu || c != vc {
		t.Fatalf("UIDOf = %q %q, Validate = %q %q", u, c, vu, vc)
	}
}

func alarmQuery(t *testing.T, ics, start, end string) (bool, time.Duration) {
	t.Helper()
	f, err := dav.ParseXML(strings.NewReader(`<c:filter xmlns:c="urn:ietf:params:xml:ns:caldav"><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">` +
		`<c:comp-filter name="VALARM"><c:time-range start="` + start + `" end="` + end + `"/></c:comp-filter></c:comp-filter></c:comp-filter></c:filter>`))
	if err != nil {
		t.Fatal(err)
	}
	cal, err := ical.ParseBytes([]byte(ics))
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	ok, err := (&Query{Filter: f}).Match(cal)
	if err != nil {
		t.Fatal(err)
	}
	return ok, time.Since(began)
}

func alarmEvent(rrule, alarm string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:20260101T100000Z\r\n" + rrule +
		"BEGIN:VALARM\r\nACTION:DISPLAY\r\n" + alarm + "END:VALARM\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

// REPEAT and DURATION widened the searched window unchecked: a huge pair
// made one object cost a minute of CPU.
func TestAlarmRepeatBounded(t *testing.T) {
	ics := alarmEvent("RRULE:FREQ=MINUTELY\r\n", "TRIGGER:-PT1M\r\nREPEAT:2000000000\r\nDURATION:P9999999D\r\n")
	if _, d := alarmQuery(t, ics, "20250101T000000Z", "20250102T000000Z"); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
	// Ordinary repeats still match: 09:50, 09:55, 10:00.
	ics = alarmEvent("", "TRIGGER:-PT10M\r\nREPEAT:2\r\nDURATION:PT5M\r\n")
	for _, c := range []struct {
		start, end string
		want       bool
	}{
		{"20260101T095900Z", "20260101T100100Z", true},
		{"20260101T100100Z", "20260101T110000Z", false},
	} {
		if ok, _ := alarmQuery(t, ics, c.start, c.end); ok != c.want {
			t.Errorf("%s-%s: %v, want %v", c.start, c.end, ok, c.want)
		}
	}
}
