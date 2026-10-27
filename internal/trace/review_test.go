package trace

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The file counter starts at 1 in every process, so after a restart
// with the same -trace directory the first requests overwrite the
// earlier run's files of the same user and method.
func TestTraceRestartOverwrites(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	serve := func(body string) {
		h, err := Handler(dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }), log)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/dav/x", nil)
		r.SetBasicAuth("alice", "pw")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	serve("first run")
	serve("second run") // the server was restarted
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		b, _ := os.ReadFile(filepath.Join(dir, ents[0].Name()))
		t.Fatalf("2 requests, %d trace file(s); %s:\n%s", len(ents), ents[0].Name(), b)
	}
}
