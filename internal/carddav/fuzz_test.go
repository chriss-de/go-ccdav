package carddav

import (
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/ical"
)

const fuzzVCF = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:c1\r\nFN:Max Muster\r\nN:Muster;Max;;;\r\nEMAIL;TYPE=INTERNET,WORK:max@example.org\r\nTEL;TYPE=CELL:+49 170 1234\r\nitem1.ADR:;;Street 1;City;;12345;DE\r\nCATEGORIES:sport,family\r\nEND:VCARD\r\n"

// FuzzMatch runs addressbook-query filters against vCards.
func FuzzMatch(f *testing.F) {
	for _, flt := range []string{
		`<card:filter xmlns:card="urn:ietf:params:xml:ns:carddav"><card:prop-filter name="FN"><card:text-match>muster</card:text-match></card:prop-filter></card:filter>`,
		`<card:filter xmlns:card="urn:ietf:params:xml:ns:carddav" test="allof"><card:prop-filter name="EMAIL"><card:text-match match-type="ends-with" collation="i;octet">.org</card:text-match></card:prop-filter><card:prop-filter name="NICKNAME"><card:is-not-defined/></card:prop-filter></card:filter>`,
		`<card:filter xmlns:card="urn:ietf:params:xml:ns:carddav"><card:prop-filter name="TEL" test="anyof"><card:param-filter name="TYPE"><card:text-match match-type="equals" negate-condition="yes">cell</card:text-match></card:param-filter></card:prop-filter></card:filter>`,
	} {
		f.Add(flt, fuzzVCF)
	}
	f.Fuzz(func(t *testing.T, filter, vcf string) {
		fe, err := dav.ParseXML(strings.NewReader(filter))
		if err != nil || fe == nil {
			return
		}
		card, err := ical.ParseBytes([]byte(vcf))
		if err != nil {
			return
		}
		Match(card, fe)
	})
}

// FuzzRenderData exercises partial address-data.
func FuzzRenderData(f *testing.F) {
	for _, spec := range []string{
		`<card:address-data xmlns:card="urn:ietf:params:xml:ns:carddav"/>`,
		`<card:address-data xmlns:card="urn:ietf:params:xml:ns:carddav"><card:prop name="FN"/><card:prop name="EMAIL"/></card:address-data>`,
	} {
		f.Add(spec, fuzzVCF)
	}
	f.Fuzz(func(t *testing.T, spec, vcf string) {
		se, err := dav.ParseXML(strings.NewReader(spec))
		if err != nil || se == nil {
			return
		}
		RenderData([]byte(vcf), se)
	})
}

// FuzzSplit checks that every card an import produces validates with the
// UID it was filed under.
func FuzzSplit(f *testing.F) {
	f.Add([]byte(fuzzVCF))
	f.Add([]byte(fuzzVCF + strings.Replace(fuzzVCF, "c1", "c2", 1)))
	f.Fuzz(func(t *testing.T, b []byte) {
		cards, _, err := Split(b)
		if err != nil {
			return
		}
		for _, c := range cards {
			if uid, err := Validate(c.Data); err != nil || uid != c.UID {
				t.Fatalf("card filed as %q: uid %q, %v\n%s", c.UID, uid, err, c.Data)
			}
		}
	})
}
