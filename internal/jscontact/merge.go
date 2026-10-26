package jscontact

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// ProdID is written into vCards that Merge creates.
const ProdID = "-//ccdav//JMAP//EN"

// serverProps are card properties Merge never writes: the JMAP layer owns
// them (id, addressBookIds, created) or Merge sets them itself (updated,
// as REV).
var serverProps = map[string]bool{"@type": true, "version": true, "id": true, "addressBookIds": true, "created": true, "updated": true}

// Merge writes card into the vCard prev and returns the result. Only
// what differs from prev's own conversion is rewritten: lines of
// unchanged properties and entries stay as they are (byte for byte, see
// ical), so whatever DAV clients stored survives JMAP edits. changed is
// false (and prev returned) if there is nothing to write.
//
// A nil prev creates a new vCard of the given version ("3.0" or "4.0").
// What a version can't express is kept as JSPROP lines (X-JSPROP in
// version 3), which the conversion reads back. Entry ids are kept as
// PROP-ID parameters (X-PROP-ID in version 3).
func Merge(prev *ical.Component, card map[string]any, version string) (out *ical.Component, changed bool, err error) {
	if paths := Validate(card); len(paths) > 0 {
		return nil, false, &InvalidError{Paths: paths}
	}
	if prev == nil {
		prev = &ical.Component{Name: "VCARD", Props: []*ical.Prop{{Name: "VERSION", Value: version}}}
		if _, ok := card["prodId"]; !ok {
			card = maps.Clone(card)
			card["prodId"] = ProdID
		}
	}
	old, origins, err := convert(prev)
	if err != nil {
		return nil, false, err
	}
	// Compare as JSON: clients send numbers as floats, conversion makes ints.
	oldN, newN := normalize(old), normalize(card)
	var keys []string
	for k := range union(oldN, newN) {
		if !serverProps[k] && !same(oldN[k], newN[k]) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return prev, false, nil
	}
	// New lines go to the end in this order: identity first, then by name.
	first := []string{"uid", "prodId", "kind", "name"}
	slices.SortFunc(keys, func(a, b string) int {
		ia, ib := slices.Index(first, a), slices.Index(first, b)
		switch {
		case ia >= 0 && ib >= 0:
			return ia - ib
		case ia >= 0:
			return -1
		case ib >= 0:
			return 1
		}
		return strings.Compare(a, b)
	})
	m := &merger{v3: strings.TrimSpace(prev.Value("VERSION")) == "3.0", origins: origins,
		props: slices.Clone(prev.Props), drop: make([]bool, len(prev.Props)), before: map[int][]*ical.Prop{}}
	m.nextGroup = maxGroup(prev.Props) + 1
	for _, k := range keys {
		if err := m.property(k, oldN[k], newN[k]); err != nil {
			return nil, false, err
		}
	}
	m.touch("updated", nil, []*ical.Prop{{Name: "REV", Value: time.Now().UTC().Format("20060102T150405Z")}})
	out = &ical.Component{Name: "VCARD", Children: prev.Children}
	for i, p := range m.props {
		out.Props = append(out.Props, m.before[i]...)
		if !m.drop[i] {
			out.Props = append(out.Props, p)
		}
	}
	out.Props = append(out.Props, m.tail...)
	if out.Prop("FN") == nil { // required in both versions
		out.Props = append(out.Props, &ical.Prop{Name: "FN", Value: ical.EscapeText(fallbackFN(newN))})
	}
	if m.v3 && out.Prop("N") == nil { // required in version 3
		out.Props = append(out.Props, &ical.Prop{Name: "N", Value: ";;;;"})
	}
	return out, true, nil
}

// InvalidError lists the card properties (as paths) that are invalid.
type InvalidError struct{ Paths []string }

func (e *InvalidError) Error() string {
	return "jscontact: invalid properties " + strings.Join(e.Paths, ", ")
}

func normalize(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// same compares normalized values, ignoring "@type": it carries nothing
// the vCard keeps, and clients may leave it out.
func same(a, b any) bool { return reflect.DeepEqual(untyped(a), untyped(b)) }

func untyped(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if k != "@type" {
				out[k] = untyped(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = untyped(e)
		}
		return out
	}
	return v
}

func union(a, b map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func maxGroup(props []*ical.Prop) int {
	n := 0
	for _, p := range props {
		if v, ok := strings.CutPrefix(strings.ToLower(p.Group), "item"); ok {
			if i, err := strconv.Atoi(v); err == nil {
				n = max(n, i)
			}
		}
	}
	return n
}

type merger struct {
	v3        bool
	origins   []loc
	props     []*ical.Prop // prev's lines; replaced (not changed) when edited
	drop      []bool
	before    map[int][]*ical.Prop // new lines to insert before props[i]
	tail      []*ical.Prop         // new lines at the end
	nextGroup int
}

// touch replaces the lines that belong to loc{prop, id} (all of prop for
// id nil) with lines, at the position of the first of them.
func (m *merger) touch(prop string, id *string, lines []*ical.Prop) {
	at := -1
	for i, o := range m.origins {
		if o.prop == prop && (id == nil || o.id == *id) && !m.drop[i] {
			m.drop[i] = true
			if at < 0 {
				at = i
			}
		}
	}
	if at < 0 {
		m.tail = append(m.tail, lines...)
	} else {
		m.before[at] = append(m.before[at], lines...)
	}
}

// group returns the vCard group of the main line of an entry, if it had
// one, so associated lines (Apple's X-ABADR) stay attached.
func (m *merger) group(prop, id string) string {
	for i, o := range m.origins {
		if o.prop == prop && o.id == id && m.props[i].Name != "X-ABLABEL" && !strings.HasSuffix(m.props[i].Name, "JSPROP") {
			return m.props[i].Group
		}
	}
	return ""
}

func (m *merger) newGroup() string {
	g := "item" + strconv.Itoa(m.nextGroup)
	m.nextGroup++
	return g
}

// property writes one changed top-level property.
func (m *merger) property(k string, oldV, newV any) error {
	if _, isEntries := entryProps[k]; isEntries && !m.hasWholeLines(k) {
		oldM, _ := oldV.(map[string]any)
		newM, _ := newV.(map[string]any)
		m.pinIDs(k)
		for _, id := range slices.Sorted(maps.Keys(oldM)) {
			if _, ok := newM[id]; !ok {
				m.touch(k, &id, nil)
			}
		}
		for _, id := range slices.Sorted(maps.Keys(newM)) {
			if same(oldM[id], newM[id]) {
				continue
			}
			lines, err := m.entry(k, id, newM[id].(map[string]any), m.group(k, id))
			if err != nil {
				return err
			}
			m.touch(k, &id, lines)
		}
		return nil
	}
	lines, err := m.whole(k, newV)
	if err != nil {
		return err
	}
	m.touch(k, nil, lines)
	return nil
}

// hasWholeLines reports whether a line of prev holds several entries of
// k at once (NICKNAME:a,b); then k is rewritten as a whole.
func (m *merger) hasWholeLines(k string) bool {
	return slices.Contains(m.origins, loc{k, ""})
}

// pinIDs writes the current entry ids of k as PROP-ID parameters, so
// they survive removals before them.
func (m *merger) pinIDs(k string) {
	name := m.propIDParam()
	for i, o := range m.origins {
		p := m.props[i]
		if o.prop != k || o.id == "" || m.drop[i] || p.Name == "X-ABLABEL" || strings.HasSuffix(p.Name, "JSPROP") || p.Param(name) == o.id {
			continue
		}
		cp := *p
		cp.Params = slices.Clone(p.Params)
		cp.SetParam(name, o.id)
		m.props[i] = &cp
	}
}

func (m *merger) propIDParam() string {
	if m.v3 {
		return "X-PROP-ID"
	}
	return "PROP-ID"
}

// jsprop is a line keeping a value at ptr that the vCard can't express.
func (m *merger) jsprop(ptr string, v any) *ical.Prop {
	b, _ := json.Marshal(v)
	name, param := "JSPROP", "JSPTR"
	if m.v3 {
		name, param = "X-JSPROP", "X-JSPTR"
	}
	return &ical.Prop{Name: name, Params: []ical.Param{{Name: param, Values: []string{ptr}}}, Value: ical.EscapeText(string(b))}
}

// rest turns the fields of obj not in used into JSPROP lines.
func (m *merger) rest(ptr string, obj map[string]any, used ...string) []*ical.Prop {
	var out []*ical.Prop
	for _, f := range slices.Sorted(maps.Keys(obj)) {
		if f != "@type" && !slices.Contains(used, f) {
			out = append(out, m.jsprop(ptr+"/"+escapePointer(f), obj[f]))
		}
	}
	return out
}

func escapePointer(s string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(s) }

// common fields every entry line can carry: contexts, pref, label,
// vCardParams (and its id, as PROP-ID).
var common = []string{"contexts", "pref", "label", "vCardParams"}

// entry renders one entry of a map property: its main line, the Apple
// label line if it has a label, and JSPROP lines for the rest.
func (m *merger) entry(k, id string, obj map[string]any, group string) ([]*ical.Prop, error) {
	ptr := k + "/" + escapePointer(id)
	v3 := m.v3
	var p *ical.Prop
	var used []string
	var types []string
	switch k {
	case "nicknames":
		p, used = &ical.Prop{Name: "NICKNAME", Value: ical.EscapeText(str(obj["name"]))}, []string{"name"}
	case "organizations":
		vals := []string{escStruct(str(obj["name"]))}
		units, _ := obj["units"].([]any)
		simple := true
		for _, u := range units {
			um, _ := u.(map[string]any)
			vals = append(vals, escStruct(str(um["name"])))
			if len(um) > 2 || len(um) == 2 && um["@type"] == nil {
				simple = false
			}
		}
		p, used = &ical.Prop{Name: "ORG", Value: strings.Join(vals, ";")}, []string{"name"}
		if simple {
			used = append(used, "units")
		}
	case "titles":
		name := "TITLE"
		if str(obj["kind"]) == "role" {
			name = "ROLE"
		}
		p, used = &ical.Prop{Name: name, Value: ical.EscapeText(str(obj["name"]))}, []string{"name", "kind"}
	case "emails":
		p, used = &ical.Prop{Name: "EMAIL", Value: ical.EscapeText(str(obj["address"]))}, []string{"address"}
		if v3 {
			types = append(types, "INTERNET")
		}
	case "phones":
		p, used = &ical.Prop{Name: "TEL", Value: ical.EscapeText(str(obj["number"]))}, []string{"number", "features"}
		for _, f := range keys(obj["features"]) {
			t, ok := featureTypes[f]
			if !ok {
				used = slices.DeleteFunc(used, func(s string) bool { return s == "features" })
				continue
			}
			types = append(types, t)
		}
	case "addresses":
		p, used = m.address(obj)
	case "links":
		p, used = &ical.Prop{Name: "URL", Value: str(obj["uri"])}, []string{"uri"}
	case "media":
		name := map[string]string{"photo": "PHOTO", "logo": "LOGO", "sound": "SOUND"}[str(obj["kind"])]
		if name == "" {
			return []*ical.Prop{m.jsprop(ptr, obj)}, nil
		}
		p, used = m.binary(name, obj)
		used = append(used, "kind")
	case "cryptoKeys":
		p, used = m.binary("KEY", obj)
	case "calendars":
		name := "CALURI"
		if str(obj["kind"]) == "freeBusy" {
			name = "FBURL"
		}
		p, used = &ical.Prop{Name: name, Value: str(obj["uri"])}, []string{"uri", "kind"}
		if mt := str(obj["mediaType"]); mt != "" && !v3 {
			p.Params = append(p.Params, ical.Param{Name: "MEDIATYPE", Values: []string{mt}})
			used = append(used, "mediaType")
		}
	case "schedulingAddresses":
		p, used = &ical.Prop{Name: "CALADRURI", Value: str(obj["uri"])}, []string{"uri"}
	case "directories":
		if str(obj["kind"]) == "directory" && v3 {
			return []*ical.Prop{m.jsprop(ptr, obj)}, nil
		}
		name := "SOURCE"
		if str(obj["kind"]) == "directory" {
			name = "ORG-DIRECTORY"
		}
		p, used = &ical.Prop{Name: name, Value: str(obj["uri"])}, []string{"uri", "kind"}
	case "onlineServices":
		p, used = m.online(obj)
	case "anniversaries":
		lines, ok := m.anniversary(ptr, obj, group)
		if ok {
			return lines, nil
		}
		return []*ical.Prop{m.jsprop(ptr, obj)}, nil
	case "notes":
		p, used = &ical.Prop{Name: "NOTE", Value: ical.EscapeText(str(obj["note"]))}, []string{"note"}
	case "preferredLanguages":
		if v3 {
			return []*ical.Prop{m.jsprop(ptr, obj)}, nil
		}
		p, used = &ical.Prop{Name: "LANG", Value: str(obj["language"])}, []string{"language"}
	default:
		return nil, fmt.Errorf("jscontact: no mapping for %s", k)
	}
	used = append(used, common...)
	lines := []*ical.Prop{p}
	p.Group = group
	m.params(p, obj, types)
	p.SetParam(m.propIDParam(), id)
	if label := str(obj["label"]); label != "" {
		if p.Group == "" {
			p.Group = m.newGroup()
		}
		lines = append(lines, &ical.Prop{Group: p.Group, Name: "X-ABLABEL", Value: ical.EscapeText(encodeAppleLabel(label))})
	}
	if ctx := keys(obj["contexts"]); slices.ContainsFunc(ctx, func(c string) bool { return c != "private" && c != "work" }) {
		used = slices.DeleteFunc(used, func(s string) bool { return s == "contexts" })
	}
	return append(lines, m.rest(ptr, obj, used...)...), nil
}

// params adds TYPE (types, contexts), PREF and vCardParams to p.
func (m *merger) params(p *ical.Prop, obj map[string]any, types []string) {
	for _, c := range keys(obj["contexts"]) {
		switch c {
		case "private":
			types = append(types, "HOME")
		case "work":
			types = append(types, "WORK")
		}
	}
	extra := map[string]any{}
	if vp, ok := obj["vCardParams"].(map[string]any); ok {
		extra = vp
	}
	for _, t := range paramStrings(extra["type"]) {
		types = append(types, strings.ToUpper(t))
	}
	if pref, ok := num(obj["pref"]); ok {
		switch {
		case !m.v3:
			p.Params = append(p.Params, ical.Param{Name: "PREF", Values: []string{strconv.Itoa(pref)}})
		case pref == 1:
			types = append(types, "PREF")
		default:
			p.Params = append(p.Params, ical.Param{Name: "X-PREF", Values: []string{strconv.Itoa(pref)}})
		}
	}
	if !m.v3 {
		for i, t := range types {
			types[i] = strings.ToLower(t)
		}
	}
	if len(types) > 0 {
		p.Params = append([]ical.Param{{Name: "TYPE", Values: types}}, p.Params...)
	}
	for _, name := range slices.Sorted(maps.Keys(extra)) {
		if name != "type" {
			p.Params = append(p.Params, ical.Param{Name: strings.ToUpper(name), Values: paramStrings(extra[name])})
		}
	}
}

var featureTypes = map[string]string{
	"mobile": "CELL", "voice": "VOICE", "fax": "FAX", "pager": "PAGER", "text": "TEXT",
	"video": "VIDEO", "textphone": "TEXTPHONE", "main-number": "MAIN-NUMBER",
}

// adrFields are the ADR fields of address component kinds; other kinds
// go into the street field.
var adrFields = map[string]int{"postOfficeBox": 0, "apartment": 1, "locality": 3, "region": 4, "postcode": 5, "country": 6}

func (m *merger) address(obj map[string]any) (*ical.Prop, []string) {
	comps, _ := obj["components"].([]any)
	only := func(f string) bool {
		for k := range obj {
			if k != f && k != "@type" && !slices.Contains(common, k) {
				return false
			}
		}
		return true
	}
	if c := str(obj["coordinates"]); c != "" && only("coordinates") {
		v := c
		if m.v3 {
			v = strings.Replace(strings.TrimPrefix(c, "geo:"), ",", ";", 1)
		}
		return &ical.Prop{Name: "GEO", Value: v}, []string{"coordinates"}
	}
	if tz := str(obj["timeZone"]); tz != "" && only("timeZone") {
		return &ical.Prop{Name: "TZ", Value: ical.EscapeText(tz)}, []string{"timeZone"}
	}
	fields := make([][]string, 7)
	simple, last := true, 0
	for _, c := range comps {
		cm, _ := c.(map[string]any)
		kind, val := str(cm["kind"]), str(cm["value"])
		i, ok := adrFields[kind]
		if !ok {
			i = 2
			if kind != "name" {
				simple = false // the exact kind needs a JSPROP
			}
		}
		if i < last {
			simple = false // ADR can't keep this order
		}
		last = i
		if len(cm) > 3 || len(cm) == 3 && cm["@type"] == nil {
			simple = false
		}
		fields[i] = append(fields[i], val)
	}
	vals := make([]string, 7)
	for i, f := range fields {
		var esc []string
		for _, v := range f {
			esc = append(esc, escStruct(v))
		}
		vals[i] = strings.Join(esc, ",")
	}
	p := &ical.Prop{Name: "ADR", Value: strings.Join(vals, ";")}
	used := []string{"full", "countryCode", "coordinates", "timeZone"}
	if simple {
		used = append(used, "components")
	}
	for _, f := range []struct{ field, param string }{{"full", "LABEL"}, {"countryCode", "CC"}, {"coordinates", "GEO"}, {"timeZone", "TZ"}} {
		if v := str(obj[f.field]); v != "" {
			p.Params = append(p.Params, ical.Param{Name: f.param, Values: []string{v}})
		}
	}
	return p, used
}

// binary renders a uri-valued line that may hold inline data: as a data:
// URI in version 4, as ENCODING=b in version 3.
func (m *merger) binary(name string, obj map[string]any) (*ical.Prop, []string) {
	uri, mt := str(obj["uri"]), str(obj["mediaType"])
	p := &ical.Prop{Name: name, Value: uri}
	used := []string{"uri", "mediaType"}
	if m.v3 {
		if head, data, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ";base64,"); ok && strings.HasPrefix(uri, "data:") {
			if mt == "" {
				mt = head
			}
			_, sub, _ := strings.Cut(mt, "/")
			p.Value = data
			p.Params = append(p.Params, ical.Param{Name: "ENCODING", Values: []string{"b"}}, ical.Param{Name: "TYPE", Values: []string{strings.ToUpper(sub)}})
			return p, used
		}
		if mt != "" {
			used = used[:1] // version 3 has no media type for links
		}
		return p, used
	}
	if mt != "" {
		p.Params = append(p.Params, ical.Param{Name: "MEDIATYPE", Values: []string{mt}})
	}
	return p, used
}

func (m *merger) online(obj map[string]any) (*ical.Prop, []string) {
	uri, user, service := str(obj["uri"]), str(obj["user"]), str(obj["service"])
	used := []string{"uri", "user", "service"}
	social := uri == "" || strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://")
	p := &ical.Prop{Name: "IMPP", Value: uri}
	switch {
	case social && m.v3:
		p.Name = "X-SOCIALPROFILE"
		// Apple's form is type=twitter; TYPE values lose their case, so
		// other names go into X-SERVICE-TYPE.
		if service != "" && service == strings.ToLower(service) {
			p.Params = append(p.Params, ical.Param{Name: "TYPE", Values: []string{service}})
			service = ""
		}
	case social:
		p.Name = "SOCIALPROFILE"
		if uri == "" {
			p.Value, user = ical.EscapeText(user), ""
			p.Params = append(p.Params, ical.Param{Name: "VALUE", Values: []string{"text"}})
		}
	}
	if service != "" {
		name := "SERVICE-TYPE"
		if m.v3 {
			name = "X-SERVICE-TYPE"
		}
		p.Params = append(p.Params, ical.Param{Name: name, Values: []string{service}})
	}
	if user != "" {
		name := "USERNAME"
		if m.v3 {
			name = "X-USER"
		}
		p.Params = append(p.Params, ical.Param{Name: name, Values: []string{user}})
	}
	return p, used
}

// anniversary renders BDAY, DEATHDATE and ANNIVERSARY, or in version 3
// Apple's labeled X-ABDATE; false if the version can't hold it.
func (m *merger) anniversary(ptr string, obj map[string]any, group string) ([]*ical.Prop, bool) {
	date, _ := obj["date"].(map[string]any)
	val, params, ok := m.date(date)
	if !ok {
		return nil, false
	}
	var p *ical.Prop
	var lines []*ical.Prop
	switch kind := str(obj["kind"]); {
	case kind == "birth":
		p = &ical.Prop{Name: "BDAY"}
	case kind == "death" && !m.v3:
		p = &ical.Prop{Name: "DEATHDATE"}
	case kind == "wedding" && !m.v3:
		p = &ical.Prop{Name: "ANNIVERSARY"}
	case kind == "wedding":
		if group == "" {
			group = m.newGroup()
		}
		p = &ical.Prop{Name: "X-ABDATE", Group: group}
		lines = append(lines, &ical.Prop{Group: group, Name: "X-ABLABEL", Value: ical.EscapeText(encodeAppleLabel("Anniversary"))})
	default:
		return nil, false
	}
	p.Value, p.Params = val, params
	m.params(p, obj, nil)
	p.SetParam(m.propIDParam(), ptr[strings.LastIndexByte(ptr, '/')+1:])
	return append(append([]*ical.Prop{p}, lines...), m.rest(ptr, obj, "date", "kind", "pref", "vCardParams")...), true
}

// date renders a PartialDate or Timestamp.
func (m *merger) date(d map[string]any) (string, []ical.Param, bool) {
	if str(d["@type"]) == "Timestamp" {
		t, err := time.Parse(time.RFC3339, str(d["utc"]))
		if err != nil {
			return "", nil, false
		}
		if m.v3 {
			return t.UTC().Format("2006-01-02T15:04:05Z"), nil, true
		}
		return t.UTC().Format("20060102T150405Z"), nil, true
	}
	if _, ok := d["calendarScale"]; ok {
		return "", nil, false
	}
	y, hasY := num(d["year"])
	mo, hasM := num(d["month"])
	day, hasD := num(d["day"])
	switch {
	case hasY && hasM && hasD && m.v3:
		return fmt.Sprintf("%04d-%02d-%02d", y, mo, day), nil, true
	case hasY && hasM && hasD:
		return fmt.Sprintf("%04d%02d%02d", y, mo, day), nil, true
	case hasM && hasD && !hasY && m.v3:
		// Apple's way to say "no year".
		return fmt.Sprintf("1604-%02d-%02d", mo, day), []ical.Param{{Name: "X-APPLE-OMIT-YEAR", Values: []string{"1604"}}}, true
	case m.v3:
		return "", nil, false
	case hasM && hasD:
		return fmt.Sprintf("--%02d%02d", mo, day), nil, true
	case hasY && hasM:
		return fmt.Sprintf("%04d-%02d", y, mo), nil, true
	case hasY:
		return fmt.Sprintf("%04d", y), nil, true
	case hasD:
		return fmt.Sprintf("---%02d", day), nil, true
	}
	return "", nil, false
}

// whole renders a property that is written as a whole.
func (m *merger) whole(k string, v any) ([]*ical.Prop, error) {
	if v == nil {
		return nil, nil
	}
	switch k {
	case "name":
		return m.name(v.(map[string]any)), nil
	case "uid":
		return []*ical.Prop{{Name: "UID", Value: ical.EscapeText(str(v))}}, nil
	case "prodId":
		return []*ical.Prop{{Name: "PRODID", Value: ical.EscapeText(str(v))}}, nil
	case "kind":
		switch {
		case !m.v3:
			return []*ical.Prop{{Name: "KIND", Value: str(v)}}, nil
		case v == "group":
			return []*ical.Prop{{Name: "X-ADDRESSBOOKSERVER-KIND", Value: "group"}}, nil
		case v == "individual":
			return nil, nil
		}
	case "members":
		var out []*ical.Prop
		for _, uid := range keys(v) {
			if m.v3 {
				// Apple's form; reading strips exactly this prefix again.
				out = append(out, &ical.Prop{Name: "X-ADDRESSBOOKSERVER-MEMBER", Value: "urn:uuid:" + uid})
			} else {
				out = append(out, &ical.Prop{Name: "MEMBER", Value: uid})
			}
		}
		return out, nil
	case "keywords":
		var esc []string
		for _, k := range keys(v) {
			esc = append(esc, ical.EscapeText(k))
		}
		if len(esc) == 0 {
			return nil, nil
		}
		return []*ical.Prop{{Name: "CATEGORIES", Value: strings.Join(esc, ",")}}, nil
	case "relatedTo":
		if lines, ok := m.related(v.(map[string]any)); ok {
			return lines, nil
		}
	case "vCardProps":
		return m.vCardProps(v.([]any)), nil
	case "nicknames":
		// Lines with several nicknames are rewritten as one line each.
		var out []*ical.Prop
		nm, _ := v.(map[string]any)
		for _, id := range slices.Sorted(maps.Keys(nm)) {
			lines, err := m.entry(k, id, nm[id].(map[string]any), "")
			if err != nil {
				return nil, err
			}
			out = append(out, lines...)
		}
		return out, nil
	}
	return []*ical.Prop{m.jsprop(escapePointer(k), v)}, nil
}

// nKinds without the empty catch-all, by N field.
var nFields = map[string]int{"surname": 0, "given": 1, "given2": 2, "title": 3, "credential": 4, "surname2": 5, "generation": 6}

func (m *merger) name(n map[string]any) []*ical.Prop {
	full := str(n["full"])
	comps, _ := n["components"].([]any)
	nf := 5
	if !m.v3 {
		nf = 7
	}
	fields := make([][]string, nf)
	simple, last := true, 0
	for _, c := range comps {
		cm, _ := c.(map[string]any)
		i, ok := nFields[str(cm["kind"])]
		if !ok || i >= nf || len(cm) > 3 || len(cm) == 3 && cm["@type"] == nil {
			simple = false
			continue
		}
		if i < last {
			simple = false // N can't keep this order
		}
		last = i
		fields[i] = append(fields[i], str(cm["value"]))
	}
	vals := make([]string, nf)
	for i, f := range fields {
		var esc []string
		for _, v := range f {
			esc = append(esc, escStruct(v))
		}
		vals[i] = strings.Join(esc, ",")
	}
	if !m.v3 && vals[5] == "" && vals[6] == "" {
		vals = vals[:5] // RFC 6350 N, unless RFC 9554's fields are used
	}
	if full == "" {
		full = fallbackFN(map[string]any{"name": n})
	}
	fn := &ical.Prop{Name: "FN", Value: ical.EscapeText(full)}
	// Keep FN's parameters (LANGUAGE, ...).
	for i, o := range m.origins {
		if o.prop == "name" && m.props[i].Name == "FN" {
			fn.Params = slices.Clone(m.props[i].Params)
			break
		}
	}
	out := []*ical.Prop{fn, {Name: "N", Value: strings.Join(vals, ";")}}
	used := []string{"full"}
	if simple {
		used = append(used, "components")
	}
	return append(out, m.rest("name", n, used...)...)
}

// fallbackFN derives a formatted name when the card has none.
func fallbackFN(card map[string]any) string {
	n, _ := card["name"].(map[string]any)
	if s := str(n["full"]); s != "" {
		return s
	}
	var parts []string
	comps, _ := n["components"].([]any)
	for _, kind := range []string{"title", "given", "given2", "surname", "credential"} {
		for _, c := range comps {
			if cm, _ := c.(map[string]any); str(cm["kind"]) == kind && str(cm["value"]) != "" {
				parts = append(parts, str(cm["value"]))
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	if orgs, ok := card["organizations"].(map[string]any); ok {
		for _, id := range slices.Sorted(maps.Keys(orgs)) {
			if o, _ := orgs[id].(map[string]any); str(o["name"]) != "" {
				return str(o["name"])
			}
		}
	}
	return ""
}

// related renders relatedTo: RELATED in version 4; in version 3 Apple's
// labeled X-ABRELATEDNAMES if every relation has an Apple label.
func (m *merger) related(rel map[string]any) ([]*ical.Prop, bool) {
	var out []*ical.Prop
	for _, key := range slices.Sorted(maps.Keys(rel)) {
		r, _ := rel[key].(map[string]any)
		kinds := keys(r["relation"])
		if len(r) > 2 || len(r) == 2 && r["@type"] == nil && r["relation"] == nil {
			return nil, false
		}
		if !m.v3 {
			p := &ical.Prop{Name: "RELATED", Value: key}
			if len(kinds) > 0 {
				p.Params = append(p.Params, ical.Param{Name: "TYPE", Values: kinds})
			}
			if !strings.Contains(key, ":") {
				p.Value = ical.EscapeText(key)
				p.Params = append(p.Params, ical.Param{Name: "VALUE", Values: []string{"text"}})
			}
			out = append(out, p)
			continue
		}
		label, ok := appleRelationLabel[strings.Join(kinds, ",")]
		if !ok {
			return nil, false
		}
		g := m.newGroup()
		out = append(out, &ical.Prop{Group: g, Name: "X-ABRELATEDNAMES", Value: ical.EscapeText(key)},
			&ical.Prop{Group: g, Name: "X-ABLABEL", Value: ical.EscapeText(encodeAppleLabel(label))})
	}
	return out, true
}

// appleRelationLabel is the label written for a relation; each maps
// back through appleRelations.
var appleRelationLabel = map[string]string{
	"parent": "Parent", "child": "Child", "friend": "Friend", "spouse": "Spouse", "agent": "Assistant", "sibling": "Sibling",
}

// vCardProps renders jCard arrays back into lines.
func (m *merger) vCardProps(list []any) []*ical.Prop {
	var out []*ical.Prop
	for _, item := range list {
		a, _ := item.([]any)
		if len(a) != 4 {
			continue
		}
		p := &ical.Prop{Name: strings.ToUpper(str(a[0]))}
		params, _ := a[1].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(params)) {
			if name == "group" {
				p.Group = str(params[name])
				continue
			}
			p.Params = append(p.Params, ical.Param{Name: strings.ToUpper(name), Values: paramStrings(params[name])})
		}
		typ := str(a[2])
		switch typ {
		case "unknown":
			p.Value = str(a[3])
		case "text":
			p.Value = ical.EscapeText(str(a[3]))
			p.Params = append(p.Params, ical.Param{Name: "VALUE", Values: []string{"text"}})
		default:
			p.Value = str(a[3])
			p.Params = append(p.Params, ical.Param{Name: "VALUE", Values: []string{typ}})
		}
		out = append(out, p)
	}
	return out
}

// Apple's built-in labels are written in their encoded form.
var appleBuiltin = map[string]bool{
	"Home": true, "Work": true, "Other": true, "Main": true, "HomePage": true, "HomeFAX": true, "WorkFAX": true,
	"Pager": true, "Mobile": true, "iPhone": true, "School": true, "Anniversary": true,
	"Father": true, "Mother": true, "Parent": true, "Brother": true, "Sister": true, "Child": true,
	"Friend": true, "Spouse": true, "Partner": true, "Assistant": true, "Manager": true,
}

func encodeAppleLabel(l string) string {
	if appleBuiltin[l] {
		return "_$!<" + l + ">!$_"
	}
	return l
}

// escStruct escapes one value of a structured property (N, ADR, ORG).
func escStruct(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`).Replace(s)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), n == float64(int(n))
	}
	return 0, false
}

// keys returns the keys of a map[string]any set, sorted.
func keys(v any) []string {
	m, _ := v.(map[string]any)
	return slices.Sorted(maps.Keys(m))
}

func paramStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}
