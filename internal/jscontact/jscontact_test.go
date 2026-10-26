package jscontact

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

var update = flag.Bool("update", false, "rewrite the golden .json files")

// TestGolden converts every testdata/*.vcf and compares with the .json
// next to it. Run with -update after an intended change and review the
// diff.
func TestGolden(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.vcf")
	if len(files) == 0 {
		t.Fatal("no test data")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			card, err := Codec{}.ToJMAP(raw)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.MarshalIndent(card, "", "  ")
			got = append(got, '\n')
			golden := strings.TrimSuffix(f, ".vcf") + ".json"
			if *update {
				os.WriteFile(golden, got, 0o644)
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err, "(run with -update)")
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs:\n%s", golden, got)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	for in, want := range map[string]string{
		"19960415":             `{"@type":"PartialDate","day":15,"month":4,"year":1996}`,
		"1996-04-15":           `{"@type":"PartialDate","day":15,"month":4,"year":1996}`,
		"--0415":               `{"@type":"PartialDate","day":15,"month":4}`,
		"--04-15":              `{"@type":"PartialDate","day":15,"month":4}`,
		"--04":                 `{"@type":"PartialDate","month":4}`,
		"---15":                `{"@type":"PartialDate","day":15}`,
		"1996-04":              `{"@type":"PartialDate","month":4,"year":1996}`,
		"1996":                 `{"@type":"PartialDate","year":1996}`,
		"19530115T231000Z":     `{"@type":"Timestamp","utc":"1953-01-15T23:10:00Z"}`,
		"2009-08-08T14:30:00Z": `{"@type":"Timestamp","utc":"2009-08-08T14:30:00Z"}`,
		"19960415T1200":        `{"@type":"PartialDate","day":15,"month":4,"year":1996}`,
	} {
		d, ok := parseDate(in)
		got, _ := json.Marshal(d)
		if !ok || string(got) != want {
			t.Errorf("%s: %s %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "circa 1800", "--1315", "19961332", "abcd"} {
		if _, ok := parseDate(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestTimeZone(t *testing.T) {
	for in, want := range map[string]string{"Europe/Berlin": "Europe/Berlin", "-0500": "Etc/GMT+5", "+02:00": "Etc/GMT-2", "+0000": "Etc/UTC"} {
		if got, ok := timeZone(in); !ok || got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
	for _, bad := range []string{"+0530", "", "VALUE=text;x"} {
		if _, ok := timeZone(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSplitStructured(t *testing.T) {
	got, _ := json.Marshal(splitStructured(`a\;b;c,d\,e;;f\nx`))
	if string(got) != `[["a;b"],["c","d,e"],[""],["f\nx"]]` {
		t.Fatal(string(got))
	}
}

func TestNotAVCard(t *testing.T) {
	if _, err := (Codec{}).ToJMAP([]byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")); err == nil {
		t.Fatal("converted a VCALENDAR")
	}
}

// See the jscalendar test of the same name.
func TestDeepJSPropDropped(t *testing.T) {
	for _, prop := range []string{
		"JSPROP;JSPTR=" + strings.Repeat("a/", 20000) + "b:1",
		"JSPROP;JSPTR=x:" + strings.Repeat("[", 9990) + strings.Repeat("]", 9990),
		"JSPROP;JSPTR=" + strings.Repeat("a/", 15) + "b:" + strings.Repeat("[", 9990) + strings.Repeat("]", 9990),
	} {
		vc, err := ical.ParseBytes([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:u\r\nFN:f\r\n" + prop + "\r\nEND:VCARD\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		card, err := FromVCard(vc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(card); err != nil {
			t.Errorf("%.40s...: %v", prop, err)
		}
	}
}
