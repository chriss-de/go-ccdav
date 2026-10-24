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
			if !strings.Contains(s, e.Inner) {
				t.Fatalf("Inner %q is not part of the input", e.Inner)
			}
			for _, c := range e.Children {
				walk(c, depth+1)
			}
		}
		walk(e, 0)
	})
}
