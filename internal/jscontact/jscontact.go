// Package jscontact converts vCards (3.0 and 4.0) to JSContact cards
// (RFC 9553) following the mapping of RFC 9555. Cards are plain
// map[string]any trees, ready for JSON and for JMAP property selection.
//
// What has no JSContact equivalent is kept: unknown properties in
// vCardProps (as jCard arrays, RFC 7095) and unmapped parameters in the
// vCardParams of the property they belong to. Apple's grouped labels
// (item1.X-ABLabel) become the label of the grouped property.
//
// Ids of entries in maps like emails or phones come from the line's
// PROP-ID (X-PROP-ID in version 3), which Merge writes; without one they
// are positional ("e1", "e2", ...), stable as long as the vCard doesn't
// change. Merge (merge.go) is the way back.
package jscontact

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// Codec converts stored vCards for JMAP.
type Codec struct{}

// ToJMAP parses one vCard and converts it.
func (Codec) ToJMAP(raw []byte) (map[string]any, error) {
	c, err := ical.ParseBytes(raw)
	if err != nil {
		return nil, err
	}
	return FromVCard(c)
}

// FromVCard converts a parsed VCARD component.
func FromVCard(vc *ical.Component) (map[string]any, error) {
	card, _, err := convert(vc)
	return card, err
}

// loc is where a vCard line went in the card: a top-level property and,
// for one entry of a map property like emails, the entry's id. An empty
// id means the line belongs to the property as a whole; an empty prop
// means it maps to nothing (VERSION).
type loc struct{ prop, id string }

// entryProps are the card's map properties of entries with ids, with
// the prefix of their positional ids.
var entryProps = map[string]string{
	"nicknames": "n", "organizations": "o", "titles": "t", "emails": "e", "phones": "p",
	"addresses": "a", "links": "l", "media": "m", "cryptoKeys": "k", "calendars": "c",
	"schedulingAddresses": "s", "directories": "d", "onlineServices": "s", "anniversaries": "k",
	"notes": "n", "preferredLanguages": "l",
}

// convert converts vc and reports for each of its properties where it
// went in the card.
func convert(vc *ical.Component) (map[string]any, []loc, error) {
	if vc == nil || vc.Name != "VCARD" {
		return nil, nil, errors.New("jscontact: not a VCARD")
	}
	b := &builder{card: map[string]any{"@type": "Card", "version": "1.0"}, labels: map[string]string{}, labelUsed: map[string]bool{}}
	// Apple-style labels apply to the other properties of their group.
	for _, p := range vc.Props {
		if p.Name == "X-ABLABEL" && p.Group != "" {
			b.labels[strings.ToUpper(p.Group)] = appleLabel(p.Text())
		}
	}
	origins := make([]loc, len(vc.Props))
	for i, p := range vc.Props {
		b.at = loc{}
		b.prop(p)
		origins[i] = b.at
	}
	// A used label goes with the line of its group; any other stays as is.
	groupLoc := map[string]loc{}
	for i, p := range vc.Props {
		g := strings.ToUpper(p.Group)
		if _, seen := groupLoc[g]; !seen && g != "" && p.Name != "X-ABLABEL" && origins[i].prop != "vCardProps" {
			groupLoc[g] = origins[i]
		}
	}
	for i, p := range vc.Props {
		if p.Name != "X-ABLABEL" {
			continue
		}
		g := strings.ToUpper(p.Group)
		if l, ok := groupLoc[g]; ok && b.labelUsed[g] {
			origins[i] = l
		} else {
			b.unknownProp(p)
			origins[i] = loc{"vCardProps", ""}
		}
	}
	if b.name != nil {
		b.card["name"] = b.name
	}
	if len(b.unknown) > 0 {
		b.card["vCardProps"] = b.unknown
	}
	// JSContact properties without a vCard equivalent, stored by Merge.
	for _, j := range b.jsprops {
		setPointer(b.card, j.ptr, j.val)
	}
	return b.card, origins, nil
}

type builder struct {
	card      map[string]any
	name      map[string]any
	labels    map[string]string // vCard group -> label
	labelUsed map[string]bool   // groups whose label went into a mapped property
	unknown   []any             // vCardProps
	jsprops   []jsprop
	at        loc    // where the current line went
	propID    string // PROP-ID of the current line
}

type jsprop struct {
	ptr string
	val any
}

// entry adds an object to the map property prop. Its id is the line's
// PROP-ID if it has a usable one, else prefix and position.
func (b *builder) entry(prop, prefix string, obj map[string]any) {
	m, _ := b.card[prop].(map[string]any)
	if m == nil {
		m = map[string]any{}
		b.card[prop] = m
	}
	id := b.propID
	b.propID = ""
	if !validID(id) || m[id] != nil {
		for n := len(m) + 1; ; n++ {
			if id = prefix + strconv.Itoa(n); m[id] == nil {
				break
			}
		}
	}
	m[id] = obj
	b.at = loc{prop, id}
}

// validID reports whether s is a JMAP Id (RFC 8620 §1.2).
func validID(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (b *builder) nameObj() map[string]any {
	if b.name == nil {
		b.name = map[string]any{"@type": "Name"}
	}
	b.at = loc{"name", ""}
	return b.name
}

// whole marks the current line as belonging to prop as a whole.
func (b *builder) whole(prop string) { b.at = loc{prop, ""} }

func (b *builder) prop(p *ical.Prop) {
	pp := newParams(p)
	if b.propID = pp.take("prop-id"); b.propID == "" {
		b.propID = pp.take("x-prop-id")
	}
	var obj map[string]any // a map entry; gets contexts, pref, label, vCardParams
	switch p.Name {
	case "VERSION", "X-ABLABEL":
		return
	case "UID":
		b.card["uid"] = p.Text()
		b.whole("uid")
		return
	case "KIND", "X-ADDRESSBOOKSERVER-KIND":
		b.card["kind"] = strings.ToLower(p.Text())
		b.whole("kind")
		return
	case "PRODID":
		b.card["prodId"] = p.Text()
		b.whole("prodId")
		return
	case "REV":
		if t, ok := parseTimestamp(p.Value); ok {
			b.card["updated"] = t
			b.whole("updated")
			return
		}
	case "JSPROP", "X-JSPROP":
		ptr := p.Param("JSPTR")
		if ptr == "" {
			ptr = p.Param("X-JSPTR")
		}
		var v any
		if ptr != "" && json.Unmarshal([]byte(p.Text()), &v) == nil {
			b.jsprops = append(b.jsprops, jsprop{ptr, v})
			segs := strings.SplitN(ptr, "/", 3)
			b.at = loc{segs[0], ""}
			if _, ok := entryProps[segs[0]]; ok && len(segs) > 1 {
				b.at.id = segs[1]
			}
			return
		}
	case "FN":
		b.nameObj()["full"] = p.Text()
		return
	case "N":
		var comps []any
		for i, field := range splitStructured(p.Value) {
			kind := nKinds[min(i, len(nKinds)-1)]
			for _, v := range field {
				if v != "" && kind != "" {
					comps = append(comps, map[string]any{"@type": "NameComponent", "kind": kind, "value": v})
				}
			}
		}
		name := b.nameObj()
		if len(comps) > 0 {
			name["components"] = comps
		}
		return
	case "MEMBER", "X-ADDRESSBOOKSERVER-MEMBER":
		v := p.Text()
		if p.Name == "X-ADDRESSBOOKSERVER-MEMBER" {
			// Apple's version 3 cards have bare UIDs.
			v = strings.TrimPrefix(v, "urn:uuid:")
		}
		setKey(b.card, "members", v)
		b.whole("members")
		return
	case "CATEGORIES":
		for _, k := range ical.SplitList(p.Value) {
			if k = ical.UnescapeText(k); k != "" {
				setKey(b.card, "keywords", k)
			}
		}
		b.whole("keywords")
		return
	case "NICKNAME":
		vals := ical.SplitList(p.Value)
		for _, n := range vals {
			if n = ical.UnescapeText(n); n != "" {
				o := map[string]any{"@type": "Nickname", "name": n}
				pp.apply(o, b.label(p))
				b.entry("nicknames", "n", o)
			}
		}
		if len(vals) > 1 {
			b.whole("nicknames") // one line, several entries
		}
		return
	case "ORG":
		fields := splitStructured(p.Value)
		obj = map[string]any{}
		if len(fields) > 0 && len(fields[0]) > 0 && fields[0][0] != "" {
			obj["name"] = fields[0][0]
		}
		var units []any
		for _, f := range fields[min(1, len(fields)):] {
			for _, u := range f {
				if u != "" {
					units = append(units, map[string]any{"@type": "OrgUnit", "name": u})
				}
			}
		}
		if units != nil {
			obj["units"] = units
		}
		if len(obj) == 0 {
			break
		}
		obj["@type"] = "Organization"
		pp.apply(obj, b.label(p))
		b.entry("organizations", "o", obj)
		return
	case "TITLE", "ROLE":
		obj = map[string]any{"@type": "Title", "name": p.Text(), "kind": strings.ToLower(p.Name)}
		pp.apply(obj, b.label(p))
		b.entry("titles", "t", obj)
		return
	case "EMAIL":
		pp.dropTypes("internet", "x400")
		obj = map[string]any{"@type": "EmailAddress", "address": p.Text()}
		pp.apply(obj, b.label(p))
		b.entry("emails", "e", obj)
		return
	case "TEL":
		obj = map[string]any{"@type": "Phone", "number": p.Text()}
		for _, f := range pp.takeTypes(telFeatures) {
			setKey(obj, "features", f)
		}
		pp.apply(obj, b.label(p))
		b.entry("phones", "p", obj)
		return
	case "ADR":
		obj = b.address(p, pp)
		pp.apply(obj, b.label(p))
		b.entry("addresses", "a", obj)
		return
	case "GEO":
		geo := p.Text()
		if lat, lon, ok := strings.Cut(geo, ";"); ok && !strings.HasPrefix(geo, "geo:") {
			geo = "geo:" + lat + "," + lon // version 3: "lat;lon"
		}
		obj = map[string]any{"@type": "Address", "coordinates": geo}
		pp.apply(obj, b.label(p))
		b.entry("addresses", "a", obj)
		return
	case "TZ":
		if tz, ok := timeZone(p.Text()); ok {
			obj = map[string]any{"@type": "Address", "timeZone": tz}
			pp.apply(obj, b.label(p))
			b.entry("addresses", "a", obj)
			return
		}
	case "URL":
		b.uriEntry("links", "l", "Link", "", p, pp)
		return
	case "PHOTO", "LOGO", "SOUND":
		b.uriEntry("media", "m", "Media", strings.ToLower(p.Name), p, pp)
		return
	case "KEY":
		b.uriEntry("cryptoKeys", "k", "CryptoKey", "", p, pp)
		return
	case "CALURI", "FBURL":
		kind := "calendar"
		if p.Name == "FBURL" {
			kind = "freeBusy"
		}
		b.uriEntry("calendars", "c", "Calendar", kind, p, pp)
		return
	case "CALADRURI":
		b.uriEntry("schedulingAddresses", "s", "SchedulingAddress", "", p, pp)
		return
	case "SOURCE", "ORG-DIRECTORY":
		kind := "entry"
		if p.Name == "ORG-DIRECTORY" {
			kind = "directory"
		}
		b.uriEntry("directories", "d", "Directory", kind, p, pp)
		return
	case "IMPP", "X-SOCIALPROFILE", "SOCIALPROFILE":
		obj = map[string]any{"@type": "OnlineService"}
		v := p.Text()
		if strings.EqualFold(pp.value, "text") {
			obj["user"] = v
		} else if v != "" {
			obj["uri"] = v
		}
		service := pp.take("service-type")
		if service == "" {
			service = pp.take("x-service-type")
		}
		if service == "" && p.Name == "X-SOCIALPROFILE" {
			service = pp.takeService()
		}
		if service != "" {
			obj["service"] = service
		}
		if u := pp.take("x-user"); u != "" {
			obj["user"] = u
		} else if u := pp.take("username"); u != "" {
			obj["user"] = u
		}
		pp.apply(obj, b.label(p))
		b.entry("onlineServices", "s", obj)
		return
	case "BDAY", "ANNIVERSARY", "DEATHDATE":
		if d, ok := parseDate(p.Value); ok && !strings.EqualFold(pp.value, "text") {
			// Apple writes birthdays without a year as 1604-MM-DD.
			if y := pp.take("x-apple-omit-year"); y != "" && d["year"] != nil && strconv.Itoa(d["year"].(int)) == y {
				delete(d, "year")
			}
			kind := map[string]string{"BDAY": "birth", "ANNIVERSARY": "wedding", "DEATHDATE": "death"}[p.Name]
			obj = map[string]any{"@type": "Anniversary", "kind": kind, "date": d}
			pp.apply(obj, "")
			delete(obj, "contexts")
			b.entry("anniversaries", "k", obj)
			return
		}
	case "X-ABDATE":
		// Apple's version 3 anniversary: a date labeled Anniversary.
		if b.labels[strings.ToUpper(p.Group)] == "Anniversary" {
			if d, ok := parseDate(p.Value); ok {
				b.label(p)
				obj = map[string]any{"@type": "Anniversary", "kind": "wedding", "date": d}
				pp.apply(obj, "")
				delete(obj, "contexts")
				b.entry("anniversaries", "k", obj)
				return
			}
		}
	case "NOTE":
		obj = map[string]any{"@type": "Note", "note": p.Text()}
		pp.apply(obj, "")
		delete(obj, "contexts")
		b.entry("notes", "n", obj)
		return
	case "LANG":
		obj = map[string]any{"@type": "LanguagePref", "language": p.Text()}
		pp.apply(obj, b.label(p))
		b.entry("preferredLanguages", "l", obj)
		return
	case "RELATED":
		r := b.relation(p.Text())
		for _, t := range pp.takeTypes(nil) {
			setKey(r, "relation", t)
		}
		return
	case "X-ABRELATEDNAMES":
		// Apple's version 3 relation: a name labeled with the relation.
		if rel, ok := appleRelations[strings.ToLower(b.labels[strings.ToUpper(p.Group)])]; ok && p.Text() != "" {
			b.label(p)
			setKey(b.relation(p.Text()), "relation", rel)
			return
		}
	}
	b.unknownProp(p)
}

// relation returns the Relation object for a related card or name.
func (b *builder) relation(key string) map[string]any {
	rel, _ := b.card["relatedTo"].(map[string]any)
	if rel == nil {
		rel = map[string]any{}
		b.card["relatedTo"] = rel
	}
	r, _ := rel[key].(map[string]any)
	if r == nil {
		r = map[string]any{"@type": "Relation"}
		rel[key] = r
	}
	b.whole("relatedTo")
	return r
}

// appleRelations maps Apple's relation labels to JSContact relations.
var appleRelations = map[string]string{
	"father": "parent", "mother": "parent", "parent": "parent",
	"brother": "sibling", "sister": "sibling", "child": "child", "son": "child", "daughter": "child",
	"friend": "friend", "spouse": "spouse", "partner": "spouse", "assistant": "agent",
}

// setPointer sets the value at a JSON pointer-like path (without the
// leading "/"), creating objects on the way.
func setPointer(card map[string]any, ptr string, v any) {
	segs := strings.Split(ptr, "/")
	m := card
	for _, s := range segs[:len(segs)-1] {
		s = unescapePointer(s)
		next, ok := m[s].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[s] = next
		}
		m = next
	}
	m[unescapePointer(segs[len(segs)-1])] = v
}

func unescapePointer(s string) string {
	return strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
}

// N components in order (RFC 6350 §6.2.2, RFC 9554 §2.2).
var nKinds = []string{"surname", "given", "given2", "title", "credential", "surname2", "generation", ""}

// ADR components in order. The extended address is deprecated; RFC 9555
// maps it to "apartment".
var adrKinds = []string{"postOfficeBox", "apartment", "name", "locality", "region", "postcode", "country"}

func (b *builder) address(p *ical.Prop, pp *params) map[string]any {
	obj := map[string]any{"@type": "Address"}
	var comps []any
	for i, field := range splitStructured(p.Value) {
		if i >= len(adrKinds) {
			break
		}
		for _, v := range field {
			if v != "" {
				comps = append(comps, map[string]any{"@type": "AddressComponent", "kind": adrKinds[i], "value": v})
			}
		}
	}
	if comps != nil {
		obj["components"] = comps
	}
	if v := pp.take("label"); v != "" {
		obj["full"] = v
	}
	if v := pp.take("cc"); v != "" {
		obj["countryCode"] = v
	}
	if v := pp.take("geo"); v != "" {
		obj["coordinates"] = v
	}
	if v := pp.take("tz"); v != "" {
		obj["timeZone"] = v
	}
	return obj
}

// uriEntry adds a Link-like entry (uri, optional kind and mediaType).
// Version 3 inline binary values (ENCODING=b) become data: URIs.
func (b *builder) uriEntry(prop, prefix, typ, kind string, p *ical.Prop, pp *params) {
	obj := map[string]any{"@type": typ, "uri": p.Text()}
	if kind != "" {
		obj["kind"] = kind
	}
	mt := pp.take("mediatype")
	if enc := pp.take("encoding"); strings.EqualFold(enc, "b") || strings.EqualFold(enc, "base64") {
		if mt == "" {
			mt = mediaTypeOf(pp.takeFirstType(), p.Name)
		}
		obj["uri"] = "data:" + mt + ";base64," + strings.Join(strings.Fields(p.Value), "")
	}
	if mt != "" {
		obj["mediaType"] = mt
	}
	pp.apply(obj, b.label(p))
	b.entry(prop, prefix, obj)
}

// mediaTypeOf guesses a media type from a version 3 TYPE like JPEG.
func mediaTypeOf(t, prop string) string {
	if t == "" {
		return "application/octet-stream"
	}
	if strings.Contains(t, "/") {
		return t
	}
	major := "image"
	if prop == "SOUND" {
		major = "audio"
	}
	return major + "/" + strings.ToLower(t)
}

// unknownProp keeps a property as a jCard array in vCardProps.
func (b *builder) unknownProp(p *ical.Prop) {
	params := map[string]any{}
	if p.Group != "" {
		params["group"] = strings.ToLower(p.Group)
	}
	for _, prm := range p.Params {
		params[strings.ToLower(prm.Name)] = paramValue(prm.Values)
	}
	// The value stays raw (still escaped) unless it is declared text, so
	// structured values like CLIENTPIDMAP's survive a round trip.
	typ, val := "unknown", p.Value
	if v := p.Param("VALUE"); v != "" {
		typ = strings.ToLower(v)
		delete(params, "value")
		if typ == "text" {
			val = p.Text()
		}
	}
	b.unknown = append(b.unknown, []any{strings.ToLower(p.Name), params, typ, val})
	b.whole("vCardProps")
}

// label returns the Apple label of p's group and marks it as used.
func (b *builder) label(p *ical.Prop) string {
	g := strings.ToUpper(p.Group)
	l, ok := b.labels[g]
	if ok {
		b.labelUsed[g] = true
	}
	return l
}

// appleLabel decodes Apple's built-in labels, "_$!<Work>!$_" -> "Work".
func appleLabel(s string) string {
	if v, ok := strings.CutPrefix(s, "_$!<"); ok {
		if v, ok := strings.CutSuffix(v, ">!$_"); ok {
			return v
		}
	}
	return s
}

func setKey(m map[string]any, prop, key string) {
	set, _ := m[prop].(map[string]any)
	if set == nil {
		set = map[string]any{}
		m[prop] = set
	}
	set[key] = true
}

func paramValue(vals []string) any {
	if len(vals) == 1 {
		return vals[0]
	}
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return out
}

// splitStructured splits a structured value (N, ADR, ORG) at unescaped
// ";" into fields and each field at unescaped "," into unescaped values.
func splitStructured(raw string) [][]string {
	var fields [][]string
	var cur []string
	var sb strings.Builder
	flushValue := func() {
		cur = append(cur, sb.String())
		sb.Reset()
	}
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == '\\' && i+1 < len(raw):
			i++
			switch raw[i] {
			case 'n', 'N':
				sb.WriteByte('\n')
			default:
				sb.WriteByte(raw[i])
			}
		case c == ',':
			flushValue()
		case c == ';':
			flushValue()
			fields = append(fields, cur)
			cur = nil
		default:
			sb.WriteByte(c)
		}
	}
	flushValue()
	return append(fields, cur)
}

// parseTimestamp reads a vCard timestamp (REV, BDAY with time) as a JMAP
// UTCDate.
func parseTimestamp(v string) (string, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"20060102T150405Z0700", "2006-01-02T15:04:05Z07:00", "20060102T1504Z0700", "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05Z"), true
		}
	}
	return "", false
}

// timeZone maps a TZ value to a time zone name: names pass through, and
// whole-hour UTC offsets become Etc/GMT zones (whose sign is inverted).
func timeZone(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	if v[0] != '+' && v[0] != '-' {
		return v, !strings.ContainsAny(v, " ;")
	}
	digits := strings.ReplaceAll(v[1:], ":", "")
	if len(digits) != 4 || digits[2:] != "00" {
		return "", false
	}
	h, err := strconv.Atoi(digits[:2])
	if err != nil || h > 14 {
		return "", false
	}
	if h == 0 {
		return "Etc/UTC", true
	}
	sign := "-"
	if v[0] == '-' {
		sign = "+"
	}
	return "Etc/GMT" + sign + strconv.Itoa(h), true
}

// parseDate reads a vCard date (full, partial like --0415 or 1996, or a
// timestamp) as a JSContact PartialDate or Timestamp.
func parseDate(v string) (map[string]any, bool) {
	v = strings.TrimSpace(v)
	if t, ok := parseTimestamp(v); ok {
		return map[string]any{"@type": "Timestamp", "utc": t}, true
	}
	if i := strings.IndexByte(v, 'T'); i >= 0 {
		v = v[:i] // date with a local time: keep the date
	}
	compact := strings.ReplaceAll(v, "-", "")
	d := map[string]any{"@type": "PartialDate"}
	num := func(s string) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= 0
	}
	switch {
	case strings.HasPrefix(v, "--") && len(compact) == 4: // --MMDD, --MM-DD
		m, ok1 := num(compact[:2])
		dd, ok2 := num(compact[2:])
		if !ok1 || !ok2 {
			return nil, false
		}
		d["month"], d["day"] = m, dd
	case strings.HasPrefix(v, "---") && len(compact) == 2: // ---DD
		dd, ok := num(compact)
		if !ok {
			return nil, false
		}
		d["day"] = dd
	case strings.HasPrefix(v, "--") && len(compact) == 2: // --MM
		m, ok := num(compact)
		if !ok {
			return nil, false
		}
		d["month"] = m
	case len(compact) == 8: // YYYYMMDD, YYYY-MM-DD
		y, ok1 := num(compact[:4])
		m, ok2 := num(compact[4:6])
		dd, ok3 := num(compact[6:])
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		d["year"], d["month"], d["day"] = y, m, dd
	case len(compact) == 6 && strings.Contains(v, "-"): // YYYY-MM
		y, ok1 := num(compact[:4])
		m, ok2 := num(compact[4:])
		if !ok1 || !ok2 {
			return nil, false
		}
		d["year"], d["month"] = y, m
	case len(compact) == 4: // YYYY
		y, ok := num(compact)
		if !ok {
			return nil, false
		}
		d["year"] = y
	default:
		return nil, false
	}
	if m, ok := d["month"].(int); ok && (m < 1 || m > 12) {
		return nil, false
	}
	if dd, ok := d["day"].(int); ok && (dd < 1 || dd > 31) {
		return nil, false
	}
	return d, true
}
