package jscontact

import (
	"slices"
	"strconv"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// params are a property's parameters while it is mapped: what a mapping
// takes is removed, and what remains ends up in vCardParams.
type params struct {
	types []string // TYPE values, lower-cased, without "pref"
	pref  int      // PREF, or 1 for a version 3 TYPE=pref
	value string   // VALUE
	other map[string][]string
	order []string // names in other, as they appeared
}

func newParams(p *ical.Prop) *params {
	pp := &params{other: map[string][]string{}}
	for _, prm := range p.Params {
		name, vals := strings.ToLower(prm.Name), prm.Values
		switch name {
		case "type":
			// Version 3 packs several types into one quoted value too.
			for _, v := range vals {
				for t := range strings.SplitSeq(v, ",") {
					t = strings.ToLower(strings.TrimSpace(t))
					if t == "pref" {
						pp.pref = 1
					} else if t != "" && !slices.Contains(pp.types, t) {
						pp.types = append(pp.types, t)
					}
				}
			}
		case "pref":
			if len(vals) > 0 {
				if n, err := strconv.Atoi(vals[0]); err == nil && n >= 1 && n <= 100 {
					pp.pref = n
				}
			}
		case "value":
			if len(vals) > 0 {
				pp.value = strings.ToLower(vals[0])
			}
		default:
			if _, seen := pp.other[name]; !seen {
				pp.order = append(pp.order, name)
			}
			pp.other[name] = append(pp.other[name], vals...)
		}
	}
	return pp
}

// take removes a parameter and returns its first value.
func (pp *params) take(name string) string {
	vals, ok := pp.other[name]
	if !ok {
		return ""
	}
	delete(pp.other, name)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// dropTypes removes TYPE values that carry no information.
func (pp *params) dropTypes(types ...string) {
	pp.types = slices.DeleteFunc(pp.types, func(t string) bool { return slices.Contains(types, t) })
}

// takeTypes removes the TYPE values found in mapping and returns what
// they map to; a nil mapping takes all of them as they are.
func (pp *params) takeTypes(mapping map[string]string) []string {
	var out, rest []string
	for _, t := range pp.types {
		if mapping == nil {
			out = append(out, t)
		} else if m, ok := mapping[t]; ok {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		} else {
			rest = append(rest, t)
		}
	}
	pp.types = rest
	return out
}

// takeFirstType removes and returns the first TYPE value (version 3
// media types like JPEG, social profile services like twitter).
func (pp *params) takeFirstType() string {
	if len(pp.types) == 0 {
		return ""
	}
	t := pp.types[0]
	pp.types = pp.types[1:]
	return t
}

var telFeatures = map[string]string{
	"cell": "mobile", "iphone": "mobile", "voice": "voice", "fax": "fax", "pager": "pager",
	"text": "text", "video": "video", "textphone": "textphone", "main-number": "main-number", "main": "main-number",
}

// apply sets contexts (TYPE home/work), pref and label on obj and puts
// all remaining parameters into its vCardParams.
func (pp *params) apply(obj map[string]any, label string) {
	for _, ctx := range pp.takeTypes(map[string]string{"home": "private", "work": "work"}) {
		setKey(obj, "contexts", ctx)
	}
	if pp.pref > 0 {
		obj["pref"] = pp.pref
	}
	if label != "" {
		obj["label"] = label
	}
	rest := map[string]any{}
	if len(pp.types) > 0 {
		rest["type"] = paramValue(pp.types)
	}
	if pp.value != "" {
		rest["value"] = pp.value
	}
	for _, name := range pp.order {
		if vals, ok := pp.other[name]; ok {
			rest[name] = paramValue(vals)
		}
	}
	if len(rest) > 0 {
		obj["vCardParams"] = rest
	}
}
