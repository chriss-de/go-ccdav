package peruser

import (
	"encoding/json"
	"testing"
)

func ev(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

// Removing a per-user property is recorded at the top level (d.Props[k] =
// nil) but not inside a recurrence override: Split only looks at the
// paths the user's override has, so a path the user dropped is not in
// their data and Apply shows the owner's value again. The edit is
// accepted and silently lost.
func TestOverrideRemovalLost(t *testing.T) {
	owner := ev(`{"title":"T","alerts":{"a":{"trigger":{"offset":"-PT5M"}}},
		"recurrenceOverrides":{"2026-01-02T10:00:00":{"title":"moved","alerts":{"b":{"trigger":{"offset":"-PT1H"}}}}}}`)
	// The user removes all alerts: at the top and in the override.
	next := ev(`{"title":"T",
		"recurrenceOverrides":{"2026-01-02T10:00:00":{"title":"moved"}}}`)
	_, d := Split(next, owner)
	seen := Apply(owner, Decode(Encode(d)))
	if _, ok := seen["alerts"]; ok {
		t.Fatal("top-level removal lost")
	}
	ro := seen["recurrenceOverrides"].(map[string]any)["2026-01-02T10:00:00"].(map[string]any)
	if ro["alerts"] != nil { // null: removed for this occurrence
		t.Fatalf("user removed the occurrence's alerts, but sees the owner's: %v (stored data %s)", ro["alerts"], Encode(d))
	}
}
