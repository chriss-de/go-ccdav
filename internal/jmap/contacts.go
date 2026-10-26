package jmap

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/jscontact"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// CapContacts is JMAP for Contacts (RFC 9610).
const CapContacts = "urn:ietf:params:jmap:contacts"

var contactType = &typeDef{name: "ContactCard", kind: store.AddressBooks, codec: jscontact.Codec{}, collProp: "addressBookIds"}

// maxQueryLimit caps the ids of one /query response.
const maxQueryLimit = 1000

func (h *Handler) registerContacts() {
	h.AddCapability(CapContacts, struct{}{}, func(a account) any {
		// A vCard file lives in exactly one address book.
		return map[string]any{"maxAddressBooksPerCard": 1, "mayCreateAddressBook": a.personal}
	})
	h.Register("AddressBook/get", CapContacts, func(c *Call, args json.RawMessage) (any, error) {
		return c.addressBookGet(args)
	})
	h.Register("AddressBook/changes", CapContacts, func(c *Call, args json.RawMessage) (any, error) {
		return c.Changes(args, store.AddressBooks, store.LevelCollections)
	})
	h.Register("ContactCard/get", CapContacts, func(c *Call, args json.RawMessage) (any, error) {
		return c.getObjects(args, contactType)
	})
	h.Register("ContactCard/changes", CapContacts, func(c *Call, args json.RawMessage) (any, error) {
		return c.Changes(args, store.AddressBooks, store.LevelObjects)
	})
	h.Register("ContactCard/query", CapContacts, func(c *Call, args json.RawMessage) (any, error) {
		return c.contactQuery(args)
	})
	h.Register("ContactCard/queryChanges", CapContacts, func(*Call, json.RawMessage) (any, error) {
		return nil, Errorf(ErrCannotCalculateChanges, "query changes are not supported; query again")
	})
}

func (c *Call) addressBookGet(raw json.RawMessage) (any, error) {
	var args GetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := CheckGet(args); err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, store.AddressBooks)
	if err != nil {
		return nil, err
	}
	resp := &GetResponse{AccountID: args.AccountID, State: v.State(store.LevelCollections), List: []any{}, NotFound: []string{}}
	ids := slices.Sorted(maps.Keys(v.Colls))
	if args.IDs != nil {
		ids = *args.IDs
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		col := v.Colls[id]
		if col == nil {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		resp.List = append(resp.List, Select(c.addressBook(v, col), args.Properties))
	}
	return resp, nil
}

// addressBook is the AddressBook object of a collection, as the caller
// sees it: someone it is shared with sees their own name and order, and
// isSubscribed is false when they hid it.
func (c *Call) addressBook(v *View, col *store.Collection) map[string]any {
	m := col.Meta()
	name, order, subscribed := m.DisplayName, m.Order, true
	if name == "" {
		name = col.Name
	}
	if !v.Own {
		p := c.h.store.SharePrefs(c.User, col.Key())
		if p.DisplayName != "" {
			name = p.DisplayName
		}
		if p.Order != "" {
			order = p.Order
		}
		subscribed = !p.Hidden
	}
	sortOrder, err := strconv.Atoi(order)
	if err != nil || sortOrder < 0 {
		sortOrder = 0
	}
	var desc any
	if m.Description != "" {
		desc = m.Description
	}
	id := col.ID()
	acc := v.Access[id]
	writable := !m.ReadOnly && (acc == store.AccessOwner || acc == store.AccessWrite)
	owner := !m.ReadOnly && acc == store.AccessOwner
	return map[string]any{
		"id":           id,
		"name":         name,
		"description":  desc,
		"sortOrder":    sortOrder,
		"isDefault":    col.Name == c.h.defaultName(store.AddressBooks),
		"isSubscribed": subscribed,
		"shareWith":    nil, // needs Principal objects
		"myRights": map[string]any{
			"mayRead": true, "mayWrite": writable, "mayShare": owner, "mayDelete": owner,
		},
	}
}

// defaultName is the name of the default collection of a kind: the
// first of the defaults a new home starts with.
func (h *Handler) defaultName(kind store.Kind) string {
	names := slices.Sorted(maps.Keys(h.defaults[kind]))
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// Filter conditions and sort properties of ContactCard/query (RFC 9610
// §3.3).
var (
	contactConds = map[string]bool{
		"inAddressBook": true, "uid": true, "hasMember": true, "kind": true,
		"createdBefore": true, "createdAfter": true, "updatedBefore": true, "updatedAfter": true,
		"text": true, "name": true, "name/given": true, "name/surname": true, "name/surname2": true,
		"nickname": true, "nickName": true, "organization": true, "email": true, "phone": true,
		"onlineService": true, "address": true, "note": true,
	}
	contactSorts = map[string]bool{
		"created": true, "updated": true, "name/given": true, "name/surname": true, "name/surname2": true,
	}
)

func (c *Call) contactQuery(raw json.RawMessage) (any, error) {
	var args QueryArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, store.AddressBooks)
	if err != nil {
		return nil, err
	}
	f, err := parseFilter(args.Filter, contactConds)
	if err != nil {
		return nil, err
	}
	sorts, err := parseSort(args.Sort, contactSorts)
	if err != nil {
		return nil, err
	}
	state := v.State(store.LevelObjects)
	var hits []*entry
	for _, e := range c.h.all(contactType, v) {
		ok, err := f.eval(func(cond map[string]json.RawMessage) (bool, error) { return matchContact(e, cond) })
		if err != nil {
			return nil, err
		}
		if ok {
			hits = append(hits, e)
		}
	}
	slices.SortStableFunc(hits, func(a, b *entry) int { return compareContacts(a, b, sorts) })
	ids := make([]string, len(hits))
	for i, e := range hits {
		ids[i] = e.ref.ID
	}
	resp, err := Page(ids, args, maxQueryLimit)
	if err != nil {
		return nil, err
	}
	resp.QueryState = state
	return resp, nil
}

// matchContact checks one FilterCondition: all its properties must match.
func matchContact(e *entry, cond map[string]json.RawMessage) (bool, error) {
	for k, raw := range cond {
		var want string
		if err := json.Unmarshal(raw, &want); err != nil {
			return false, Errorf(ErrInvalidArguments, "filter %s: want a string", k)
		}
		var ok bool
		switch k {
		case "inAddressBook":
			ok = e.ref.Coll == want
		case "uid":
			ok = e.obj["uid"] == want
		case "hasMember":
			m, _ := e.obj["members"].(map[string]any)
			ok = m[want] == true
		case "kind":
			kind, _ := e.obj["kind"].(string)
			if kind == "" {
				kind = "individual"
			}
			ok = kind == want
		case "createdBefore", "createdAfter", "updatedBefore", "updatedAfter":
			t, err := time.Parse(time.RFC3339, want)
			if err != nil {
				return false, Errorf(ErrInvalidArguments, "filter %s: %v", k, err)
			}
			prop := "created"
			if strings.HasPrefix(k, "updated") {
				prop = "updated"
			}
			s, _ := e.obj[prop].(string)
			have, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return false, nil
			}
			if strings.HasSuffix(k, "Before") {
				ok = have.Before(t)
			} else {
				ok = !have.Before(t)
			}
		case "name/given", "name/surname", "name/surname2":
			ok = anyContains(nameComponents(e.obj, strings.TrimPrefix(k, "name/")), want)
		default:
			ok = anyContains(contactTexts(e.obj, k), want)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// textProps lists the card properties a text condition searches.
var textProps = map[string][]string{
	"text":          {"name", "nicknames", "organizations", "titles", "emails", "phones", "onlineServices", "addresses", "notes"},
	"name":          {"name"},
	"nickname":      {"nicknames"},
	"nickName":      {"nicknames"},
	"organization":  {"organizations"},
	"email":         {"emails"},
	"phone":         {"phones"},
	"onlineService": {"onlineServices"},
	"address":       {"addresses"},
	"note":          {"notes"},
}

// contactTexts collects the string values of the properties a condition
// searches, leaving out structural values like kinds and contexts.
func contactTexts(card map[string]any, cond string) []string {
	var out []string
	for _, p := range textProps[cond] {
		collectStrings(card[p], &out)
	}
	return out
}

var structural = map[string]bool{"@type": true, "kind": true, "contexts": true, "features": true, "vCardParams": true, "mediaType": true, "pref": true}

func collectStrings(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case []any:
		for _, item := range x {
			collectStrings(item, out)
		}
	case map[string]any:
		for k, item := range x {
			if !structural[k] {
				collectStrings(item, out)
			}
		}
	}
}

func anyContains(vals []string, needle string) bool {
	return slices.ContainsFunc(vals, func(s string) bool { return contains(s, needle) })
}

// nameComponents returns the values of name components of one kind.
func nameComponents(card map[string]any, kind string) []string {
	name, _ := card["name"].(map[string]any)
	comps, _ := name["components"].([]any)
	var out []string
	for _, c := range comps {
		if m, ok := c.(map[string]any); ok && m["kind"] == kind {
			if s, ok := m["value"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// sortName is the name used when no sort is given: the full name, or
// the components in order.
func sortName(card map[string]any) string {
	name, _ := card["name"].(map[string]any)
	if full, ok := name["full"].(string); ok && full != "" {
		return strings.ToLower(full)
	}
	var parts []string
	for _, k := range []string{"given", "surname"} {
		parts = append(parts, nameComponents(card, k)...)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// compareContacts orders by the sort criteria, then by name and id.
func compareContacts(a, b *entry, sorts []comparator) int {
	for _, s := range sorts {
		var x, y string
		switch s.Property {
		case "created", "updated":
			x, _ = a.obj[s.Property].(string)
			y, _ = b.obj[s.Property].(string)
		default:
			kind := strings.TrimPrefix(s.Property, "name/")
			x = strings.ToLower(strings.Join(nameComponents(a.obj, kind), " "))
			y = strings.ToLower(strings.Join(nameComponents(b.obj, kind), " "))
		}
		if d := strings.Compare(x, y); d != 0 {
			if !s.ascending() {
				d = -d
			}
			return d
		}
	}
	if d := strings.Compare(sortName(a.obj), sortName(b.obj)); d != 0 {
		return d
	}
	return strings.Compare(a.ref.ID, b.ref.ID)
}
