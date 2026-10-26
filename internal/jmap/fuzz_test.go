package jmap

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FuzzAPI feeds arbitrary request bodies to the API endpoint: it must
// answer 200 or 400 with JSON and never panic. Result references are the
// interesting part, so the seeds use them.
func FuzzAPI(f *testing.F) {
	e := newEnv(f)
	for _, s := range []string{
		`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"a":1},"0"]]}`,
		`{"using":["urn:ietf:params:jmap:core","urn:example:test"],"methodCalls":[["Test/fixed",{},"f"],["Core/echo",{"#x":{"resultOf":"f","name":"Test/fixed","path":"/list/*/tags"}},"1"]]}`,
		`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"#x":{"resultOf":"0","name":"Core/echo","path":"/a~1b/*/0"}},"0"]]}`,
		`{"using":[],"methodCalls":[],"createdIds":{"a":"b"}}`,
		`{"using":["urn:ietf:params:jmap:core","urn:example:test"],"methodCalls":[["Test/changes",{"accountId":"u616c696365","sinceState":"x-1-o"},"0"]]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequest("POST", "/jmap/api", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.h.api(w, req, "alice")
		switch w.Code {
		case http.StatusOK, http.StatusBadRequest:
		default:
			t.Fatalf("status %d", w.Code)
		}
		if !json.Valid(w.Body.Bytes()) {
			t.Fatalf("invalid JSON response: %q", w.Body.Bytes())
		}
	})
}
