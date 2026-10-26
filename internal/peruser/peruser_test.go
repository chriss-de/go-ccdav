package peruser

import (
	"encoding/json"
	"testing"
)

func obj(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSplitApply(t *testing.T) {
	owner := obj(`{"title":"T","color":"red","alerts":{"a":{"trigger":{"offset":"-PT5M"}}},
		"iCalProps":[["x-foo",{},"unknown","1"],["x-moz-lastack",{},"unknown","20260101T000000Z"]],
		"recurrenceOverrides":{"2026-01-02T10:00:00":{"title":"moved"}}}`)
	// The user acknowledges differently, sets no alerts, adds a reminder
	// on one plain occurrence and changes the title.
	next := obj(`{"title":"T2","color":"red",
		"iCalProps":[["x-foo",{},"unknown","1"],["x-moz-lastack",{},"unknown","20260301T000000Z"]],
		"recurrenceOverrides":{"2026-01-02T10:00:00":{"title":"moved"},"2026-01-09T10:00:00":{"alerts":{"m":{"trigger":{"offset":"-PT1H"}}}}}}`)
	file, d := Split(next, owner)
	if js(file) != `{"alerts":{"a":{"trigger":{"offset":"-PT5M"}}},"color":"red","iCalProps":[["x-foo",{},"unknown","1"],["x-moz-lastack",{},"unknown","20260101T000000Z"]],"recurrenceOverrides":{"2026-01-02T10:00:00":{"title":"moved"}},"title":"T2"}` {
		t.Fatalf("file %s", js(file))
	}
	if js(d) != `{"props":{"alerts":null},"overrides":{"2026-01-09T10:00:00":{"alerts":{"m":{"trigger":{"offset":"-PT1H"}}}}},"iCalProps":[["x-moz-lastack",{},"unknown","20260301T000000Z"]],"hasICal":true}` {
		t.Fatalf("data %s", js(d))
	}
	// Applying the data to the (new) file gives the user's view back.
	if got := Apply(file, d); js(got) != js(next) {
		t.Fatalf("apply\n got %s\nwant %s", js(got), js(next))
	}
	// The owner's own values: nothing per-user to keep.
	if _, d := Split(owner, owner); !d.Empty() || Encode(d) != nil {
		t.Fatalf("%+v", d)
	}
	if !Decode(Encode(d)).HasICal {
		t.Fatal("round trip")
	}
}
