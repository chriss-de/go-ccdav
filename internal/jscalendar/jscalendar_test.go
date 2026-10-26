package jscalendar

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden .json files")

// TestGolden converts every testdata/*.ics and compares with the .json
// next to it. Run with -update after an intended change and review.
func TestGolden(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.ics")
	if len(files) == 0 {
		t.Fatal("no test data")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, _ := os.ReadFile(f)
			ev, err := Codec{}.ToJMAP(raw)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.MarshalIndent(ev, "", "  ")
			got = append(got, '\n')
			golden := strings.TrimSuffix(f, ".ics") + ".json"
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

func TestFormatParts(t *testing.T) {
	for _, tc := range []struct {
		neg   bool
		days  int
		clock string
		want  string
	}{
		{false, 0, "0s", "PT0S"}, {false, 1, "0s", "P1D"}, {false, 0, "1h30m", "PT1H30M"},
		{true, 0, "15m", "-PT15M"}, {false, 2, "25h", "P3DT1H"}, {false, 0, "45s", "PT45S"},
	} {
		d, _ := time.ParseDuration(tc.clock)
		if got := formatParts(tc.neg, tc.days, d); got != tc.want {
			t.Errorf("%+v: %s", tc, got)
		}
	}
}

func TestOnlyOverride(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\nRECURRENCE-ID;TZID=Europe/Berlin:20261103T100000\r\nDTSTART;TZID=Europe/Berlin:20261103T110000\r\nSUMMARY:moved instance\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	e, err := Codec{}.ToJMAP([]byte(raw))
	if err != nil || e["recurrenceId"] != "2026-11-03T10:00:00" || e["recurrenceIdTimeZone"] != "Europe/Berlin" || e["start"] != "2026-11-03T11:00:00" {
		t.Fatal(err, e)
	}
	if _, err := (Codec{}).ToJMAP([]byte("BEGIN:VCALENDAR\r\nBEGIN:VTODO\r\nUID:t\r\nEND:VTODO\r\nEND:VCALENDAR\r\n")); err != ErrNoEvent {
		t.Fatal("task converted", err)
	}
}

func TestWallDurationAcrossDST(t *testing.T) {
	// 01:00 to 04:00 in Berlin on the night clocks go back: 4 hours of
	// elapsed time, but JSCalendar durations are nominal.
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTART;TZID=Europe/Berlin:20261025T010000\r\nDTEND;TZID=Europe/Berlin:20261025T040000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	e, _ := Codec{}.ToJMAP([]byte(raw))
	if e["duration"] != "PT3H" {
		t.Fatal(e["duration"])
	}
}
