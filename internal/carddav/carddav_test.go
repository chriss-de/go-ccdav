package carddav

import (
	"errors"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/ical"
)

const appleCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nPRODID:-//Apple Inc.//iPhone OS 18.0//EN\r\nN:Doe;Jane;;;\r\nFN:Jane Doe\r\n" +
	"ORG:ACME\\, Inc.;\r\nitem1.EMAIL;type=INTERNET;type=WORK;type=pref:jane@acme.example\r\nitem1.X-ABLabel:_$!<Work>!$_\r\n" +
	"TEL;type=CELL;type=VOICE;type=pref:+49 170 1234567\r\nUID:jane-1\r\nEND:VCARD\r\n"

func TestValidate(t *testing.T) {
	if uid, err := Validate([]byte(appleCard)); err != nil || uid != "jane-1" {
		t.Fatal(uid, err)
	}
	for _, c := range []struct {
		in   string
		want error
	}{
		{"garbage", ErrInvalidData},
		{appleCard + appleCard, ErrInvalidData},
		{strings.Replace(appleCard, "UID:jane-1\r\n", "", 1), ErrInvalidData},
		{strings.Replace(appleCard, "VERSION:3.0\r\n", "", 1), ErrInvalidData},
		{strings.Replace(appleCard, "VERSION:3.0", "VERSION:2.1", 1), ErrUnsupportedData},
		{"BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n", ErrInvalidData},
	} {
		if _, err := Validate([]byte(c.in)); !errors.Is(err, c.want) {
			t.Errorf("%.40q: got %v, want %v", c.in, err, c.want)
		}
	}
}

func filter(t *testing.T, x string) *dav.Element {
	t.Helper()
	e, err := dav.ParseXML(strings.NewReader(`<C:filter xmlns:C="urn:ietf:params:xml:ns:carddav" ` + x))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestMatch(t *testing.T) {
	card, err := ical.ParseBytes([]byte(appleCard))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		f    string
		want bool
	}{
		{`/>`, true},
		{`><C:prop-filter name="FN"/></C:filter>`, true},
		{`><C:prop-filter name="NICKNAME"/></C:filter>`, false},
		{`><C:prop-filter name="NICKNAME"><C:is-not-defined/></C:prop-filter></C:filter>`, true},
		{`><C:prop-filter name="FN"><C:text-match>jane</C:text-match></C:prop-filter></C:filter>`, true},
		{`><C:prop-filter name="FN"><C:text-match collation="i;octet">jane</C:text-match></C:prop-filter></C:filter>`, false},
		{`><C:prop-filter name="FN"><C:text-match match-type="equals">jane doe</C:text-match></C:prop-filter></C:filter>`, true},
		{`><C:prop-filter name="FN"><C:text-match match-type="starts-with">doe</C:text-match></C:prop-filter></C:filter>`, false},
		{`><C:prop-filter name="FN"><C:text-match match-type="ends-with">DOE</C:text-match></C:prop-filter></C:filter>`, true},
		{`><C:prop-filter name="FN"><C:text-match negate-condition="yes">jane</C:text-match></C:prop-filter></C:filter>`, false},
		{`><C:prop-filter name="ORG"><C:text-match>acme, inc</C:text-match></C:prop-filter></C:filter>`, true}, // unescaped
		{`><C:prop-filter name="EMAIL"><C:text-match match-type="ends-with">@acme.example</C:text-match></C:prop-filter></C:filter>`, true},
		{`><C:prop-filter name="EMAIL"><C:param-filter name="TYPE"><C:text-match match-type="equals">work</C:text-match></C:param-filter></C:prop-filter></C:filter>`, true},
		{`><C:prop-filter name="EMAIL"><C:param-filter name="TYPE"><C:text-match match-type="equals">home</C:text-match></C:param-filter></C:prop-filter></C:filter>`, false},
		{`><C:prop-filter name="TEL"><C:param-filter name="X-FOO"><C:is-not-defined/></C:param-filter></C:prop-filter></C:filter>`, true},
		// anyof (default) vs allof on the filter
		{`><C:prop-filter name="FN"><C:text-match>zzz</C:text-match></C:prop-filter><C:prop-filter name="TEL"/></C:filter>`, true},
		{` test="allof"><C:prop-filter name="FN"><C:text-match>zzz</C:text-match></C:prop-filter><C:prop-filter name="TEL"/></C:filter>`, false},
		// anyof vs allof inside a prop-filter
		{`><C:prop-filter name="FN" test="allof"><C:text-match>jane</C:text-match><C:text-match>zzz</C:text-match></C:prop-filter></C:filter>`, false},
		{`><C:prop-filter name="FN"><C:text-match>jane</C:text-match><C:text-match>zzz</C:text-match></C:prop-filter></C:filter>`, true},
	} {
		got, err := Match(card, filter(t, c.f))
		if err != nil || got != c.want {
			t.Errorf("%s: got %v %v, want %v", c.f, got, err, c.want)
		}
	}
	for _, bad := range []string{
		` test="maybe"/>`,
		`><C:prop-filter/></C:filter>`,
		`><C:prop-filter name="FN"><C:text-match collation="x">a</C:text-match></C:prop-filter></C:filter>`,
		`><C:prop-filter name="FN"><C:text-match match-type="x">a</C:text-match></C:prop-filter></C:filter>`,
	} {
		if _, err := Match(card, filter(t, bad)); !errors.Is(err, ErrBadFilter) {
			t.Errorf("%s: expected ErrBadFilter, got %v", bad, err)
		}
	}
}

func TestRenderData(t *testing.T) {
	spec, _ := dav.ParseXML(strings.NewReader(`<C:address-data xmlns:C="urn:ietf:params:xml:ns:carddav"><C:prop name="FN"/><C:prop name="EMAIL"/></C:address-data>`))
	out, err := RenderData([]byte(appleCard), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BEGIN:VCARD", "VERSION:3.0", "UID:jane-1", "FN:Jane Doe", "item1.EMAIL"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"TEL", "ORG", "X-ABLabel"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpected %q in\n%s", unwanted, out)
		}
	}
	all, _ := dav.ParseXML(strings.NewReader(`<C:address-data xmlns:C="urn:ietf:params:xml:ns:carddav"/>`))
	if out, _ := RenderData([]byte(appleCard), all); out != appleCard {
		t.Error("full address-data must be byte-exact")
	}
}

func TestSplit(t *testing.T) {
	v21 := strings.Replace(strings.Replace(appleCard, "VERSION:3.0", "VERSION:2.1", 1), "jane-1", "old", 1)
	two := strings.Replace(appleCard, "jane-1", "jane-2", 1)
	cards, skipped, err := Split([]byte(appleCard + v21 + two))
	if err != nil || len(cards) != 2 || skipped != 1 || cards[1].UID != "jane-2" {
		t.Fatalf("%v %d %+v", err, skipped, cards)
	}
	if _, err := Validate(cards[0].Data); err != nil {
		t.Fatal(err)
	}
}
