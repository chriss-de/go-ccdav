package ical

import (
	"bytes"
	"strings"
	"testing"
)

const sample = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:abc@example.com\r\n" +
	"DTSTART;TZID=Europe/Berlin:20260105T090000\r\n" +
	"SUMMARY:Hello\\, world\\nsecond line\r\n" +
	"ATTENDEE;CN=\"Doe, Jane\";PARTSTAT=NEEDS-ACTION;ROLE=REQ-PARTICIPANT:mailto:j\r\n" +
	" ane@example.com\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestParse(t *testing.T) {
	c, err := ParseBytes([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "VCALENDAR" || len(c.Children) != 1 {
		t.Fatalf("got %+v", c)
	}
	ev := c.Children[0]
	if got := ev.Prop("SUMMARY").Text(); got != "Hello, world\nsecond line" {
		t.Errorf("summary %q", got)
	}
	att := ev.Prop("ATTENDEE")
	if att.Value != "mailto:jane@example.com" {
		t.Errorf("attendee %q", att.Value)
	}
	if att.Param("CN") != "Doe, Jane" || att.Param("PARTSTAT") != "NEEDS-ACTION" {
		t.Errorf("params %+v", att.Params)
	}
	if ev.Prop("DTSTART").Param("TZID") != "Europe/Berlin" {
		t.Error("tzid")
	}
}

func TestRoundTrip(t *testing.T) {
	c, err := ParseBytes([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	out := c.Bytes()
	c2, err := ParseBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, c2.Bytes()) {
		t.Errorf("not stable:\n%s\n---\n%s", out, c2.Bytes())
	}
	for _, l := range strings.Split(string(out), "\r\n") {
		if len(l) > 75 {
			t.Errorf("line too long: %q", l)
		}
	}
}

func TestFoldUTF8(t *testing.T) {
	c := &Component{Name: "VEVENT", Props: []*Prop{{Name: "SUMMARY", Value: strings.Repeat("äöü€", 40)}}}
	c2, err := ParseBytes(c.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if c2.Value("SUMMARY") != c.Value("SUMMARY") {
		t.Error("utf8 fold mismatch")
	}
}

func TestLFAndVCardGroups(t *testing.T) {
	in := "BEGIN:VCARD\nVERSION:3.0\nitem1.EMAIL;TYPE=INTERNET:a@b.c\nEND:VCARD\nBEGIN:VCARD\nVERSION:3.0\nEND:VCARD\n"
	cs, err := ParseAll(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("got %d", len(cs))
	}
	if p := cs[0].Prop("EMAIL"); p == nil || p.Group != "item1" {
		t.Errorf("group: %+v", p)
	}
}

func TestErrors(t *testing.T) {
	for _, in := range []string{
		"BEGIN:VCALENDAR\r\n",
		"END:VCALENDAR\r\n",
		"BEGIN:VCALENDAR\r\nnocolon\r\nEND:VCALENDAR\r\n",
		"BEGIN:A\r\nEND:B\r\n",
		"X:1\r\n",
	} {
		if _, err := ParseBytes([]byte(in)); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList(`a,b\,c,d`)
	if len(got) != 3 || got[1] != `b\,c` {
		t.Errorf("%q", got)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(sample))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := ParseBytes(b)
		if err != nil {
			return
		}
		if _, err := ParseBytes(c.Bytes()); err != nil {
			t.Fatalf("re-parse failed: %v", err)
		}
	})
}
