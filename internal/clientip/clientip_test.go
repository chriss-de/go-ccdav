package clientip

import (
	"net/http"
	"testing"
)

func TestFrom(t *testing.T) {
	trusted, err := ParsePrefixes([]string{"10.88.0.0/16", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		remote, xff, want string
	}{
		{"203.0.113.9:5000", "", "203.0.113.9"},
		{"203.0.113.9:5000", "1.2.3.4", "203.0.113.9"},                // untrusted peer: header ignored
		{"10.88.0.5:5000", "198.51.100.7", "198.51.100.7"},            // via proxy
		{"10.88.0.5:5000", "6.6.6.6, 198.51.100.7", "198.51.100.7"},   // spoofed left part ignored
		{"10.88.0.5:5000", "198.51.100.7, 10.88.0.9", "198.51.100.7"}, // two proxies
		{"127.0.0.1:1", "", "127.0.0.1"},                              // trusted, no header
		{"10.88.0.5:5000", "garbage, 198.51.100.7", "198.51.100.7"},
		{"10.88.0.5:5000", "198.51.100.7, garbage", "10.88.0.5"},
		{"[::ffff:10.88.0.5]:80", "2001:db8::1", "2001:db8::1"},
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := From(r, trusted); got != c.want {
			t.Errorf("%s / %q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	if _, err := ParsePrefixes([]string{"nope"}); err == nil {
		t.Error("bad prefix accepted")
	}
}
