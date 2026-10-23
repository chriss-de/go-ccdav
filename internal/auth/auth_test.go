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
