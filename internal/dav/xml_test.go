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
	if foo := prop.Child("urn:custom", "foo"); foo.Inner != `bar <n:b xmlns:n="urn:custom">baz</n:b>` {
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
	// Linear takes milliseconds; quadratic would take many seconds. The
	// generous bound keeps this stable under -race and on slow CI runners.
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

// Namespaces may contain "}", local names can't: keys must round-trip.
func TestKeyRoundTrip(t *testing.T) {
	for _, n := range []xml.Name{N("urn:a}b", "c"), N("", "d"), N(NSDAV, "displayname"), N("}", "x")} {
		if got := ParseKey(Key(n)); got != n {
			t.Errorf("ParseKey(Key(%v)) = %v", n, got)
		}
	}
}

// Inner is stored as a dead property and later written into other
// documents, without the request's namespace declarations: it has to
// declare what it uses.
func TestInnerIsSelfContained(t *testing.T) {
	body := `<d:propertyupdate xmlns:d="DAV:" xmlns:y="urn:y" xmlns:z="urn:z"><d:prop>` +
		`<y:p>a &amp; b <y:b z:attr="1" plain="2">c</y:b><!-- note --><d:e/></y:p></d:prop></d:propertyupdate>`
	e, err := ParseXML(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	p := e.Child(NSDAV, "prop").Child("urn:y", "p")
	doc := "<root>" + E(N("urn:y", "p"), p.Inner) + "</root>"
	re, err := ParseXML(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("%v\n%s", err, doc)
	}
	got := re.Children[0]
	if got.Text != "a & b " || len(got.Children) != 2 {
		t.Fatalf("%q %d\n%s", got.Text, len(got.Children), doc)
	}
	b := got.Children[0]
	if b.Name != N("urn:y", "b") || b.Attr("plain") != "2" || got.Children[1].Name != N(NSDAV, "e") {
		t.Fatalf("%+v\n%s", b, doc)
	}
	found := false
	for _, a := range b.Attrs {
		found = found || a.Name == N("urn:z", "attr") && a.Value == "1"
	}
	if !found {
		t.Fatalf("namespaced attribute lost: %+v\n%s", b.Attrs, doc)
	}
}
