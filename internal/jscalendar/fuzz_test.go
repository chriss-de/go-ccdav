package jscalendar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// FuzzFromICal: any parseable calendar converts without panicking, into
// something that marshals as JSON.
func FuzzFromICal(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.ics")
	for _, p := range files {
		b, _ := os.ReadFile(p)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		ev, err := Codec{}.ToJMAP(raw)
		if err != nil {
			return
		}
		if _, err := json.Marshal(ev); err != nil {
			t.Fatal(err)
		}
	})
}

// FuzzMerge: for any calendar, merging its own conversion changes
// nothing, and adding an alert gives a calendar that parses and has it.
func FuzzMerge(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.ics")
	for _, p := range files {
		b, _ := os.ReadFile(p)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		prev, err := ical.ParseBytes(raw)
		if err != nil {
			return
		}
		ev, err := FromICal(prev)
		if err != nil {
			return
		}
		if _, changed, err := Merge(prev, ev); err == nil && changed {
			t.Fatalf("own conversion changed the calendar:\n%s", raw)
		}
		e := normalize(ev)
		entries(e, "alerts")["fz"] = map[string]any{"trigger": map[string]any{"offset": "-PT1M"}}
		out, _, err := Merge(prev, e)
		if err != nil {
			return // e.g. the fuzzed event itself is invalid
		}
		back, err := ical.ParseBytes(out.Bytes())
		if err != nil {
			t.Fatalf("output doesn't parse: %v\n%s", err, out.Bytes())
		}
		got, err := FromICal(back)
		if err != nil {
			t.Fatalf("output has no event: %v\n%s", err, out.Bytes())
		}
		if a, _ := got["alerts"].(map[string]any); a["fz"] == nil {
			t.Fatalf("added alert lost:\n%s", out.Bytes())
		}
	})
}
