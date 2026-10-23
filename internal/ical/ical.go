// Package ical is a minimal content-line parser and encoder for iCalendar
// (RFC 5545) and vCard (RFC 6350). It keeps property values raw (still
// escaped) so that data round-trips unchanged; use Prop.Text for unescaped
// TEXT values.
package ical

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Param is a property parameter. Names are upper-cased; values keep their
// case and are unquoted.
type Param struct {
	Name   string
	Values []string
}

// Prop is a single content line.
type Prop struct {
	Group  string // vCard group prefix ("item1"), empty for iCalendar
	Name   string // upper-cased
	Params []Param
	Value  string // raw, still escaped
}

// Component is a BEGIN/END block.
type Component struct {
	Name     string // upper-cased, e.g. VCALENDAR, VEVENT
	Props    []*Prop
	Children []*Component
}

// ErrSyntax is wrapped by all parse errors.
var ErrSyntax = errors.New("ical: syntax error")

// maxLineLen bounds a single unfolded content line.
const maxLineLen = 1 << 20

// Parse reads exactly one top-level component (VCALENDAR or VCARD).
func Parse(r io.Reader) (*Component, error) {
	comps, err := ParseAll(r)
	if err != nil {
		return nil, err
	}
	if len(comps) != 1 {
		return nil, fmt.Errorf("%w: expected 1 top-level component, got %d", ErrSyntax, len(comps))
	}
	return comps[0], nil
}

// ParseBytes is Parse on a byte slice.
func ParseBytes(b []byte) (*Component, error) { return Parse(bytes.NewReader(b)) }

// ParseAll reads all top-level components (a .vcf may hold several VCARDs).
func ParseAll(r io.Reader) ([]*Component, error) {
	lr := newLineReader(r)
	var (
		out   []*Component
		stack []*Component
	)
	for {
		line, n, err := lr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if line == "" {
			continue
		}
		p, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		switch p.Name {
		case "BEGIN":
			stack = append(stack, &Component{Name: strings.ToUpper(p.Value)})
		case "END":
			if len(stack) == 0 || stack[len(stack)-1].Name != strings.ToUpper(p.Value) {
				return nil, fmt.Errorf("%w: line %d: unexpected END:%s", ErrSyntax, n, p.Value)
			}
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				out = append(out, c)
			} else {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, c)
			}
		default:
			if len(stack) == 0 {
				return nil, fmt.Errorf("%w: line %d: property outside component", ErrSyntax, n)
			}
			c := stack[len(stack)-1]
			c.Props = append(c.Props, p)
		}
	}
	if len(stack) != 0 {
		return nil, fmt.Errorf("%w: missing END:%s", ErrSyntax, stack[len(stack)-1].Name)
	}
	return out, nil
}

type lineReader struct {
	br      *bufio.Reader
	pending string
	havePen bool
	lineNo  int
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{br: bufio.NewReader(r)} }

func (l *lineReader) physical() (string, error) {
	if l.havePen {
		l.havePen = false
		return l.pending, nil
	}
	var sb strings.Builder
	for {
		frag, isPrefix, err := l.br.ReadLine()
		if err != nil {
			if err == io.EOF && sb.Len() > 0 {
				break
			}
			return "", err
		}
		if sb.Len()+len(frag) > maxLineLen {
			return "", fmt.Errorf("%w: line too long", ErrSyntax)
		}
		sb.Write(frag)
		if !isPrefix {
			break
		}
	}
	l.lineNo++
	return sb.String(), nil
}

// next returns the next unfolded logical line.
func (l *lineReader) next() (string, int, error) {
	first, err := l.physical()
	if err != nil {
		return "", 0, err
	}
	start := l.lineNo
	var sb strings.Builder
	sb.WriteString(first)
	for {
		nxt, err := l.physical()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, err
		}
		if len(nxt) > 0 && (nxt[0] == ' ' || nxt[0] == '\t') {
			if sb.Len()+len(nxt) > maxLineLen {
				return "", 0, fmt.Errorf("%w: line too long", ErrSyntax)
			}
			sb.WriteString(nxt[1:])
			continue
		}
		l.pending, l.havePen = nxt, true
		break
	}
	return sb.String(), start, nil
}

func parseLine(line string) (*Prop, error) {
	p := &Prop{}
	i := strings.IndexAny(line, ";:")
	if i <= 0 {
		return nil, fmt.Errorf("%w: missing name", ErrSyntax)
	}
	name := line[:i]
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		p.Group, name = name[:dot], name[dot+1:]
		if !validName(p.Group) {
			return nil, fmt.Errorf("%w: bad group %q", ErrSyntax, p.Group)
		}
	}
	if !validName(name) {
		return nil, fmt.Errorf("%w: bad property name %q", ErrSyntax, line[:i])
	}
	p.Name = strings.ToUpper(name)
	rest := line[i:]
	for len(rest) > 0 && rest[0] == ';' {
		rest = rest[1:]
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			// Tolerate vCard 2.1 style "TYPE" without value.
			end := strings.IndexAny(rest, ";:")
			if end < 0 {
				return nil, fmt.Errorf("%w: bad parameter", ErrSyntax)
			}
			p.Params = append(p.Params, Param{Name: "TYPE", Values: []string{rest[:end]}})
			rest = rest[end:]
			continue
		}
		param := Param{Name: strings.ToUpper(rest[:eq])}
		rest = rest[eq+1:]
		for {
			var v string
			if len(rest) > 0 && rest[0] == '"' {
				end := strings.IndexByte(rest[1:], '"')
				if end < 0 {
					return nil, fmt.Errorf("%w: unterminated quote", ErrSyntax)
				}
				v, rest = rest[1:1+end], rest[2+end:]
			} else {
				end := strings.IndexAny(rest, ",;:")
				if end < 0 {
					return nil, fmt.Errorf("%w: missing value", ErrSyntax)
				}
				v, rest = rest[:end], rest[end:]
			}
			param.Values = append(param.Values, unescapeParam(v))
			if len(rest) > 0 && rest[0] == ',' {
				rest = rest[1:]
				continue
			}
			break
		}
		p.Params = append(p.Params, param)
	}
	if len(rest) == 0 || rest[0] != ':' {
		return nil, fmt.Errorf("%w: missing ':'", ErrSyntax)
	}
	p.Value = rest[1:]
	return p, nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// RFC 6868 parameter value encoding.
func unescapeParam(s string) string {
	if !strings.Contains(s, "^") {
		return s
	}
	return strings.NewReplacer("^n", "\n", "^N", "\n", "^'", `"`, "^^", "^").Replace(s)
}

func escapeParam(s string) string {
	s = strings.NewReplacer("^", "^^", "\n", "^n", `"`, "^'").Replace(s)
	if strings.ContainsAny(s, ",;:") {
		return `"` + s + `"`
	}
	return s
}

// Encode writes the component with CRLF line endings, folding at 75 octets.
func (c *Component) Encode(w io.Writer) error {
	bw := bufio.NewWriter(w)
	c.encode(bw)
	return bw.Flush()
}

// Bytes returns the encoded component.
func (c *Component) Bytes() []byte {
	var b bytes.Buffer
	c.Encode(&b)
	return b.Bytes()
}

func (c *Component) encode(w *bufio.Writer) {
	writeFolded(w, "BEGIN:"+c.Name)
	for _, p := range c.Props {
		writeFolded(w, p.line())
	}
	for _, ch := range c.Children {
		ch.encode(w)
	}
	writeFolded(w, "END:"+c.Name)
}

func (p *Prop) line() string {
	var sb strings.Builder
	if p.Group != "" {
		sb.WriteString(p.Group)
		sb.WriteByte('.')
	}
	sb.WriteString(p.Name)
	for _, pa := range p.Params {
		sb.WriteByte(';')
		sb.WriteString(pa.Name)
		sb.WriteByte('=')
		for i, v := range pa.Values {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(escapeParam(v))
		}
	}
	sb.WriteByte(':')
	sb.WriteString(p.Value)
	return sb.String()
}

func writeFolded(w *bufio.Writer, s string) {
	const limit = 75
	first := true
	for len(s) > 0 {
		max := limit
		if !first {
			max = limit - 1
		}
		if len(s) <= max {
			if !first {
				w.WriteByte(' ')
			}
			w.WriteString(s)
			break
		}
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if !first {
			w.WriteByte(' ')
		}
		w.WriteString(s[:cut])
		w.WriteString("\r\n")
		s = s[cut:]
		first = false
	}
	w.WriteString("\r\n")
}

// Prop returns the first property with the given name, or nil.
func (c *Component) Prop(name string) *Prop {
	for _, p := range c.Props {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// PropsNamed returns all properties with the given name.
func (c *Component) PropsNamed(name string) []*Prop {
	var out []*Prop
	for _, p := range c.Props {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// Value returns the raw value of the first property with the given name.
func (c *Component) Value(name string) string {
	if p := c.Prop(name); p != nil {
		return p.Value
	}
	return ""
}

// Set replaces all properties named p.Name with p (appending if absent).
func (c *Component) Set(p *Prop) {
	c.Del(p.Name)
	c.Props = append(c.Props, p)
}

// Del removes all properties with the given name.
func (c *Component) Del(name string) {
	out := c.Props[:0]
	for _, p := range c.Props {
		if p.Name != name {
			out = append(out, p)
		}
	}
	c.Props = out
}

// ChildrenNamed returns direct children with the given name.
func (c *Component) ChildrenNamed(name string) []*Component {
	var out []*Component
	for _, ch := range c.Children {
		if ch.Name == name {
			out = append(out, ch)
		}
	}
	return out
}

// Param returns the first value of the named parameter, or "".
func (p *Prop) Param(name string) string {
	for _, pa := range p.Params {
		if pa.Name == name && len(pa.Values) > 0 {
			return pa.Values[0]
		}
	}
	return ""
}

// SetParam sets (or with no values, removes) a parameter.
func (p *Prop) SetParam(name string, values ...string) {
	for i, pa := range p.Params {
		if pa.Name == name {
			if len(values) == 0 {
				p.Params = append(p.Params[:i], p.Params[i+1:]...)
			} else {
				p.Params[i].Values = values
			}
			return
		}
	}
	if len(values) > 0 {
		p.Params = append(p.Params, Param{Name: name, Values: values})
	}
}

// Text returns the unescaped TEXT value.
func (p *Prop) Text() string { return UnescapeText(p.Value) }

// UnescapeText reverses RFC 5545 TEXT escaping.
func UnescapeText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n', 'N':
				sb.WriteByte('\n')
			default:
				sb.WriteByte(s[i])
			}
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// EscapeText applies RFC 5545 TEXT escaping.
func EscapeText(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`).Replace(s)
}

// SplitList splits a comma-separated raw value, honoring backslash escapes.
func SplitList(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case ',':
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
