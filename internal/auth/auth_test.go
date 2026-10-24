package auth

import (
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
