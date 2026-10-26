package jscontact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

func parseFile(t testing.TB, path string) *ical.Component {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ical.ParseBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func clone(m map[string]any) map[string]any { return normalize(m) }

// comparable drops what Merge sets itself.
func comparable(m map[string]any) map[string]any {
	m = normalize(m)
	delete(m, "updated")
	return m
}

func TestMergeUnchanged(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.vcf")
	for _, f := range files {
		prev := parseFile(t, f)
		card, _ := FromVCard(prev)
		out, changed, err := Merge(prev, card, "")
		if err != nil || changed || out != prev {
			t.Errorf("%s: changed=%v err=%v", f, changed, err)
		}
	}
}

// mods are changes a JMAP client could make. Each must survive the way
// into the vCard and back, in every test file's version.
var mods = map[string]func(c map[string]any){
	"change email": func(c map[string]any) {
		setEntry(c, "emails", "e1", map[string]any{"@type": "EmailAddress", "address": "new@example.com", "contexts": map[string]any{"work": true}})
	},
	"remove first email": func(c map[string]any) { deleteEntry(c, "emails", "e1") },
	"add labeled email with rank": func(c map[string]any) {
		setEntry(c, "emails", "x9", map[string]any{"@type": "EmailAddress", "address": "x@example.com", "label": "Club", "pref": 2})
	},
	"rename": func(c map[string]any) {
		c["name"] = map[string]any{"@type": "Name", "full": "Ada Lovelace", "components": []any{
			map[string]any{"@type": "NameComponent", "kind": "given", "value": "Ada"},
			map[string]any{"@type": "NameComponent", "kind": "surname", "value": "Lovelace"},
		}}
	},
	"name with unusual parts": func(c map[string]any) {
		c["name"] = map[string]any{"@type": "Name", "full": "X", "isOrdered": true, "components": []any{
			map[string]any{"@type": "NameComponent", "kind": "given", "value": "X"},
			map[string]any{"@type": "NameComponent", "kind": "separator", "value": " "},
			map[string]any{"@type": "NameComponent", "kind": "surname2", "value": "Y"},
		}}
	},
	"JSContact-only properties": func(c map[string]any) {
		c["speakToAs"] = map[string]any{"@type": "SpeakToAs", "grammaticalGender": "feminine"}
		c["example.com:custom"] = []any{"a", 1.0}
	},
	"keywords": func(c map[string]any) { c["keywords"] = map[string]any{"a,b": true, "c": true} },
	"birthday without year": func(c map[string]any) {
		setEntry(c, "anniversaries", "b1", map[string]any{"@type": "Anniversary", "kind": "birth", "date": map[string]any{"@type": "PartialDate", "month": 2, "day": 29}})
	},
	"wedding": func(c map[string]any) {
		setEntry(c, "anniversaries", "w1", map[string]any{"@type": "Anniversary", "kind": "wedding", "date": map[string]any{"@type": "PartialDate", "year": 2001, "month": 5, "day": 6}})
	},
	"spouse": func(c map[string]any) {
		c["relatedTo"] = map[string]any{"Grace": map[string]any{"@type": "Relation", "relation": map[string]any{"spouse": true}}}
	},
	"colleague": func(c map[string]any) {
		c["relatedTo"] = map[string]any{"urn:uuid:1": map[string]any{"@type": "Relation", "relation": map[string]any{"colleague": true}}}
	},
	"online services": func(c map[string]any) {
		setEntry(c, "onlineServices", "o1", map[string]any{"@type": "OnlineService", "service": "Mastodon", "uri": "https://example.social/@ada", "user": "ada"})
		setEntry(c, "onlineServices", "o2", map[string]any{"@type": "OnlineService", "uri": "xmpp:ada@example.com", "contexts": map[string]any{"private": true}})
	},
	"address": func(c map[string]any) {
		setEntry(c, "addresses", "h1", map[string]any{"@type": "Address", "full": "Main St 1\nTown", "countryCode": "DE", "components": []any{
			map[string]any{"@type": "AddressComponent", "kind": "name", "value": "Main St"},
			map[string]any{"@type": "AddressComponent", "kind": "number", "value": "1"},
			map[string]any{"@type": "AddressComponent", "kind": "locality", "value": "Town"},
		}})
	},
	"phone with features": func(c map[string]any) {
		setEntry(c, "phones", "p7", map[string]any{"@type": "Phone", "number": "+1 555", "features": map[string]any{"mobile": true, "text": true}, "pref": 1})
	},
	"photo": func(c map[string]any) {
		setEntry(c, "media", "m9", map[string]any{"@type": "Media", "kind": "photo", "uri": "data:image/png;base64,iVBORw0KGgo=", "mediaType": "image/png"})
	},
	"group": func(c map[string]any) {
		c["kind"] = "group"
		c["members"] = map[string]any{"a-uid": true, "urn:uuid:b": true}
	},
	"note with special characters": func(c map[string]any) {
		setEntry(c, "notes", "n5", map[string]any{"@type": "Note", "note": "a;b,c\\d\nline 2"})
	},
	"drop all notes": func(c map[string]any) { delete(c, "notes") },
	"vCardProps": func(c map[string]any) {
		c["vCardProps"] = []any{[]any{"x-foo", map[string]any{"group": "item42"}, "unknown", `raw\;value`}}
	},
}

func setEntry(c map[string]any, prop, id string, v map[string]any) {
	m, _ := c[prop].(map[string]any)
	if m == nil {
		m = map[string]any{}
		c[prop] = m
	}
	m[id] = v
}

func deleteEntry(c map[string]any, prop, id string) {
	if m, ok := c[prop].(map[string]any); ok {
		delete(m, id)
		if len(m) == 0 {
			delete(c, prop)
		}
	}
}

func TestMergeRoundTrip(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.vcf")
	for _, f := range files {
		for name, mod := range mods {
			t.Run(filepath.Base(f)+"/"+name, func(t *testing.T) {
				prev := parseFile(t, f)
				card, _ := FromVCard(prev)
				want := clone(card)
				mod(want)
				out, changed, err := Merge(prev, want, "")
				if err != nil {
					t.Fatal(err)
				}
				if !changed {
					return // the mod happened to change nothing
				}
				reparsed, err := ical.ParseBytes(out.Bytes())
				if err != nil {
					t.Fatalf("output doesn't parse: %v\n%s", err, out.Bytes())
				}
				got, _ := FromVCard(reparsed)
				if !reflect.DeepEqual(comparable(got), comparable(want)) {
					g, _ := json.MarshalIndent(comparable(got), "", " ")
					w, _ := json.MarshalIndent(comparable(want), "", " ")
					t.Fatalf("round trip differs\nvCard:\n%s\ngot:\n%s\nwant:\n%s", out.Bytes(), g, w)
				}
				// A second Merge of the same card writes nothing.
				if _, changed, _ := Merge(reparsed, want, ""); changed {
					t.Fatalf("second merge changed something:\n%s", out.Bytes())
				}
			})
		}
	}
}

func TestMergeKeepsOtherLines(t *testing.T) {
	raw, _ := os.ReadFile("testdata/apple-full.vcf")
	prev := parseFile(t, "testdata/apple-full.vcf")
	card, _ := FromVCard(prev)
	card = clone(card)
	card["emails"].(map[string]any)["e2"].(map[string]any)["address"] = "changed@other.example"
	out, _, err := Merge(prev, card, "")
	if err != nil {
		t.Fatal(err)
	}
	orig := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	got := string(out.Bytes())
	for _, l := range orig {
		if strings.Contains(l, "john@other.example") || strings.HasPrefix(l, "item2.X-ABLabel") || strings.HasPrefix(l, "REV:") || l == "" ||
			strings.HasPrefix(l, "item1.EMAIL") || strings.HasPrefix(l, " e") { // e1 gains its X-PROP-ID (and is refolded)
			continue
		}
		if !strings.Contains(got, l) {
			t.Errorf("line %q lost or changed:\n%s", l, got)
		}
	}
	if !strings.Contains(got, "item2.EMAIL;TYPE=INTERNET;X-PROP-ID=e2:changed@other.example") || !strings.Contains(got, "item2.X-ABLABEL:Club") {
		t.Errorf("changed entry:\n%s", got)
	}
	if !strings.Contains(got, "item1.EMAIL;TYPE=INTERNET;TYPE=HOME;TYPE=pref;X-PROP-ID=e1:") {
		t.Errorf("ids not pinned:\n%s", got)
	}
}

var revRe = regexp.MustCompile(`REV:\d{8}T\d{6}Z`)

// TestNewCard writes new cards in both versions; compare with
// testdata/new-*.golden (REV masked).
func TestNewCard(t *testing.T) {
	card := map[string]any{"@type": "Card", "version": "1.0", "uid": "new-1",
		"name":          map[string]any{"full": "Ada Lovelace", "components": []any{map[string]any{"kind": "surname", "value": "Lovelace"}, map[string]any{"kind": "given", "value": "Ada"}}},
		"emails":        map[string]any{"e1": map[string]any{"address": "ada@example.com", "contexts": map[string]any{"work": true}, "pref": 1}},
		"phones":        map[string]any{"p1": map[string]any{"number": "+44 20 1234", "features": map[string]any{"mobile": true}, "label": "Office"}},
		"anniversaries": map[string]any{"b": map[string]any{"kind": "birth", "date": map[string]any{"@type": "PartialDate", "month": 12, "day": 10}}},
		"keywords":      map[string]any{"math": true},
	}
	for _, v := range []string{"3.0", "4.0"} {
		out, changed, err := Merge(nil, card, v)
		if err != nil || !changed {
			t.Fatal(err, changed)
		}
		got := revRe.ReplaceAllString(string(out.Bytes()), "REV:XXX")
		golden := "testdata/new-" + v + ".golden"
		if *update {
			os.WriteFile(golden, []byte(got), 0o644)
			continue
		}
		want, _ := os.ReadFile(golden)
		if got != string(want) {
			t.Errorf("%s differs:\n%s", golden, got)
		}
		back, _ := FromVCard(out)
		if back["uid"] != "new-1" || back["prodId"] != ProdID {
			t.Errorf("%s: %v", v, back)
		}
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]any{
		"uid":     3.0,
		"emails":  map[string]any{"e1": map[string]any{"address": ""}, "bad id!": map[string]any{"address": "x"}},
		"phones":  map[string]any{"p1": map[string]any{"number": "1", "pref": 0.0, "features": map[string]any{"mobile": "yes"}}},
		"name":    map[string]any{"components": []any{map[string]any{"kind": "given"}}},
		"members": map[string]any{"x": false},
	}
	got := strings.Join(Validate(bad), " ")
	want := "emails/bad id! emails/e1/address members name/components phones/p1/pref phones/p1/features uid"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if _, _, err := Merge(nil, bad, "3.0"); err == nil {
		t.Fatal("merged an invalid card")
	}
}

func TestMergeIgnoresMissingTypes(t *testing.T) {
	prev := parseFile(t, "testdata/thunderbird-contact.vcf")
	card, _ := FromVCard(prev)
	card = clone(card)
	card["name"] = map[string]any{"full": "Erika Musterfrau", "components": []any{
		map[string]any{"kind": "surname", "value": "Musterfrau"}, map[string]any{"kind": "given", "value": "Erika"}}}
	if _, changed, err := Merge(prev, card, ""); err != nil || changed {
		t.Fatal("a card without @type counted as changed", err)
	}
}

// FuzzMerge: for any vCard, merging its own conversion changes nothing,
// and adding an email gives a vCard that parses and has it.
func FuzzMerge(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.vcf")
	for _, p := range files {
		b, _ := os.ReadFile(p)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		prev, err := ical.ParseBytes(raw)
		if err != nil {
			return
		}
		card, err := FromVCard(prev)
		if err != nil {
			return
		}
		if _, changed, err := Merge(prev, card, ""); err == nil && changed {
			t.Fatalf("own conversion changed the card:\n%s", raw)
		}
		card = clone(card)
		setEntry(card, "emails", "zz9", map[string]any{"address": "fuzz@example.com", "label": "L"})
		out, _, err := Merge(prev, card, "")
		if err != nil {
			return // e.g. the fuzzed card itself is invalid
		}
		back, err := ical.ParseBytes(out.Bytes())
		if err != nil {
			t.Fatalf("output doesn't parse: %v\n%s", err, out.Bytes())
		}
		got, _ := FromVCard(back)
		e, _ := got["emails"].(map[string]any)["zz9"].(map[string]any)
		if e == nil || e["address"] != "fuzz@example.com" || e["label"] != "L" {
			t.Fatalf("added email lost:\n%s", out.Bytes())
		}
	})
}
