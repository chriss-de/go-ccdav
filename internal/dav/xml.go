// Package dav holds the WebDAV protocol plumbing: parsing XML request bodies
// into a generic tree and writing multistatus responses. Responses are built
// by hand with fixed prefixes because encoding/xml struct tags cope badly
// with the mix of DAV, CalDAV, CardDAV, CalendarServer and Apple namespaces.
package dav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const (
	NSDAV     = "DAV:"
	NSCalDAV  = "urn:ietf:params:xml:ns:caldav"
	NSCardDAV = "urn:ietf:params:xml:ns:carddav"
	NSCS      = "http://calendarserver.org/ns/"
	NSApple   = "http://apple.com/ns/ical/"
)

var prefixes = map[string]string{
	NSDAV:     "d",
	NSCalDAV:  "cal",
	NSCardDAV: "card",
	NSCS:      "cs",
	NSApple:   "ical",
}

// N builds an xml.Name.
func N(ns, local string) xml.Name { return xml.Name{Space: ns, Local: local} }

// Key renders a name as "{ns}local".
func Key(n xml.Name) string { return "{" + n.Space + "}" + n.Local }

// ParseKey reverses Key.
func ParseKey(k string) xml.Name {
	if strings.HasPrefix(k, "{") {
		if i := strings.IndexByte(k, '}'); i > 0 {
			return N(k[1:i], k[i+1:])
		}
	}
	return N("", k)
}

// Element is a generic parsed XML element.
type Element struct {
	Name     xml.Name
	Attrs    []xml.Attr
	Children []*Element
	Text     string // concatenated character data directly inside
	// Inner is the raw inner XML (for storing dead properties verbatim).
	Inner string
}

// Child returns the first child with the given name.
func (e *Element) Child(ns, local string) *Element {
	if e == nil {
		return nil
	}
	for _, c := range e.Children {
		if c.Name.Space == ns && c.Name.Local == local {
			return c
		}
	}
	return nil
}

// ChildrenNamed returns all children with the given name.
func (e *Element) ChildrenNamed(ns, local string) []*Element {
	if e == nil {
		return nil
	}
	var out []*Element
	for _, c := range e.Children {
		if c.Name.Space == ns && c.Name.Local == local {
			out = append(out, c)
		}
	}
	return out
}

// Attr returns an unqualified attribute value.
func (e *Element) Attr(local string) string {
	for _, a := range e.Attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// ErrBadXML wraps request body parse errors.
var ErrBadXML = errors.New("dav: malformed XML")

const maxXMLBody = 1 << 20

// ParseXML parses a request body. An empty body yields (nil, nil).
func ParseXML(r io.Reader) (*Element, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxXMLBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxXMLBody {
		return nil, fmt.Errorf("%w: body too large", ErrBadXML)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	d := xml.NewDecoder(strings.NewReader(string(data)))
	var (
		root   *Element
		stack  []*Element
		starts []int64
	)
	for {
		off := d.InputOffset()
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadXML, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) > 64 {
				return nil, fmt.Errorf("%w: too deep", ErrBadXML)
			}
			e := &Element{Name: t.Name, Attrs: t.Attr}
			if len(stack) == 0 {
				if root != nil {
					return nil, fmt.Errorf("%w: multiple roots", ErrBadXML)
				}
				root = e
			} else {
				p := stack[len(stack)-1]
				p.Children = append(p.Children, e)
			}
			stack = append(stack, e)
			starts = append(starts, d.InputOffset())
		case xml.EndElement:
			e := stack[len(stack)-1]
			e.Inner = string(data[starts[len(starts)-1]:off])
			stack, starts = stack[:len(stack)-1], starts[:len(starts)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("%w: no root element", ErrBadXML)
	}
	return root, nil
}

// Esc escapes text for XML character data / attribute values.
func Esc(s string) string {
	var sb strings.Builder
	xml.EscapeText(&sb, []byte(s))
	return sb.String()
}

// E renders an element with the given raw inner XML; empty inner renders
// a self-closing tag. Unknown namespaces get an inline xmlns declaration.
func E(name xml.Name, inner string) string {
	tag, decl := tagOf(name)
	if inner == "" {
		return "<" + tag + decl + "/>"
	}
	return "<" + tag + decl + ">" + inner + "</" + tag + ">"
}

// T renders an element containing escaped text.
func T(name xml.Name, text string) string { return E(name, Esc(text)) }

func tagOf(n xml.Name) (tag, decl string) {
	if p, ok := prefixes[n.Space]; ok {
		return p + ":" + n.Local, ""
	}
	if n.Space == "" {
		return n.Local, ` xmlns=""`
	}
	return "x:" + n.Local, ` xmlns:x="` + Esc(n.Space) + `"`
}

// Href renders a DAV:href for a path, escaping each segment.
func Href(path string) string {
	return E(N(NSDAV, "href"), Esc(EscapePath(path)))
}

// EscapePath percent-encodes each segment of a slash-separated path.
func EscapePath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Prop is one property in a response; Value is raw inner XML.
type Prop struct {
	Name  xml.Name
	Value string
}

// Response is one DAV:response. Either Props (grouped by status into
// propstats) or Status (for whole-resource status) is used.
type Response struct {
	Href   string
	Props  map[int][]Prop // status -> props
	Status int
	Error  string // raw XML inside DAV:error
}

// AddProp adds a property under the given status.
func (r *Response) AddProp(status int, p Prop) {
	if r.Props == nil {
		r.Props = make(map[int][]Prop)
	}
	r.Props[status] = append(r.Props[status], p)
}

// Multistatus is a 207 response body.
type Multistatus struct {
	Responses []*Response
	SyncToken string
}

func statusLine(code int) string {
	return "HTTP/1.1 " + strconv.Itoa(code) + " " + http.StatusText(code)
}

func (m *Multistatus) String() string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	sb.WriteString(`<d:multistatus`)
	for _, ns := range []string{NSDAV, NSCalDAV, NSCardDAV, NSCS, NSApple} {
		sb.WriteString(` xmlns:` + prefixes[ns] + `="` + ns + `"`)
	}
	sb.WriteString(">")
	for _, r := range m.Responses {
		sb.WriteString("<d:response>")
		sb.WriteString(Href(r.Href))
		if len(r.Props) == 0 {
			st := r.Status
			if st == 0 {
				st = http.StatusOK
			}
			sb.WriteString(T(N(NSDAV, "status"), statusLine(st)))
		}
		codes := make([]int, 0, len(r.Props))
		for c := range r.Props {
			codes = append(codes, c)
		}
		slices.Sort(codes)
		for _, c := range codes {
			sb.WriteString("<d:propstat><d:prop>")
			for _, p := range r.Props[c] {
				sb.WriteString(E(p.Name, p.Value))
			}
			sb.WriteString("</d:prop>")
			sb.WriteString(T(N(NSDAV, "status"), statusLine(c)))
			sb.WriteString("</d:propstat>")
		}
		if r.Error != "" {
			sb.WriteString(E(N(NSDAV, "error"), r.Error))
		}
		sb.WriteString("</d:response>")
	}
	if m.SyncToken != "" {
		sb.WriteString(T(N(NSDAV, "sync-token"), m.SyncToken))
	}
	sb.WriteString("</d:multistatus>\n")
	return sb.String()
}

// Write sends the multistatus with status 207.
func (m *Multistatus) Write(w http.ResponseWriter) {
	body := m.String()
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(w, body)
}

// WriteError sends a DAV:error body naming a failed precondition.
func WriteError(w http.ResponseWriter, code int, precondition xml.Name, desc string) {
	inner := E(precondition, "")
	if desc != "" {
		inner += T(N(NSDAV, "responsedescription"), desc)
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	sb.WriteString(`<d:error xmlns:d="DAV:" xmlns:cal="` + NSCalDAV + `" xmlns:card="` + NSCardDAV + `">`)
	sb.WriteString(inner)
	sb.WriteString("</d:error>\n")
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(code)
	io.WriteString(w, sb.String())
}

// Depth parses the Depth header; def is used when absent. Returns -1 for
// infinity.
func Depth(r *http.Request, def int) (int, error) {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Depth"))) {
	case "":
		return def, nil
	case "0":
		return 0, nil
	case "1":
		return 1, nil
	case "infinity":
		return -1, nil
	}
	return 0, errors.New("dav: bad Depth header")
}
