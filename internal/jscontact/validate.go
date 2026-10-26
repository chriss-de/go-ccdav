package jscontact

import (
	"maps"
	"slices"
)

// required is the field each entry of a map property must have, as a
// non-empty string ("" if none is required).
var required = map[string]string{
	"nicknames": "name", "titles": "name", "emails": "address", "phones": "number",
	"links": "uri", "media": "uri", "cryptoKeys": "uri", "calendars": "uri",
	"schedulingAddresses": "uri", "directories": "uri", "notes": "note", "preferredLanguages": "language",
}

// Validate checks the structure of a card as far as Merge relies on it
// and returns the paths of invalid properties.
func Validate(card map[string]any) []string {
	var bad []string
	fail := func(p string) { bad = append(bad, p) }
	for _, k := range slices.Sorted(maps.Keys(card)) {
		v := card[k]
		switch k {
		case "uid", "kind", "prodId", "language", "@type", "version", "created", "updated", "id":
			if _, ok := v.(string); !ok {
				fail(k)
			}
		case "members", "keywords":
			if !isSet(v) {
				fail(k)
			}
		case "name":
			n, ok := v.(map[string]any)
			if !ok {
				fail(k)
				continue
			}
			if f, ok := n["full"]; ok {
				if _, ok := f.(string); !ok {
					fail("name/full")
				}
			}
			if c, ok := n["components"]; ok && !validComponents(c) {
				fail("name/components")
			}
		case "relatedTo":
			rel, ok := v.(map[string]any)
			if !ok {
				fail(k)
				continue
			}
			for key, r := range rel {
				rm, ok := r.(map[string]any)
				if !ok || rm["relation"] != nil && !isSet(rm["relation"]) {
					fail("relatedTo/" + escapePointer(key))
				}
			}
		case "vCardProps":
			list, ok := v.([]any)
			if !ok {
				fail(k)
				continue
			}
			for _, item := range list {
				a, ok := item.([]any)
				if !ok || len(a) != 4 || str(a[0]) == "" || str(a[2]) == "" {
					fail(k)
					break
				}
				if _, ok := a[1].(map[string]any); !ok {
					fail(k)
					break
				}
				if _, ok := a[3].(string); !ok {
					fail(k)
					break
				}
			}
		default:
			if _, isEntries := entryProps[k]; isEntries {
				bad = append(bad, validEntries(k, v)...)
			}
		}
	}
	return bad
}

func validEntries(k string, v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return []string{k}
	}
	var bad []string
	for _, id := range slices.Sorted(maps.Keys(m)) {
		path := k + "/" + escapePointer(id)
		e, ok := m[id].(map[string]any)
		if !ok || !validID(id) {
			bad = append(bad, path)
			continue
		}
		if f := required[k]; f != "" && str(e[f]) == "" {
			bad = append(bad, path+"/"+f)
		}
		if c, ok := e["contexts"]; ok && !isSet(c) {
			bad = append(bad, path+"/contexts")
		}
		if p, ok := e["pref"]; ok {
			if n, ok := num(p); !ok || n < 1 || n > 100 {
				bad = append(bad, path+"/pref")
			}
		}
		if l, ok := e["label"]; ok {
			if _, ok := l.(string); !ok {
				bad = append(bad, path+"/label")
			}
		}
		if vp, ok := e["vCardParams"]; ok {
			if _, ok := vp.(map[string]any); !ok {
				bad = append(bad, path+"/vCardParams")
			}
		}
		switch k {
		case "phones":
			if f, ok := e["features"]; ok && !isSet(f) {
				bad = append(bad, path+"/features")
			}
		case "addresses":
			if c, ok := e["components"]; ok && !validComponents(c) {
				bad = append(bad, path+"/components")
			}
		case "anniversaries":
			d, ok := e["date"].(map[string]any)
			if !ok || str(e["kind"]) == "" {
				bad = append(bad, path)
			} else if d["@type"] == "Timestamp" && str(d["utc"]) == "" {
				bad = append(bad, path+"/date")
			}
		case "organizations":
			if str(e["name"]) == "" && e["units"] == nil {
				bad = append(bad, path+"/name")
			}
		case "onlineServices":
			if str(e["uri"]) == "" && str(e["user"]) == "" {
				bad = append(bad, path+"/uri")
			}
		}
	}
	return bad
}

// isSet reports whether v is a JSON object whose values are all true.
func isSet(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, x := range m {
		if x != true {
			return false
		}
	}
	return true
}

func validComponents(v any) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, c := range list {
		cm, ok := c.(map[string]any)
		if !ok || str(cm["kind"]) == "" {
			return false
		}
		if _, ok := cm["value"].(string); !ok {
			return false
		}
	}
	return true
}
