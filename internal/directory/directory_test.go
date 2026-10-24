package directory

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/store"
)

func uidOf(_ store.Kind, b []byte) (string, string) {
	c, err := ical.ParseBytes(b)
	if err != nil {
		return "", ""
	}
	return c.Value("UID"), ""
}

var defaults = map[string]store.Meta{"contacts": {DisplayName: "Contacts"}}

func testConfig() *config.Config {
	return &config.Config{
		Users: map[string]config.User{
			"alice": {Name: "Alice Liddell", Email: "alice@example.com"},
			"bob":   {},
			"carol": {Name: "Carol, the 2nd"},
		},
		Groups: map[string][]string{"family": {"alice", "bob"}, "work": {"carol"}},
	}
}

func dir(t *testing.T, st *store.Store, user string) *store.Collection {
	t.Helper()
	c, err := st.Collection(user, store.AddressBooks, Name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func body(t *testing.T, c *store.Collection, uid string) string {
	t.Helper()
	b, _, err := c.Get(uid + ".vcf")
	if err != nil {
		t.Fatalf("%s: %v", uid, err)
	}
	return string(b)
}

func TestSync(t *testing.T) {
	st := store.New(t.TempDir(), uidOf)
	cfg := testConfig()
	if err := Sync(cfg, st, defaults); err != nil {
		t.Fatal(err)
	}
	// The default address book still exists next to the directory.
	if _, err := st.Collection("bob", store.AddressBooks, "contacts"); err != nil {
		t.Fatal("default address book missing:", err)
	}
	a := dir(t, st, "alice")
	if !a.ReadOnly() || a.Meta().DisplayName != "Directory" {
		t.Fatalf("%+v", a.Meta())
	}
	// 3 users + alice's one group.
	if n := len(a.List()); n != 4 {
		t.Fatalf("alice has %d cards", n)
	}
	al := body(t, a, UserUID("alice"))
	for _, want := range []string{"BEGIN:VCARD\r\n", "VERSION:3.0", "FN:Alice Liddell", "N:Liddell;Alice;;;", "EMAIL;TYPE=INTERNET:alice@example.com", "UID:" + UserUID("alice")} {
		if !strings.Contains(al, want) {
			t.Errorf("missing %q in\n%s", want, al)
		}
	}
	if b := body(t, a, UserUID("bob")); !strings.Contains(b, "FN:bob") || strings.Contains(b, "EMAIL") {
		t.Errorf("bob:\n%s", b)
	}
	if c := body(t, a, UserUID("carol")); !strings.Contains(c, `FN:Carol\, the 2nd`) {
		t.Errorf("carol not escaped:\n%s", c)
	}
	fam := body(t, a, uuidFor("group:family"))
	for _, want := range []string{"X-ADDRESSBOOKSERVER-KIND:group", "X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:" + UserUID("alice"), "X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:" + UserUID("bob")} {
		if !strings.Contains(fam, want) {
			t.Errorf("missing %q in\n%s", want, fam)
		}
	}
	// Only the owner's own groups.
	if _, _, err := a.Get(uuidFor("group:work") + ".vcf"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("alice sees carol's group")
	}

	// Nothing changed: no new sync token.
	tok := a.SyncToken()
	if err := Sync(cfg, st, defaults); err != nil || a.SyncToken() != tok {
		t.Fatalf("idempotent sync changed token: %v", err)
	}

	// Rename alice, drop bob: only those cards change.
	alice := cfg.Users["alice"]
	alice.Name = "Alice Pleasance"
	cfg.Users["alice"] = alice
	delete(cfg.Users, "bob")
	cfg.Groups["family"] = []string{"alice"}
	etag := func(uid string) string { i, _ := a.Stat(uid + ".vcf"); return i.ETag }
	carolTag := etag(UserUID("carol"))
	if err := Sync(cfg, st, defaults); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body(t, a, UserUID("alice")), "FN:Alice Pleasance") {
		t.Fatal("rename not applied")
	}
	if _, _, err := a.Get(UserUID("bob") + ".vcf"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("removed user still listed")
	}
	if etag(UserUID("carol")) != carolTag {
		t.Fatal("unchanged card rewritten")
	}
	if a.SyncToken() == tok {
		t.Fatal("token didn't move")
	}

	// Turned off: generated address books go, the rest stays.
	off := false
	cfg.Directory = &off
	if err := Sync(cfg, st, defaults); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Collection("alice", store.AddressBooks, Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("directory not removed")
	}
	if _, err := st.Collection("alice", store.AddressBooks, "contacts"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncLeavesOwnCollectionAlone(t *testing.T) {
	st := store.New(t.TempDir(), uidOf)
	cfg := testConfig()
	st.EnsureHome("bob", store.AddressBooks, defaults)
	own, err := st.CreateCollection("bob", store.AddressBooks, Name, store.Meta{DisplayName: "Mine"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Sync(cfg, st, defaults); err == nil || !strings.Contains(err.Error(), "not touching it") {
		t.Fatalf("err = %v", err)
	}
	if len(own.List()) != 0 || own.Meta().DisplayName != "Mine" {
		t.Fatal("own collection modified")
	}
	// Other users still got theirs.
	if len(dir(t, st, "alice").List()) == 0 {
		t.Fatal("alice's directory empty")
	}
	// And sharing a generated one is refused.
	if err := dir(t, st, "alice").SetShares([]store.Share{{User: "bob", Access: store.AccessRead}}); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("share: %v", err)
	}
}

func TestUUIDFormat(t *testing.T) {
	u := UserUID("alice")
	if len(u) != 36 || u[14] != '5' || !strings.ContainsAny(u[19:20], "89ab") || u != UserUID("alice") || u == UserUID("bob") {
		t.Fatal(u)
	}
}

// FuzzCards checks that any name and email from config.yaml give vCards
// that parse back to the same values.
func FuzzCards(f *testing.F) {
	f.Add("Alice Liddell", "alice@example.com")
	f.Add(`Back\slash; semi, comma`, "")
	f.Add("Line\nbreak", "x@y")
	f.Fuzz(func(t *testing.T, name, email string) {
		if strings.ContainsAny(name+email, "\r") || !utf8.ValidString(name+email) {
			return // not representable in YAML-loaded config either
		}
		cfg := &config.Config{Users: map[string]config.User{"u": {Name: name, Email: email}}}
		b := Cards(cfg, "u")[UserUID("u")+".vcf"]
		c, err := ical.ParseBytes(b)
		if err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		wantFN := name
		if wantFN == "" {
			wantFN = "u"
		}
		if got := c.Prop("FN").Text(); got != wantFN {
			t.Fatalf("FN %q, want %q", got, wantFN)
		}
		if email != "" && c.Prop("EMAIL").Text() != email {
			t.Fatalf("EMAIL %q, want %q", c.Prop("EMAIL").Text(), email)
		}
		if c.Value("UID") != UserUID("u") {
			t.Fatal("UID")
		}
	})
}
