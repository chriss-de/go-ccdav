package dav

import (
	"strings"
	"testing"
)

func FuzzParseXML(f *testing.F) {
	for _, s := range []string{
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`,
		`<d:propertyupdate xmlns:d="DAV:"><d:set><d:prop><x:color xmlns:x="urn:x">#fff</x:color></d:prop></d:set></d:propertyupdate>`,
		`<c:calendar-query xmlns:c="urn:ietf:params:xml:ns:caldav"><c:filter><c:comp-filter name="VCALENDAR"/></c:filter></c:calendar-query>`,
		`<a>text<!-- c -->more<![CDATA[<raw>]]></a>`,
		`<a><b></a>`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := ParseXML(strings.NewReader(s))
		if err != nil || e == nil {
			return
		}
		var walk func(*Element, int)
		walk = func(e *Element, depth int) {
			if depth > 66 {
				t.Fatal("depth limit not enforced")
			}
			// Inner stands alone: it parses wrapped in a bare element, with
			// the same children and text.
			w, err := ParseXML(strings.NewReader("<w>" + e.Inner + "</w>"))
			if err != nil {
				t.Fatalf("Inner %q does not parse: %v", e.Inner, err)
			}
			if w.Text != e.Text || len(w.Children) != len(e.Children) {
				t.Fatalf("Inner %q: text %q, %d children; want %q, %d", e.Inner, w.Text, len(w.Children), e.Text, len(e.Children))
			}
			for i, c := range w.Children {
				if c.Name != e.Children[i].Name {
					t.Fatalf("Inner %q: child %v, want %v", e.Inner, c.Name, e.Children[i].Name)
				}
			}
			for _, c := range e.Children {
				walk(c, depth+1)
			}
		}
		walk(e, 0)
	})
}
