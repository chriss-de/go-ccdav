package dav

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestParseXML(t *testing.T) {
	body := `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:x="urn:custom">
  <d:prop><d:displayname/><c:calendar-home-set/><x:foo>bar <x:b>baz</x:b></x:foo></d:prop>
</d:propfind>`
	e, err := ParseXML(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != N(NSDAV, "propfind") {
		t.Fatalf("root %v", e.Name)
	}
	prop := e.Child(NSDAV, "prop")
	if len(prop.Children) != 3 || prop.Children[1].Name != N(NSCalDAV, "calendar-home-set") {
		t.Fatalf("children %+v", prop.Children)
	}
	if foo := prop.Child("urn:custom", "foo"); foo.Inner != "bar <x:b>baz</x:b>" {
		t.Errorf("inner %q", foo.Inner)
	}
	if e, err := ParseXML(strings.NewReader("  ")); e != nil || err != nil {
		t.Error("empty body")
	}
	if _, err := ParseXML(strings.NewReader("<a><b></a>")); err == nil {
		t.Error("bad xml accepted")
	}
}

func TestMultistatus(t *testing.T) {
	r := &Response{Href: "/dav/calendars/alice/my cal/"}
	r.AddProp(200, Prop{Name: N(NSDAV, "displayname"), Value: Esc("A & B")})
	r.AddProp(404, Prop{Name: N("urn:x", "y")})
	ms := &Multistatus{Responses: []*Response{r}}
	out := ms.String()
	for _, want := range []string{
		`<d:href>/dav/calendars/alice/my%20cal/</d:href>`,
		`<d:displayname>A &amp; B</d:displayname>`,
		`<x:y xmlns:x="urn:x"/>`,
		`HTTP/1.1 404 Not Found`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	// Must be well-formed.
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Fatal(err)
	}
}

func TestParseXMLManyTextChunks(t *testing.T) {
	// Text split by many comments must not take quadratic time.
	n := (maxXMLBody - 10) / 8
	start := time.Now()
	e, err := ParseXML(strings.NewReader("<a>" + strings.Repeat("x<!---->", n) + "</a>"))
	if err != nil || len(e.Text) != n {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("took %v", d)
	}
}
