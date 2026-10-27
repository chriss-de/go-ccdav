package jmap

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/store"
)

// contactsEnv gives alice her default address book with the jscontact
// test cards in it.
func contactsEnv(t *testing.T) (*env, *store.Collection, map[string]string) {
	t.Helper()
	e := newEnv(t)
	e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.alice}, "0")) // creates the home
	book, err := e.st.Collection("alice", store.AddressBooks, "contacts")
	if err != nil {
		t.Fatal(err)
	}
	cards := map[string]string{} // file name -> JMAP id
	files, _ := filepath.Glob("../jscontact/testdata/*.vcf")
	log, _ := e.st.Account("alice", store.AddressBooks)
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		name := filepath.Base(f)
		if _, _, err := book.Put(name, raw, store.Cond{}); err != nil {
			t.Fatal(err)
		}
		cards[name], _ = log.ObjectID(book, name)
	}
	return e, book, cards
}

func (e *env) query(user string, args map[string]any) apiResp {
	e.t.Helper()
	if _, ok := args["accountId"]; !ok {
		args["accountId"] = e.alice
	}
	return e.api(user, call("ContactCard/query", args, "q"))[0]
}

func TestContactsSession(t *testing.T) {
	e := newEnv(t)
	cal, _ := e.st.CreateCollection("bob", store.AddressBooks, "b", store.Meta{})
	cal.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})
	s := decode[session](t, e.do("alice", "GET", "/jmap/session", "", nil))
	if _, ok := s.Capabilities[CapContacts]; !ok || s.PrimaryAccounts[CapContacts] != e.alice {
		t.Fatalf("%+v", s)
	}
	own := s.Accounts[e.alice].AccountCapabilities[CapContacts].(map[string]any)
	shared := s.Accounts[e.bob].AccountCapabilities[CapContacts].(map[string]any)
	if own["mayCreateAddressBook"] != true || shared["mayCreateAddressBook"] != false || own["maxAddressBooksPerCard"] != float64(1) {
		t.Fatalf("own %v shared %v", own, shared)
	}
}

func TestAddressBookGet(t *testing.T) {
	e, book, _ := contactsEnv(t)
	r := e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.alice, "ids": []string{book.ID(), "nope"}}, "0"))[0]
	list := r.Args["list"].([]any)
	if len(list) != 1 || !slices.Equal(ids(r.Args["notFound"]), []string{"nope"}) {
		t.Fatalf("%+v", r.Args)
	}
	ab := list[0].(map[string]any)
	rights := ab["myRights"].(map[string]any)
	if ab["name"] != "Contacts" || ab["isDefault"] != true || ab["isSubscribed"] != true || len(ab["shareWith"].(map[string]any)) != 0 ||
		rights["mayWrite"] != true || rights["mayDelete"] != true {
		t.Fatalf("%+v", ab)
	}

	// A read-only (directory) address book.
	dir, _ := e.st.CreateCollection("alice", store.AddressBooks, "ccdav-directory", store.Meta{DisplayName: "Directory", ReadOnly: true})
	r = e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.alice, "ids": []string{dir.ID()}, "properties": []string{"myRights"}}, "0"))[0]
	ab = r.Args["list"].([]any)[0].(map[string]any)
	if rights := ab["myRights"].(map[string]any); rights["mayWrite"] != false || rights["mayDelete"] != false || ab["name"] != nil {
		t.Fatalf("directory %+v", ab)
	}
}

func TestContactCardGet(t *testing.T) {
	e, book, cards := contactsEnv(t)
	r := e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.alice, "ids": nil}, "0"))[0]
	if n := len(r.Args["list"].([]any)); n != len(cards) {
		t.Fatalf("got %d cards, want %d", n, len(cards))
	}
	id := cards["apple-contact.vcf"]
	r = e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.alice, "ids": []string{id, "missing", "#unknown"}}, "0"))[0]
	card := r.Args["list"].([]any)[0].(map[string]any)
	name := card["name"].(map[string]any)
	if card["id"] != id || card["@type"] != "Card" || name["full"] != "Jane Doe" ||
		card["addressBookIds"].(map[string]any)[book.ID()] != true || card["updated"] != "2026-10-23T20:00:00Z" {
		t.Fatalf("%+v", card)
	}
	if c, err := time.Parse(time.RFC3339, card["created"].(string)); err != nil || time.Since(c) > time.Minute {
		t.Fatalf("created %v", card["created"])
	}
	if got := ids(r.Args["notFound"]); !slices.Equal(got, []string{"#unknown", "missing"}) {
		t.Fatalf("notFound %v", got)
	}
	r = e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.alice, "ids": []string{id}, "properties": []string{"uid"}}, "0"))[0]
	if card := r.Args["list"].([]any)[0].(map[string]any); len(card) != 2 || card["uid"] == nil {
		t.Fatalf("properties %+v", card)
	}

	// A broken file is left out, not an error.
	book.Put("broken.vcf", []byte("BEGIN:VCARD\r\nnonsense\r\n"), store.Cond{})
	r = e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.alice}, "0"))[0]
	if r.Name == "error" || len(r.Args["list"].([]any)) != len(cards) {
		t.Fatalf("broken card: %+v", r)
	}
}

func TestContactCardChanges(t *testing.T) {
	e, book, cards := contactsEnv(t)
	r := e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.alice, "ids": []string{}}, "0"))[0]
	state := r.Args["state"].(string)
	// DAV clients change the files; JMAP sees it.
	book.Delete("davx5-contact.vcf", store.Cond{})
	raw, _ := os.ReadFile("../jscontact/testdata/thunderbird-contact.vcf")
	book.Put("thunderbird-contact.vcf", []byte(strings.Replace(string(raw), "Erika", "Erica", 1)), store.Cond{})
	r = e.api("alice", call("ContactCard/changes", map[string]any{"accountId": e.alice, "sinceState": state}, "0"))[0]
	if !slices.Equal(ids(r.Args["destroyed"]), []string{cards["davx5-contact.vcf"]}) ||
		!slices.Equal(ids(r.Args["updated"]), []string{cards["thunderbird-contact.vcf"]}) {
		t.Fatalf("%+v", r.Args)
	}
	// The cache doesn't serve the old version.
	r = e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.alice, "ids": []string{cards["thunderbird-contact.vcf"]}, "properties": []string{"name"}}, "0"))[0]
	if full := r.Args["list"].([]any)[0].(map[string]any)["name"].(map[string]any)["full"]; full != "Erica Musterfrau" {
		t.Fatalf("stale card: %v", full)
	}
	r = e.api("alice", call("AddressBook/changes", map[string]any{"accountId": e.alice, "sinceState": "x"}, "0"))[0]
	if errType(r) != ErrCannotCalculateChanges {
		t.Fatalf("%+v", r)
	}
}

func TestContactCardQuery(t *testing.T) {
	e, book, cards := contactsEnv(t)
	q := func(args map[string]any) []string {
		t.Helper()
		r := e.query("alice", args)
		if r.Name == "error" {
			t.Fatalf("%v: %+v", args, r.Args)
		}
		var out []string
		for _, id := range r.Args["ids"].([]any) {
			for name, i := range cards {
				if i == id {
					out = append(out, strings.TrimSuffix(name, ".vcf"))
				}
			}
		}
		return out
	}
	filter := func(f map[string]any) []string {
		got := q(map[string]any{"filter": f})
		slices.Sort(got)
		return got
	}
	for _, tc := range []struct {
		f    map[string]any
		want string
	}{
		{map[string]any{"text": "acme"}, "apple-contact"},
		{map[string]any{"email": "VIAGENIE"}, "vcard4-full"},
		{map[string]any{"name/given": "max"}, "davx5-contact"},
		{map[string]any{"name/surname": "musterfrau"}, "thunderbird-contact"},
		{map[string]any{"phone": "555 0101"}, "apple-full"},
		{map[string]any{"organization": "engineering"}, "apple-full"},
		{map[string]any{"nickname": "johnny"}, "apple-full"},
		{map[string]any{"address": "cupertino"}, "apple-full"},
		{map[string]any{"note": "line two"}, "thunderbird-contact"},
		{map[string]any{"onlineService": "mastodon"}, "vcard4-full"},
		{map[string]any{"kind": "group"}, "apple-group"},
		{map[string]any{"hasMember": "F1A2B3C4-0000-4000-8000-000000000001"}, "apple-group"},
		{map[string]any{"uid": "3b8e1c1e-7f0e-4f3e-9b55-2a1f9c0d1e2f"}, "davx5-contact"},
		{map[string]any{"updatedBefore": "2026-10-24T00:00:00Z"}, "apple-contact,apple-full,apple-group,davx5-contact"}, // REV; thunderbird has none: file time
		{map[string]any{"operator": "AND", "conditions": []any{map[string]any{"text": "fam"}, map[string]any{"kind": "group"}}}, "apple-group"},
		{map[string]any{"operator": "OR", "conditions": []any{map[string]any{"uid": "3b8e1c1e-7f0e-4f3e-9b55-2a1f9c0d1e2f"}, map[string]any{"kind": "group"}}}, "apple-group,davx5-contact"},
		{map[string]any{"operator": "NOT", "conditions": []any{map[string]any{"kind": "individual"}}}, "apple-group"},
	} {
		if got := strings.Join(filter(tc.f), ","); got != tc.want {
			t.Errorf("%v: %s, want %s", tc.f, got, tc.want)
		}
	}
	if got := filter(map[string]any{"inAddressBook": book.ID()}); len(got) != len(cards) {
		t.Errorf("inAddressBook: %v", got)
	}
	if got := filter(map[string]any{"createdAfter": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}); len(got) != 0 {
		t.Errorf("createdAfter future: %v", got)
	}

	// Sorting: by surname, descending; default by full name.
	sorted := q(map[string]any{"sort": []any{map[string]any{"property": "name/surname", "isAscending": false}}, "filter": map[string]any{"kind": "individual"}})
	if strings.Join(sorted, ",") != "vcard4-full,davx5-contact,thunderbird-contact,apple-contact,apple-full" {
		t.Errorf("by surname desc: %v", sorted)
	}
	if got := q(map[string]any{}); got[0] != "apple-full" { // "Dr. John ..."
		t.Errorf("default order: %v", got)
	}

	// Paging and totals.
	r := e.query("alice", map[string]any{"position": 1, "limit": 2, "calculateTotal": true})
	if len(r.Args["ids"].([]any)) != 2 || r.Args["total"] != float64(len(cards)) || r.Args["position"] != float64(1) ||
		r.Args["canCalculateChanges"] != false || r.Args["queryState"] == "" {
		t.Errorf("paging %+v", r.Args)
	}

	for _, tc := range []struct {
		args map[string]any
		err  string
	}{
		{map[string]any{"filter": map[string]any{"bogus": "x"}}, ErrUnsupportedFilter},
		{map[string]any{"filter": map[string]any{"text": 3}}, ErrInvalidArguments},
		{map[string]any{"filter": map[string]any{"operator": "XOR", "conditions": []any{}}}, ErrInvalidArguments},
		{map[string]any{"sort": []any{map[string]any{"property": "email"}}}, ErrUnsupportedSort},
		{map[string]any{"sort": []any{map[string]any{"property": "created", "collation": "i;klingon"}}}, ErrUnsupportedSort},
		{map[string]any{"anchor": "nope"}, ErrAnchorNotFound},
		{map[string]any{"accountId": e.bob}, ErrAccountNotFound},
	} {
		if r := e.query("alice", tc.args); errType(r) != tc.err {
			t.Errorf("%v: %+v, want %s", tc.args, r, tc.err)
		}
	}
	r = e.api("alice", call("ContactCard/queryChanges", map[string]any{"accountId": e.alice, "sinceQueryState": "x"}, "0"))[0]
	if errType(r) != ErrCannotCalculateChanges {
		t.Errorf("queryChanges %+v", r)
	}
}

func TestContactsShared(t *testing.T) {
	e := newEnv(t)
	shared, _ := e.st.CreateCollection("bob", store.AddressBooks, "team", store.Meta{DisplayName: "Team"})
	private, _ := e.st.CreateCollection("bob", store.AddressBooks, "private", store.Meta{})
	raw, _ := os.ReadFile("../jscontact/testdata/apple-contact.vcf")
	shared.Put("a.vcf", raw, store.Cond{})
	raw, _ = os.ReadFile("../jscontact/testdata/davx5-contact.vcf")
	private.Put("b.vcf", raw, store.Cond{})
	shared.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})

	r := e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.bob}, "0"))[0]
	list := r.Args["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("alice sees %d of bob's books", len(list))
	}
	ab := list[0].(map[string]any)
	rights := ab["myRights"].(map[string]any)
	if ab["name"] != "Team" || ab["isDefault"] != false || rights["mayWrite"] != false || rights["mayShare"] != false {
		t.Fatalf("%+v", ab)
	}
	r = e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.bob}, "0"))[0]
	if l := r.Args["list"].([]any); len(l) != 1 || l[0].(map[string]any)["uid"] != "F1A2B3C4-0000-4000-8000-000000000001" {
		t.Fatalf("cards %+v", l)
	}
	log, _ := e.st.Account("bob", store.AddressBooks)
	pid, _ := log.ObjectID(private, "b.vcf")
	r = e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.bob, "ids": []string{pid}}, "0"))[0]
	if !slices.Equal(ids(r.Args["notFound"]), []string{pid}) {
		t.Fatalf("private card visible: %+v", r.Args)
	}

	// Alice renames and hides it for herself: her AddressBook state moves
	// on (a resync), and she sees her own settings.
	state := e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.bob, "ids": []string{}}, "0"))[0].Args["state"].(string)
	e.st.UpdateSharePrefs("alice", shared, func(p *store.SharePrefs) { p.DisplayName = "Bob's team"; p.Hidden = true })
	r = e.api("alice", call("AddressBook/changes", map[string]any{"accountId": e.bob, "sinceState": state}, "0"))[0]
	if errType(r) != ErrCannotCalculateChanges {
		t.Fatalf("prefs change: %+v", r)
	}
	ab = e.api("alice", call("AddressBook/get", map[string]any{"accountId": e.bob}, "0"))[0].Args["list"].([]any)[0].(map[string]any)
	if ab["name"] != "Bob's team" || ab["isSubscribed"] != false {
		t.Fatalf("prefs %+v", ab)
	}
	// Bob still sees his own name.
	ab = e.api("bob", call("AddressBook/get", map[string]any{"accountId": e.bob, "ids": []string{shared.ID()}}, "0"))[0].Args["list"].([]any)[0].(map[string]any)
	if ab["name"] != "Team" || ab["isSubscribed"] != true {
		t.Fatalf("owner view %+v", ab)
	}
}

// Filter depth was limited, breadth not: each condition is checked against
// every object, so a huge OR multiplied the cost of a query.
func TestFilterSizeLimited(t *testing.T) {
	e := newEnv(t)
	conds := make([]any, maxFilterNodes)
	for i := range conds {
		conds[i] = map[string]any{"text": "x"}
	}
	r := e.api("alice", call("ContactCard/query", map[string]any{"accountId": e.alice,
		"filter": map[string]any{"operator": "OR", "conditions": conds}}, "0"))[0]
	if errType(r) != ErrUnsupportedFilter {
		t.Fatalf("filter of %d nodes: %.200v", len(conds)+1, r)
	}
	r = e.api("alice", call("ContactCard/query", map[string]any{"accountId": e.alice,
		"filter": map[string]any{"operator": "OR", "conditions": conds[:10]}}, "0"))[0]
	if errType(r) != "" {
		t.Fatalf("small filter: %.200v", r)
	}
}
