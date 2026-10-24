// Package carddav implements the CardDAV-specific logic on top of parsed
// vCards: object validation (RFC 6352 §5.1), addressbook-query filters
// (§10.5) and address-data partial retrieval (§10.4).
//
// vCards are stored as uploaded; there is no conversion between vCard 3.0
// and 4.0, and contact groups (Apple's X-ADDRESSBOOKSERVER-KIND/MEMBER or
// vCard 4 KIND/MEMBER) are plain client data.
package carddav

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/ical"
)

// Validation errors map to CardDAV preconditions.
var (
	ErrInvalidData     = errors.New("carddav: invalid address data")     // valid-address-data
	ErrUnsupportedData = errors.New("carddav: unsupported address data") // supported-address-data
)

// Validate checks an address object resource and returns its UID.
// Exactly one VCARD, version 3.0 or 4.0, with a UID.
func Validate(data []byte) (uid string, err error) {
	cards, err := ical.ParseAll(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidData, err)
	}
	if len(cards) != 1 || cards[0].Name != "VCARD" {
		return "", fmt.Errorf("%w: expected exactly one VCARD", ErrInvalidData)
	}
	c := cards[0]
	switch v := strings.TrimSpace(c.Value("VERSION")); v {
	case "3.0", "4.0":
	case "":
		return "", fmt.Errorf("%w: missing VERSION", ErrInvalidData)
	default:
		return "", fmt.Errorf("%w: vCard version %s (only 3.0 and 4.0)", ErrUnsupportedData, v)
	}
	uid = strings.TrimSpace(c.Value("UID"))
	if uid == "" {
		return "", fmt.Errorf("%w: missing UID", ErrInvalidData)
	}
	return uid, nil
}

// UIDOf returns the UID of a vCard, "" if unparsable.
func UIDOf(data []byte) string {
	c, err := ical.ParseBytes(data)
	if err != nil || c.Name != "VCARD" {
		return ""
	}
	return strings.TrimSpace(c.Value("UID"))
}

// ErrBadFilter is returned for malformed filters (→ 403 / valid-filter).
var ErrBadFilter = errors.New("carddav: invalid filter")

// Match evaluates a CARDDAV:filter element against a vCard. A nil or
// empty filter matches everything.
func Match(card *ical.Component, filter *dav.Element) (bool, error) {
	if filter == nil {
		return true, nil
	}
	allof, err := testAttr(filter)
	if err != nil {
		return false, err
	}
	pfs := filter.ChildrenNamed(dav.NSCardDAV, "prop-filter")
	if len(pfs) == 0 {
		return true, nil
	}
	results := make([]bool, len(pfs))
	for i, pf := range pfs {
		if results[i], err = propFilter(card, pf); err != nil {
			return false, err
		}
	}
	return combine(allof, results), nil
}

func testAttr(e *dav.Element) (allof bool, err error) {
	switch e.Attr("test") {
	case "", "anyof":
		return false, nil
	case "allof":
		return true, nil
	}
	return false, fmt.Errorf("%w: bad test attribute %q", ErrBadFilter, e.Attr("test"))
}

func combine(allof bool, results []bool) bool {
	if len(results) == 0 {
		return true
	}
	for _, r := range results {
		if allof && !r {
			return false
		}
		if !allof && r {
			return true
		}
	}
	return allof
}

func propFilter(card *ical.Component, f *dav.Element) (bool, error) {
	name := strings.ToUpper(f.Attr("name"))
	if name == "" {
		return false, fmt.Errorf("%w: prop-filter without name", ErrBadFilter)
	}
	props := card.PropsNamed(name)
	if f.Child(dav.NSCardDAV, "is-not-defined") != nil {
		return len(props) == 0, nil
	}
	allof, err := testAttr(f)
	if err != nil {
		return false, err
	}
	tms := f.ChildrenNamed(dav.NSCardDAV, "text-match")
	pfs := f.ChildrenNamed(dav.NSCardDAV, "param-filter")
	for _, p := range props {
		var results []bool
		for _, tm := range tms {
			ok, err := textMatch(tm, p.Text())
			if err != nil {
				return false, err
			}
			results = append(results, ok)
		}
		for _, pf := range pfs {
			ok, err := paramFilter(p, pf)
			if err != nil {
				return false, err
			}
			results = append(results, ok)
		}
		if combine(allof, results) {
			return true, nil
		}
	}
	return false, nil
}

func paramFilter(p *ical.Prop, f *dav.Element) (bool, error) {
	name := strings.ToUpper(f.Attr("name"))
	if name == "" {
		return false, fmt.Errorf("%w: param-filter without name", ErrBadFilter)
	}
	var vals []string
	for _, pa := range p.Params {
		if pa.Name == name {
			for _, v := range pa.Values {
				// vCard 3 often packs several types into one value.
				vals = append(vals, strings.Split(v, ",")...)
			}
		}
	}
	if f.Child(dav.NSCardDAV, "is-not-defined") != nil {
		return len(vals) == 0, nil
	}
	tm := f.Child(dav.NSCardDAV, "text-match")
	if tm == nil {
		return len(vals) > 0, nil
	}
	for _, v := range vals {
		ok, err := textMatch(tm, v)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// textMatch implements CARDDAV:text-match: match-type equals, contains
// (default), starts-with, ends-with; collations i;unicode-casemap
// (default), i;ascii-casemap and i;octet; negate-condition.
func textMatch(tm *dav.Element, value string) (bool, error) {
	needle := tm.Text
	switch tm.Attr("collation") {
	case "", "i;unicode-casemap":
		value, needle = strings.ToLower(value), strings.ToLower(needle)
	case "i;ascii-casemap":
		value, needle = asciiLower(value), asciiLower(needle)
	case "i;octet":
	default:
		return false, fmt.Errorf("%w: unsupported collation %q", ErrBadFilter, tm.Attr("collation"))
	}
	var ok bool
	switch tm.Attr("match-type") {
	case "", "contains":
		ok = strings.Contains(value, needle)
	case "equals":
		ok = value == needle
	case "starts-with":
		ok = strings.HasPrefix(value, needle)
	case "ends-with":
		ok = strings.HasSuffix(value, needle)
	default:
		return false, fmt.Errorf("%w: unsupported match-type %q", ErrBadFilter, tm.Attr("match-type"))
	}
	if tm.Attr("negate-condition") == "yes" {
		ok = !ok
	}
	return ok, nil
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// alwaysIncluded are kept in partial address-data so the result is still
// a valid vCard (RFC 6352 §10.4.2).
var alwaysIncluded = map[string]bool{"VERSION": true, "UID": true}

// RenderData applies a CARDDAV:address-data request element to a stored
// vCard. Without prop selection the raw bytes are returned unchanged.
func RenderData(raw []byte, spec *dav.Element) (string, error) {
	if spec == nil || spec.Child(dav.NSCardDAV, "allprop") != nil {
		return string(raw), nil
	}
	sel := spec.ChildrenNamed(dav.NSCardDAV, "prop")
	if len(sel) == 0 {
		return string(raw), nil
	}
	card, err := ical.ParseBytes(raw)
	if err != nil {
		return "", err
	}
	want := map[string]bool{}
	novalue := map[string]bool{}
	for _, p := range sel {
		n := strings.ToUpper(p.Attr("name"))
		want[n] = true
		if p.Attr("novalue") == "yes" {
			novalue[n] = true
		}
	}
	out := &ical.Component{Name: card.Name}
	for _, p := range card.Props {
		if !want[p.Name] && !alwaysIncluded[p.Name] {
			continue
		}
		if novalue[p.Name] {
			np := *p
			np.Value = ""
			p = &np
		}
		out.Props = append(out.Props, p)
	}
	return string(out.Bytes()), nil
}

// Card is one address object resource produced by Split.
type Card struct {
	UID  string
	Data []byte
}

// Split turns a .vcf file with any number of vCards into address object
// resources. vCards that fail Validate are counted as skipped.
func Split(data []byte) (cards []Card, skipped int, err error) {
	comps, err := ical.ParseAll(bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	for _, c := range comps {
		b := c.Bytes()
		uid, err := Validate(b)
		if err != nil {
			skipped++
			continue
		}
		cards = append(cards, Card{UID: uid, Data: b})
	}
	return cards, skipped, nil
}
