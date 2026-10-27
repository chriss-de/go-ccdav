package trace

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	dir := t.TempDir()
	h, err := Handler(dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Set-Cookie", "s=secret")
		w.WriteHeader(http.StatusMultiStatus)
		w.Write([]byte("echo:" + string(b)))
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PROPFIND", "/dav/x/", strings.NewReader("<propfind/>"))
	r.SetBasicAuth("al/ice", "pw")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMultiStatus || w.Body.String() != "echo:<propfind/>" {
		t.Fatalf("passthrough broken: %d %q", w.Code, w.Body)
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*-00001-al_ice-PROPFIND.txt")) // <run start>-<n>-<user>-<method>
	if len(m) != 1 {
		t.Fatalf("trace files %v", m)
	}
	b, err := os.ReadFile(m[0])
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"PROPFIND /dav/x/ HTTP/1.1", "Authorization: (redacted)", "Set-Cookie: (redacted)", "<propfind/>", "--- 207 Multi-Status", "echo:<propfind/>"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "secret") || strings.Contains(s, "cHc") {
		t.Fatalf("credentials leaked:\n%s", s)
	}
}
