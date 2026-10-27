package jmap

import (
	"os"
	"strings"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/carddav"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// set calls one /set-like method and fails on a method error.
func (e *env) set(user, method string, args map[string]any) apiResp {
	e.t.Helper()
	r := e.api(user, call(method, args, "s"))[0]
	if r.Name == "error" {
		e.t.Fatalf("%s: %+v", method, r.Args)
	}
	return r
}

func setErrType(r apiResp, field, id string) string {
	m, _ := r.Args[field].(map[string]any)
	se, _ := m[id].(map[string]any)
	s, _ := se["type"].(string)
	return s
}

// file returns the stored vCard of a card id in alice's account.
func (e *env) file(owner, id string) string {
	e.t.Helper()
	log, _ := e.st.Account(owner, store.AddressBooks)
	ref, ok := log.Lookup(id)
	if !ok {
		e.t.Fatalf("no card %s", id)
	}
	col, _ := e.st.CollectionByID(owner, store.AddressBooks, ref.Coll)
	data, _, err := col.Get(ref.Name)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func TestContactCardCreate(t *testing.T) {
	e, book, _ := contactsEnv(t)
	r := e.set("alice", "ContactCard/set", map[string]any{"accountId": e.alice, "create": map[string]any{
		"c1": map[string]any{"addressBookIds": map[string]any{book.ID(): true},
			"name":   map[string]any{"full": "Ada Lovelace"},
			"emails": map[string]any{"e1": map[string]any{"address": "ada@example.com", "label": "Club"}}},
	}})
	created := r.Args["created"].(map[string]any)["c1"].(map[string]any)
	id, uid := created["id"].(string), created["uid"].(string)
	if id == "" || uid == "" || created["created"] == nil || r.Args["newState"] == r.Args["oldState"] {
		t.Fatalf("%+v", r.Args)
	}
	vcf := e.file("alice", id)
	if _, err := carddav.Validate([]byte(vcf)); err != nil || !strings.Contains(vcf, "VERSION:3.0") ||
		!strings.Contains(vcf, "UID:"+uid) || !strings.Contains(vcf, "X-ABLABEL:Club") {
		t.Fatalf("stored vCard (%v):\n%s", err, vcf)
	}

	// A creation id can be used later in the same request.
	rs := e.api("alice",
		call("ContactCard/set", map[string]any{"accountId": e.alice, "create": map[string]any{
			"c2": map[string]any{"addressBookIds": map[string]any{book.ID(): true}, "name": map[string]any{"full": "Grace"}}}}, "0"),
		call("ContactCard/get", map[string]any{"accountId": e.alice, "ids": []string{"#c2"}, "properties": []string{"name"}}, "1"))
	if l := rs[1].Args["list"].([]any); len(l) != 1 || l[0].(map[string]any)["name"].(map[string]any)["full"] != "Grace" {
		t.Fatalf("get by creation id: %+v", rs[1].Args)
	}

	dir, _ := e.st.CreateCollection("alice", store.AddressBooks, "dir", store.Meta{ReadOnly: true})
	other, _ := e.st.CreateCollection("alice", store.AddressBooks, "other", store.Meta{})
	r = e.set("alice", "ContactCard/set", map[string]any{"accountId": e.alice, "create": map[string]any{
		"none":     map[string]any{"name": map[string]any{"full": "X"}},
		"two":      map[string]any{"addressBookIds": map[string]any{book.ID(): true, other.ID(): true}},
		"readonly": map[string]any{"addressBookIds": map[string]any{dir.ID(): true}},
		"withId":   map[string]any{"addressBookIds": map[string]any{book.ID(): true}, "id": "x"},
		"badEmail": map[string]any{"addressBookIds": map[string]any{book.ID(): true}, "emails": map[string]any{"e1": map[string]any{"address": ""}}},
		"dupUID":   map[string]any{"addressBookIds": map[string]any{book.ID(): true}, "uid": uid},
	}})
	for cid, want := range map[string]string{"none": SetInvalidProperties, "two": SetInvalidProperties, "readonly": SetForbidden,
		"withId": SetInvalidProperties, "badEmail": SetInvalidProperties, "dupUID": SetAlreadyExists} {
		if got := setErrType(r, "notCreated", cid); got != want {
			t.Errorf("%s: %q, want %q", cid, got, want)
		}
	}
	nc := r.Args["notCreated"].(map[string]any)
	if nc["dupUID"].(map[string]any)["existingId"] != id || !strings.Contains(strings.Join(ids(nc["badEmail"].(map[string]any)["properties"]), ","), "emails/e1/address") {
		t.Fatalf("details %+v", nc)
	}
}

func TestContactCardUpdate(t *testing.T) {
	e, book, cards := contactsEnv(t)
	id := cards["apple-full.vcf"]
	_, before, _ := book.Get("apple-full.vcf")
	r := e.set("alice", "ContactCard/set", map[string]any{"accountId": e.alice, "update": map[string]any{
		id: map[string]any{"emails/e2/address": "new@club.example", "nicknames/n1": nil},
	}})
	if r.Args["updated"].(map[string]any)[id] == nil {
		t.Fatalf("%+v", r.Args)
	}
	vcf := e.file("alice", id)
	for _, keep := range []string{"item3.X-ABADR:us", "X-PHONETIC-FIRST-NAME:Jon", "item1.EMAIL", "PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgAB\r\n AQAAAQABAAD"} {
		if !strings.Contains(vcf, keep) {
			t.Errorf("lost %q:\n%s", keep, vcf)
		}
	}
	if !strings.Contains(vcf, ":new@club.example") || strings.Contains(vcf, "Johnny") || !strings.Contains(vcf, "JA") {
		t.Errorf("update not applied:\n%s", vcf)
	}
	if _, after, _ := book.Get("apple-full.vcf"); after.ETag == before.ETag {
		t.Error("ETag unchanged")
	}

	// A patch that changes nothing writes nothing.
	_, before, _ = book.Get("apple-full.vcf")
	r = e.set("alice", "ContactCard/set", map[string]any{"accountId": e.alice, "update": map[string]any{id: map[string]any{"emails/e2/address": "new@club.example"}}})
	if _, after, _ := book.Get("apple-full.vcf"); after.ETag != before.ETag {
		t.Error("no-op update rewrote the file")
	}

	r = e.set("alice", "ContactCard/set", map[string]any{"accountId": e.alice, "update": map[string]any{
		id:                         map[string]any{"uid": "other"},
		"nope":                     map[string]any{"notes": nil},
		cards["davx5-contact.vcf"]: map[string]any{"emails/e1/nope/deeper": 1},
	}})
	for uid, want := range map[string]string{id: SetInvalidProperties, "nope": SetNotFound, cards["davx5-contact.vcf"]: SetInvalidPatch} {
		if got := setErrType(r, "notUpdated", uid); got != want {
			t.Errorf("%s: %q, want %q", uid, got, want)
		}
	}

	// ifInState.
	r = e.api("alice", call("ContactCard/set", map[string]any{"accountId": e.alice, "ifInState": "stale", "destroy": []string{id}}, "0"))[0]
	if errType(r) != ErrStateMismatch {
		t.Fatalf("%+v", r)
	}
}

func TestContactCardUpdateRetriesAfterDAVWrite(t *testing.T) {
	e, book, cards := contactsEnv(t)
	id := cards["thunderbird-contact.vcf"]
	raw, _ := os.ReadFile("../jscontact/testdata/thunderbird-contact.vcf")
	once := true
	testHookBeforeWrite = func() {
		if once { // a DAV client adds a note right before JMAP writes
			once = false
			book.Put("thunderbird-contact.vcf", []byte(strings.Replace(string(raw), "END:VCARD", "X-DAV-EDIT:yes\r\nEND:VCARD", 1)), store.Cond{})
		}
	}
	defer func() { testHookBeforeWrite = nil }()
	e.set("alice", "ContactCard/set", map[string]any{"accountId": e.alice, "update": map[string]any{id: map[string]any{"name/full": "Erika M."}}})
	vcf := e.file("alice", id)
	if !strings.Contains(vcf, "X-DAV-EDIT:yes") || !strings.Contains(vcf, "FN:Erika M.") {
		t.Fatalf("lost one of the writes:\n%s", vcf)
	}
}

func TestContactCardMoveAndDestroy(t *testing.T) {
	e, book, cards := contactsEnv(t)
	other, _ := e.st.CreateCollection("alice", store.AddressBooks, "other", store.Meta{})
	id := cards["davx5-contact.vcf"]
	e.set("alice", "ContactCard/set", map[string]any{"accountId": e.alice, "update": map[string]any{
		id: map[string]any{"addressBookIds": map[string]any{other.ID(): true}, "notes": map[string]any{"n1": map[string]any{"note": "moved"}}},
	}})
	r := e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.alice, "ids": []string{id}, "properties": []string{"addressBookIds", "notes"}}, "0"))[0]
	card := r.Args["list"].([]any)[0].(map[string]any)
	if card["addressBookIds"].(map[string]any)[other.ID()] != true || card["notes"] == nil {
		t.Fatalf("after move %+v", card)
	}
	if _, err := book.Stat("davx5-contact.vcf"); err == nil {
		t.Fatal("still in the old book")
	}
	r = e.set("alice", "ContactCard/set", map[string]any{"accountId": e.alice, "destroy": []string{id, "nope"}})
	if len(r.Args["destroyed"].([]any)) != 1 || setErrType(r, "notDestroyed", "nope") != SetNotFound {
		t.Fatalf("%+v", r.Args)
	}
	if len(other.List()) != 0 {
		t.Fatal("file not deleted")
	}
}

func TestContactCardShared(t *testing.T) {
	e := newEnv(t)
	rw, _ := e.st.CreateCollection("bob", store.AddressBooks, "rw", store.Meta{})
	ro, _ := e.st.CreateCollection("bob", store.AddressBooks, "ro", store.Meta{})
	rw.SetShares([]store.Share{{User: "alice", Access: store.AccessWrite}})
	ro.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})
	raw, _ := os.ReadFile("../jscontact/testdata/apple-contact.vcf")
	ro.Put("a.vcf", raw, store.Cond{})
	log, _ := e.st.Account("bob", store.AddressBooks)
	roID, _ := log.ObjectID(ro, "a.vcf")

	r := e.set("alice", "ContactCard/set", map[string]any{"accountId": e.bob, "create": map[string]any{
		"ok": map[string]any{"addressBookIds": map[string]any{rw.ID(): true}, "name": map[string]any{"full": "Shared"}},
		"no": map[string]any{"addressBookIds": map[string]any{ro.ID(): true}, "name": map[string]any{"full": "Nope"}},
	}, "destroy": []string{roID}})
	if setErrType(r, "notCreated", "no") != SetForbidden || setErrType(r, "notDestroyed", roID) != SetForbidden || r.Args["created"].(map[string]any)["ok"] == nil {
		t.Fatalf("%+v", r.Args)
	}

	// Copy bob's read-only card into alice's own book; destroying the
	// original fails (read-only), reported in the implicit /set.
	e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.alice}, "0"))
	mine, _ := e.st.Collection("alice", store.AddressBooks, "contacts")
	rs := e.api("alice", call("ContactCard/copy", map[string]any{"fromAccountId": e.bob, "accountId": e.alice, "onSuccessDestroyOriginal": true,
		"create": map[string]any{"k": map[string]any{"id": roID, "addressBookIds": map[string]any{mine.ID(): true}, "keywords": map[string]any{"copied": true}}}}, "c"))
	if len(rs) != 2 || rs[0].Name != "ContactCard/copy" || rs[1].Name != "ContactCard/set" || rs[1].ID != "c" {
		t.Fatalf("%+v", rs)
	}
	newID := rs[0].Args["created"].(map[string]any)["k"].(map[string]any)["id"].(string)
	vcf := e.file("alice", newID)
	if !strings.Contains(vcf, "Jane Doe") || !strings.Contains(vcf, "CATEGORIES:copied") || !strings.Contains(vcf, "item1.X-ABLabel:_$!<Work>!$_") {
		t.Fatalf("copy:\n%s", vcf)
	}
	if setErrType(rs[1], "notDestroyed", roID) != SetForbidden {
		t.Fatalf("implicit destroy %+v", rs[1].Args)
	}
	r = e.api("alice", call("ContactCard/copy", map[string]any{"fromAccountId": e.alice, "accountId": e.alice, "create": map[string]any{}}, "c"))[0]
	if errType(r) != ErrInvalidArguments {
		t.Fatalf("same account: %+v", r)
	}
}

func TestAddressBookSet(t *testing.T) {
	e, book, _ := contactsEnv(t)
	rs := e.api("alice",
		call("AddressBook/set", map[string]any{"accountId": e.alice, "create": map[string]any{
			"b": map[string]any{"name": "Work", "description": "colleagues", "sortOrder": 3, "isSubscribed": false}}}, "0"),
		call("ContactCard/set", map[string]any{"accountId": e.alice, "create": map[string]any{
			"c": map[string]any{"addressBookIds": map[string]any{"#b": true}, "name": map[string]any{"full": "Colleague"}}}}, "1"))
	bid := rs[0].Args["created"].(map[string]any)["b"].(map[string]any)["id"].(string)
	if rs[1].Args["created"] == nil {
		t.Fatalf("card in new book: %+v", rs[1].Args)
	}
	ab := e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.alice, "ids": []string{bid}}, "0"))[0].Args["list"].([]any)[0].(map[string]any)
	if ab["name"] != "Work" || ab["description"] != "colleagues" || ab["sortOrder"] != float64(3) || ab["isSubscribed"] != false || ab["isDefault"] != false {
		t.Fatalf("%+v", ab)
	}

	r := e.set("alice", "AddressBook/set", map[string]any{"accountId": e.alice,
		"update":  map[string]any{bid: map[string]any{"name": "Office", "isSubscribed": true}, book.ID(): map[string]any{"myRights/mayDelete": false}},
		"destroy": []string{bid}})
	if setErrType(r, "notUpdated", book.ID()) != SetInvalidProperties || setErrType(r, "notDestroyed", bid) != SetAddressBookHasContents {
		t.Fatalf("%+v", r.Args)
	}
	if m := e.st.All(); len(m) == 0 {
		t.Fatal()
	}
	r = e.set("alice", "AddressBook/set", map[string]any{"accountId": e.alice, "destroy": []string{bid}, "onDestroyRemoveContents": true})
	if len(r.Args["destroyed"].([]any)) != 1 {
		t.Fatalf("%+v", r.Args)
	}
	r = e.api("alice", call("AddressBook/set", map[string]any{"accountId": e.alice, "onSuccessSetIsDefault": "x"}, "0"))[0]
	if errType(r) != ErrInvalidArguments {
		t.Fatalf("%+v", r)
	}
	r = e.set("alice", "AddressBook/set", map[string]any{"accountId": e.alice, "create": map[string]any{
		"shared": map[string]any{"name": "S", "shareWith": map[string]any{"x": map[string]any{"mayRead": true}}},
		"noname": map[string]any{"description": "d"},
	}})
	if setErrType(r, "notCreated", "shared") != SetInvalidProperties || setErrType(r, "notCreated", "noname") != SetInvalidProperties {
		t.Fatalf("%+v", r.Args)
	}
}

func TestAddressBookSetShared(t *testing.T) {
	e := newEnv(t)
	team, _ := e.st.CreateCollection("bob", store.AddressBooks, "team", store.Meta{DisplayName: "Team"})
	team.SetShares([]store.Share{{User: "alice", Access: store.AccessWrite}})
	r := e.set("alice", "AddressBook/set", map[string]any{"accountId": e.bob,
		"create":  map[string]any{"x": map[string]any{"name": "Mine"}},
		"update":  map[string]any{team.ID(): map[string]any{"name": "Bob's team", "isSubscribed": false}},
		"destroy": []string{team.ID()}})
	if setErrType(r, "notCreated", "x") != SetForbidden || setErrType(r, "notDestroyed", team.ID()) != SetForbidden {
		t.Fatalf("%+v", r.Args)
	}
	if p := e.st.SharePrefs("alice", team); p.DisplayName != "Bob's team" || !p.Hidden {
		t.Fatalf("prefs %+v", p)
	}
	if team.Meta().DisplayName != "Team" {
		t.Fatal("sharee renamed the owner's book")
	}
	r = e.set("alice", "AddressBook/set", map[string]any{"accountId": e.bob, "update": map[string]any{team.ID(): map[string]any{"description": "x"}}})
	if setErrType(r, "notUpdated", team.ID()) != SetForbidden {
		t.Fatalf("%+v", r.Args)
	}
}
