package auth

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHashVerify(t *testing.T) {
	h, err := Hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(h, "s3cret"); !ok || err != nil {
		t.Fatalf("verify: %v %v", ok, err)
	}
	if ok, _ := Verify(h, "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	if _, err := Verify("garbage", "x"); err == nil {
		t.Fatal("expected format error")
	}
	c := NewCache(time.Minute)
	if !c.Verify("u", h, "s3cret") || !c.Verify("u", h, "s3cret") || c.Verify("u", h, "nope") {
		t.Fatal("cache")
	}
}

func TestAppPasswords(t *testing.T) {
	dir := t.TempDir()
	a := NewAppPasswords(dir)
	pw, ap, err := a.Create("alice", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 27 || ap.Name != "Phone" || ap.Hash == "" {
		t.Fatalf("%q %+v", pw, ap)
	}
	if _, _, err := a.Create("alice", " "); err == nil {
		t.Fatal("empty name accepted")
	}
	if !a.Verify("alice", pw) || a.Verify("alice", "nope") || a.Verify("bob", pw) {
		t.Fatal("verify")
	}
	// Persisted and reloaded.
	b := NewAppPasswords(dir)
	if l, _ := b.List("alice"); len(l) != 1 || l[0].ID != ap.ID {
		t.Fatalf("%+v", l)
	}
	if l, _ := b.List("alice"); l[0].LastUsed.IsZero() {
		t.Fatal("last used not recorded")
	}
	if !b.Verify("alice", pw) {
		t.Fatal("verify after reload")
	}
	if err := b.Revoke("alice", ap.ID); err != nil {
		t.Fatal(err)
	}
	if b.Verify("alice", pw) || b.Revoke("alice", ap.ID) == nil {
		t.Fatal("revoked password still valid")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter()
	l.PerUser, l.PerIP = 3, 5
	for range 3 {
		if ok, _ := l.Allow("1.1.1.1", "alice"); !ok {
			t.Fatal("blocked too early")
		}
		l.Fail("1.1.1.1", "alice")
	}
	if ok, wait := l.Allow("1.1.1.1", "alice"); ok || wait <= 0 {
		t.Fatal("not blocked after 3 failures", wait)
	}
	if ok, _ := l.Allow("2.2.2.2", "alice"); !ok {
		t.Fatal("other IP blocked")
	}
	if ok, _ := l.Allow("1.1.1.1", "bob"); !ok {
		t.Fatal("other user from same IP blocked")
	}
	// Per-IP cap across user names.
	l.Fail("1.1.1.1", "bob")
	l.Fail("1.1.1.1", "carol")
	if ok, _ := l.Allow("1.1.1.1", "dave"); ok {
		t.Fatal("per-IP cap not enforced")
	}
	// Success clears the pair counter; window expiry clears everything.
	l2 := NewLimiter()
	l2.PerUser = 2
	l2.Fail("ip", "u")
	l2.Success("ip", "u")
	l2.Fail("ip", "u")
	if ok, _ := l2.Allow("ip", "u"); !ok {
		t.Fatal("success did not reset")
	}
	l2.Window = time.Nanosecond
	l2.Fail("ip", "u")
	time.Sleep(time.Millisecond)
	if ok, _ := l2.Allow("ip", "u"); !ok {
		t.Fatal("window did not expire")
	}
}

// Asking costs no memory; failures keep at most a clipped name each.
func TestLimiterMemory(t *testing.T) {
	l := NewLimiter()
	long := strings.Repeat("x", 1<<20)
	for i := range 1000 {
		if ok, _ := l.Allow("192.0.2.1", long+strconv.Itoa(i)); !ok {
			t.Fatal("refused before any failure")
		}
	}
	if len(l.fails) != 0 {
		t.Fatalf("%d entries after only asking", len(l.fails))
	}
	l.Fail("192.0.2.1", long)
	for k := range l.fails {
		if len(k) > 100 {
			t.Fatalf("key of %d bytes", len(k))
		}
	}
	if ok, _ := l.Allow("192.0.2.1", long[:200]); !ok {
		t.Fatal("refused after one failure")
	}
}

// One IPv6 client has a whole /64: counting per address let it rotate
// through addresses for unlimited guesses.
func TestLimiterIPv6Prefix(t *testing.T) {
	l := NewLimiter()
	for i := range l.PerIP {
		l.Fail(fmt.Sprintf("2001:db8:1:2::%x", i+1), "u"+strconv.Itoa(i))
	}
	if ok, _ := l.Allow("2001:db8:1:2:ffff::1", "x"); ok {
		t.Fatal("same /64, new address: allowed")
	}
	if ok, _ := l.Allow("2001:db8:1:3::1", "x"); !ok {
		t.Fatal("another /64 refused")
	}
	if ok, _ := l.Allow("192.0.2.7", "x"); !ok {
		t.Fatal("IPv4 refused")
	}
}

// Guesses at one user's config.yaml password from ever new addresses were
// unlimited. After PerAccount failures, that password stops working for
// the window; app passwords, which can't be guessed, go on working.
func TestAccountWidePasswordLimit(t *testing.T) {
	dir := t.TempDir()
	ap := NewAppPasswords(dir)
	app, _, err := ap.Create("alice", "phone")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := Hash("right")
	l := NewLimiter()
	b := NewBasic(func(u string) (string, bool) {
		if u == "alice" {
			return hash, true
		}
		return "", false
	}, ap, l, slog.New(slog.NewTextHandler(io.Discard, nil)), "dav")
	try := func(ip, pw string) int {
		req := httptest.NewRequest("PROPFIND", "/dav/", nil)
		req.SetBasicAuth("alice", pw)
		w := httptest.NewRecorder()
		b.Authenticate(w, req, ip)
		return w.Code
	}
	if try("192.0.2.1", "right") != http.StatusOK {
		t.Fatal("right password refused")
	}
	l.PerAccount = 20 // PBKDF2 per guess: keep the test short
	for i := range l.PerAccount {
		try(fmt.Sprintf("198.51.100.%d", i), "guess")
	}
	if st := try("203.0.113.9", "right"); st == http.StatusOK {
		t.Fatal("config.yaml password still accepted after many failures from many addresses")
	}
	if st := try("203.0.113.9", app); st != http.StatusOK {
		t.Fatalf("app password refused: %d", st)
	}
}
