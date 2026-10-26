package jmap

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/chriss-de/go-ccdav/internal/carddav"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/jscalendar"
	"github.com/chriss-de/go-ccdav/internal/jscontact"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// newCardVersion is the vCard version of cards created via JMAP: the one
// every CardDAV client (Apple's in particular) reads reliably.
const newCardVersion = "3.0"

// testHookBeforeWrite runs in updateCard between reading and writing.
var testHookBeforeWrite func()

// maxWriteRetries bounds how often an update is re-applied when a DAV
// client changed the file in between.
const maxWriteRetries = 3

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// asSetError turns an error of a single create/update/destroy into the
// SetError to report; other errors are the method's.
func asSetError(err error) (*SetError, bool) {
	var se *setErr
	if errors.As(err, &se) {
		return &se.SetError, true
	}
	var inv *jscontact.InvalidError
	if errors.As(err, &inv) {
		return &SetError{Type: SetInvalidProperties, Properties: inv.Paths}, true
	}
	var cinv *jscalendar.InvalidError
	if errors.As(err, &cinv) {
		return &SetError{Type: SetInvalidProperties, Properties: cinv.Paths}, true
	}
	return nil, false
}

// copyJSON deep-copies a JSON-like value, so patches never touch cached
// conversions.
func copyJSON(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

// writable reports whether the caller may change objects in collection
// id: not found if they can't see it, forbidden if read-only for them.
func (v *View) writable(id string) error {
	col := v.Colls[id]
	if col == nil {
		return failSet(SetNotFound, "no such address book "+id)
	}
	acc := v.Access[id]
	if col.ReadOnly() || acc != store.AccessOwner && acc != store.AccessWrite {
		return failSet(SetForbidden, "read-only")
	}
	return nil
}

// targetBook reads addressBookIds: exactly one address book, resolving
// creation ids, writable by the caller.
func (c *Call) targetBook(v *View, raw any) (*store.Collection, error) {
	return c.target(v, raw, "addressBookIds")
}

// target reads a collection map (addressBookIds, calendarIds): exactly
// one collection, resolving creation ids, writable by the caller.
func (c *Call) target(v *View, raw any, prop string) (*store.Collection, error) {
	m, ok := raw.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, failSet(SetInvalidProperties, prop+" must name exactly one collection", prop)
	}
	for id, val := range m {
		real, ok := c.ResolveID(id)
		if val != true || !ok || v.Colls[real] == nil {
			return nil, failSet(SetInvalidProperties, "unknown collection "+id, prop)
		}
		if err := v.writable(real); err != nil {
			return nil, err
		}
		return v.Colls[real], nil
	}
	return nil, nil
}

// storeCard validates and writes a vCard; a UID taken by another object
// of the collection is alreadyExists.
func (c *Call) storeCard(v *View, col *store.Collection, name string, card *ical.Component, cond store.Cond) error {
	data := card.Bytes()
	if _, err := carddav.Validate(data); err != nil {
		return failSet(SetInvalidProperties, err.Error())
	}
	_, _, err := col.Put(name, data, cond)
	var dup *store.UIDConflictError
	if errors.As(err, &dup) {
		id, _ := v.Log.ObjectID(col, dup.Existing)
		return &setErr{SetError{Type: SetAlreadyExists, Description: "the UID is in use", ExistingID: id}}
	}
	if errors.Is(err, store.ErrQuota) {
		return failSet(SetOverQuota, "the owner's storage quota is used up")
	}
	return err
}

// contactSet implements ContactCard/set.
func (c *Call) contactSet(raw json.RawMessage) (any, error) {
	var args SetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, store.AddressBooks)
	if err != nil {
		return nil, err
	}
	old := v.State(store.LevelObjects)
	if err := CheckSet(args, old); err != nil {
		return nil, err
	}
	resp := &SetResponse{AccountID: args.AccountID, OldState: &old}
	fail := func(m *map[string]SetError, id string, err error) error {
		se, ok := asSetError(err)
		if !ok {
			return err
		}
		if *m == nil {
			*m = map[string]SetError{}
		}
		(*m)[id] = *se
		return nil
	}
	for _, cid := range slices.Sorted(maps.Keys(args.Create)) {
		created, err := c.createCard(v, cid, args.Create[cid])
		if err != nil {
			if err := fail(&resp.NotCreated, cid, err); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[cid] = created
	}
	for _, id := range slices.Sorted(maps.Keys(args.Update)) {
		changed, err := c.updateCard(v, id, args.Update[id])
		if err != nil {
			if err := fail(&resp.NotUpdated, id, err); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Updated == nil {
			resp.Updated = map[string]any{}
		}
		resp.Updated[id] = changed
	}
	for _, id := range args.Destroy {
		if err := c.destroyCard(v, id); err != nil {
			if err := fail(&resp.NotDestroyed, id, err); err != nil {
				return nil, err
			}
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	resp.NewState = v.State(store.LevelObjects)
	return resp, nil
}

// serverSet are ContactCard properties a client may not set.
var serverSet = []string{"id", "created"}

func (c *Call) createCard(v *View, cid string, raw json.RawMessage) (map[string]any, error) {
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil || card == nil {
		return nil, failSet(SetInvalidProperties, "not an object")
	}
	for _, p := range serverSet {
		if _, ok := card[p]; ok {
			return nil, failSet(SetInvalidProperties, p+" is set by the server", p)
		}
	}
	if t, ok := card["@type"]; ok && t != "Card" {
		return nil, failSet(SetInvalidProperties, "@type must be Card", "@type")
	}
	col, err := c.targetBook(v, card["addressBookIds"])
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	uid, _ := card["uid"].(string)
	if uid == "" {
		uid = newUUID()
		card["uid"] = uid
		out["uid"] = uid
	}
	if other, ok := col.ByUID(uid); ok {
		id, _ := v.Log.ObjectID(col, other)
		return nil, &setErr{SetError{Type: SetAlreadyExists, Description: "the UID is in use", ExistingID: id}}
	}
	vc, _, err := jscontact.Merge(nil, card, newCardVersion)
	if err != nil {
		return nil, err
	}
	name := newUUID() + ".vcf"
	if err := c.storeCard(v, col, name, vc, store.Cond{IfNoneMatch: "*"}); err != nil {
		return nil, err
	}
	ref, ok := v.Log.ObjectAt(col, name)
	if !ok {
		return nil, errors.New("jmap: created card has no id")
	}
	c.Created(cid, ref.ID)
	e, ok := c.h.load(contactType, v, ref)
	if !ok {
		return nil, errors.New("jmap: cannot read the created card")
	}
	out["id"], out["created"], out["updated"] = ref.ID, e.obj["created"], e.obj["updated"]
	return out, nil
}

// immutable are ContactCard properties an update may not change.
var immutable = []string{"id", "created", "uid", "@type", "version"}

func (c *Call) updateCard(v *View, id string, raw json.RawMessage) (map[string]any, error) {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || patch == nil {
		return nil, failSet(SetInvalidPatch, "not a PatchObject")
	}
	real, ok := c.ResolveID(id)
	if !ok {
		return nil, failSet(SetNotFound, "unknown creation id")
	}
	for attempt := 0; ; attempt++ {
		ref, ok := v.Log.Lookup(real)
		if !ok || v.Colls[ref.Coll] == nil {
			return nil, failSet(SetNotFound, "no such card")
		}
		if err := v.writable(ref.Coll); err != nil {
			return nil, err
		}
		col := v.Colls[ref.Coll]
		data, info, err := col.Get(ref.Name)
		if err != nil {
			return nil, failSet(SetNotFound, "no such card")
		}
		e, ok := c.h.load(contactType, v, ref)
		if !ok {
			return nil, failSet(SetForbidden, "the stored vCard cannot be converted")
		}
		card := copyJSON(e.obj)
		if err := ApplyPatch(card, patch); err != nil {
			return nil, failSet(SetInvalidPatch, err.Error())
		}
		for _, p := range immutable {
			if !jsonEqual(card[p], e.obj[p]) {
				return nil, failSet(SetInvalidProperties, p+" cannot be changed", p)
			}
		}
		target, err := c.targetBook(v, card["addressBookIds"])
		if err != nil {
			return nil, err
		}
		prev, err := ical.ParseBytes(data)
		if err != nil {
			return nil, failSet(SetForbidden, "the stored vCard cannot be parsed")
		}
		vc, changed, err := jscontact.Merge(prev, card, "")
		if err != nil {
			return nil, err
		}
		if target != col {
			// Moving keeps the id; the content change follows in place.
			name := ref.Name
			if _, err := target.Stat(name); err == nil {
				name = newUUID() + ".vcf"
			}
			if _, err := col.Move(ref.Name, target, name, store.Cond{IfMatch: info.ETag}); err != nil {
				if errors.Is(err, store.ErrPrecondition) && attempt < maxWriteRetries {
					continue
				}
				var dup *store.UIDConflictError
				if errors.As(err, &dup) {
					other, _ := v.Log.ObjectID(target, dup.Existing)
					return nil, &setErr{SetError{Type: SetAlreadyExists, Description: "the UID is in use in the target address book", ExistingID: other}}
				}
				return nil, err
			}
			col, ref.Name = target, name
			if info, err = col.Stat(name); err != nil {
				return nil, err
			}
		}
		if !changed {
			return nil, nil
		}
		if testHookBeforeWrite != nil {
			testHookBeforeWrite()
		}
		err = c.storeCard(v, col, ref.Name, vc, store.Cond{IfMatch: info.ETag})
		if errors.Is(err, store.ErrPrecondition) && attempt < maxWriteRetries {
			continue // a DAV client wrote in between: apply the patch again
		}
		if err != nil {
			return nil, err
		}
		ref, _ = v.Log.Lookup(real)
		if e, ok := c.h.load(contactType, v, ref); ok {
			return map[string]any{"updated": e.obj["updated"]}, nil
		}
		return nil, nil
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (c *Call) destroyCard(v *View, id string) error {
	real, ok := c.ResolveID(id)
	if !ok {
		return failSet(SetNotFound, "unknown creation id")
	}
	ref, ok := v.Log.Lookup(real)
	if !ok || v.Colls[ref.Coll] == nil {
		return failSet(SetNotFound, "no such card")
	}
	if err := v.writable(ref.Coll); err != nil {
		return err
	}
	err := v.Colls[ref.Coll].Delete(ref.Name, store.Cond{})
	if errors.Is(err, store.ErrNotFound) {
		return failSet(SetNotFound, "no such card")
	}
	return err
}

type copyArgs struct {
	FromAccountID            string                     `json:"fromAccountId"`
	IfFromInState            *string                    `json:"ifFromInState"`
	AccountID                string                     `json:"accountId"`
	IfInState                *string                    `json:"ifInState"`
	Create                   map[string]json.RawMessage `json:"create"`
	OnSuccessDestroyOriginal bool                       `json:"onSuccessDestroyOriginal"`
	DestroyFromIfInState     *string                    `json:"destroyFromIfInState"`
}

type copyResponse struct {
	FromAccountID string              `json:"fromAccountId"`
	AccountID     string              `json:"accountId"`
	OldState      *string             `json:"oldState"`
	NewState      string              `json:"newState"`
	Created       map[string]any      `json:"created"`
	NotCreated    map[string]SetError `json:"notCreated"`
}

// contactCopy implements ContactCard/copy (RFC 8620 §5.4): the stored
// vCard is copied as it is, then the overrides are merged in.
func (c *Call) contactCopy(raw json.RawMessage) (any, error) {
	var args copyArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.FromAccountID == args.AccountID {
		return nil, Errorf(ErrInvalidArguments, "fromAccountId and accountId must differ; use /set to move")
	}
	from, err := c.View(args.FromAccountID, store.AddressBooks)
	if err != nil {
		var me *MethodError
		if errors.As(err, &me) && me.Type == ErrAccountNotFound {
			return nil, Errorf(ErrFromAccountNotFound, "no account %q", args.FromAccountID)
		}
		return nil, err
	}
	if args.IfFromInState != nil && *args.IfFromInState != from.State(store.LevelObjects) {
		return nil, Errorf(ErrStateMismatch, "fromAccount state differs")
	}
	to, err := c.View(args.AccountID, store.AddressBooks)
	if err != nil {
		return nil, err
	}
	old := to.State(store.LevelObjects)
	if err := CheckSet(SetArgs{IfInState: args.IfInState, Create: args.Create}, old); err != nil {
		return nil, err
	}
	resp := &copyResponse{FromAccountID: args.FromAccountID, AccountID: args.AccountID, OldState: &old}
	var copied []string
	for _, cid := range slices.Sorted(maps.Keys(args.Create)) {
		srcID, out, err := c.copyCard(from, to, cid, args.Create[cid])
		if err != nil {
			se, ok := asSetError(err)
			if !ok {
				return nil, err
			}
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[cid] = *se
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[cid] = out
		copied = append(copied, srcID)
	}
	resp.NewState = to.State(store.LevelObjects)
	if args.OnSuccessDestroyOriginal && len(copied) > 0 {
		b, _ := json.Marshal(SetArgs{AccountID: args.FromAccountID, IfInState: args.DestroyFromIfInState, Destroy: copied})
		set, err := c.contactSet(b)
		if err != nil {
			var me *MethodError
			if !errors.As(err, &me) {
				return nil, err
			}
			c.Respond("error", me)
		} else {
			c.Respond("ContactCard/set", set)
		}
	}
	return resp, nil
}

func (c *Call) copyCard(from, to *View, cid string, raw json.RawMessage) (string, map[string]any, error) {
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil || card == nil {
		return "", nil, failSet(SetInvalidProperties, "not an object")
	}
	srcID, _ := card["id"].(string)
	ref, ok := from.Log.Lookup(srcID)
	if !ok || from.Colls[ref.Coll] == nil {
		return "", nil, failSet(SetNotFound, "no such card "+srcID)
	}
	data, _, err := from.Colls[ref.Coll].Get(ref.Name)
	if err != nil {
		return "", nil, failSet(SetNotFound, "no such card "+srcID)
	}
	col, err := c.targetBook(to, card["addressBookIds"])
	if err != nil {
		return "", nil, err
	}
	prev, err := ical.ParseBytes(data)
	if err != nil {
		return "", nil, failSet(SetForbidden, "the stored vCard cannot be parsed")
	}
	vc := prev
	if overrides := withoutKeys(card, "id", "addressBookIds"); len(overrides) > 0 {
		cur, err := jscontact.FromVCard(prev)
		if err != nil {
			return "", nil, failSet(SetForbidden, "the stored vCard cannot be converted")
		}
		cur = copyJSON(cur)
		for k, val := range overrides {
			if slices.Contains(immutable, k) && !jsonEqual(cur[k], val) {
				return "", nil, failSet(SetInvalidProperties, k+" cannot be changed", k)
			}
			cur[k] = val
		}
		if vc, _, err = jscontact.Merge(prev, cur, ""); err != nil {
			return "", nil, err
		}
	}
	name := newUUID() + ".vcf"
	if err := c.storeCard(to, col, name, vc, store.Cond{IfNoneMatch: "*"}); err != nil {
		return "", nil, err
	}
	nref, ok := to.Log.ObjectAt(col, name)
	if !ok {
		return "", nil, errors.New("jmap: copied card has no id")
	}
	c.Created(cid, nref.ID)
	out := map[string]any{"id": nref.ID}
	if e, ok := c.h.load(contactType, to, nref); ok {
		out["created"], out["updated"] = e.obj["created"], e.obj["updated"]
	}
	return srcID, out, nil
}

func withoutKeys(m map[string]any, drop ...string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if !slices.Contains(drop, k) {
			out[k] = v
		}
	}
	return out
}

type addressBookSetArgs struct {
	SetArgs
	OnDestroyRemoveContents bool    `json:"onDestroyRemoveContents"`
	OnSuccessSetIsDefault   *string `json:"onSuccessSetIsDefault"`
}

// addressBookSet implements AddressBook/set. Someone a book is shared
// with changes only their own name, order and subscription.
func (c *Call) addressBookSet(raw json.RawMessage) (any, error) {
	var args addressBookSetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, store.AddressBooks)
	if err != nil {
		return nil, err
	}
	old := v.State(store.LevelCollections)
	if err := CheckSet(args.SetArgs, old); err != nil {
		return nil, err
	}
	resp := &SetResponse{AccountID: args.AccountID, OldState: &old}
	fail := func(m *map[string]SetError, id string, err error) error {
		se, ok := asSetError(err)
		if !ok {
			return err
		}
		if *m == nil {
			*m = map[string]SetError{}
		}
		(*m)[id] = *se
		return nil
	}
	for _, cid := range slices.Sorted(maps.Keys(args.Create)) {
		ab, err := c.createBook(v, cid, args.Create[cid])
		if err != nil {
			if err := fail(&resp.NotCreated, cid, err); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[cid] = ab
	}
	// Created books are in the view from now on.
	if v, err = c.View(args.AccountID, store.AddressBooks); err != nil {
		return nil, err
	}
	if args.OnSuccessSetIsDefault != nil {
		id, _ := c.ResolveID(*args.OnSuccessSetIsDefault)
		if col := v.Colls[id]; col == nil || col.Name != c.h.defaultName(store.AddressBooks) {
			return nil, Errorf(ErrInvalidArguments, "the default address book can't be changed")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(args.Update)) {
		if err := c.updateBook(v, id, args.Update[id]); err != nil {
			if err := fail(&resp.NotUpdated, id, err); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Updated == nil {
			resp.Updated = map[string]any{}
		}
		resp.Updated[id] = nil
	}
	for _, id := range args.Destroy {
		if err := c.destroyBook(v, id, args.OnDestroyRemoveContents); err != nil {
			if err := fail(&resp.NotDestroyed, id, err); err != nil {
				return nil, err
			}
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	resp.NewState = v.State(store.LevelCollections)
	return resp, nil
}

// bookChange reads the client-settable properties of an AddressBook.
type bookChange struct {
	name, description *string
	sortOrder         *int
	isSubscribed      *bool
	shares            *[]store.Share
}

func (c *Call) parseBookProps(owner string, obj, current map[string]any) (bookChange, error) {
	var ch bookChange
	for _, k := range keysWithRemoved(obj, current) {
		val := obj[k] // a property a patch removed is null
		if current != nil && jsonEqual(val, current[k]) {
			continue // unchanged, including server-set ones
		}
		bad := func() error { return failSet(SetInvalidProperties, "invalid "+k, k) }
		switch k {
		case "name":
			s, ok := val.(string)
			if !ok || s == "" {
				return ch, bad()
			}
			ch.name = &s
		case "description":
			s, ok := val.(string)
			if val != nil && !ok {
				return ch, bad()
			}
			ch.description = &s
		case "sortOrder":
			f, ok := val.(float64)
			if !ok || f < 0 || f != float64(int(f)) {
				return ch, bad()
			}
			n := int(f)
			ch.sortOrder = &n
		case "isSubscribed":
			b, ok := val.(bool)
			if !ok {
				return ch, bad()
			}
			ch.isSubscribed = &b
		case "shareWith":
			if current != nil && current[k] == nil {
				return ch, failSet(SetForbidden, "only the owner can share", k)
			}
			shares, err := parseShareWith(c.h.cfg.Get(), owner, val)
			if err != nil {
				return ch, err
			}
			ch.shares = &shares
		default:
			return ch, failSet(SetInvalidProperties, k+" cannot be set", k)
		}
	}
	return ch, nil
}

func (c *Call) createBook(v *View, cid string, raw json.RawMessage) (map[string]any, error) {
	if !v.Own {
		return nil, failSet(SetForbidden, "address books can only be created in your own account")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, failSet(SetInvalidProperties, "not an object")
	}
	ch, err := c.parseBookProps(v.Owner, obj, nil)
	if err != nil {
		return nil, err
	}
	if ch.name == nil {
		return nil, failSet(SetInvalidProperties, "name is required", "name")
	}
	m := store.Meta{DisplayName: *ch.name}
	if ch.description != nil {
		m.Description = *ch.description
	}
	if ch.sortOrder != nil {
		m.Order = strconv.Itoa(*ch.sortOrder)
	}
	if ch.isSubscribed != nil {
		m.Unsubscribed = !*ch.isSubscribed
	}
	col, err := c.h.store.CreateCollection(v.Owner, store.AddressBooks, newUUID(), m)
	if err != nil {
		return nil, err
	}
	if ch.shares != nil {
		if err := col.SetShares(*ch.shares); err != nil {
			return nil, err
		}
	}
	id := col.ID()
	c.Created(cid, id)
	v.Colls[id], v.Access[id] = col, store.AccessOwner
	ab := c.addressBook(v, col)
	return withoutKeys(ab, "name", "description"), nil
}

func (c *Call) updateBook(v *View, id string, raw json.RawMessage) error {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || patch == nil {
		return failSet(SetInvalidPatch, "not a PatchObject")
	}
	real, _ := c.ResolveID(id)
	col := v.Colls[real]
	if col == nil {
		return failSet(SetNotFound, "no such address book")
	}
	cur := c.addressBook(v, col)
	next := copyJSON(cur)
	if err := ApplyPatch(next, patch); err != nil {
		return failSet(SetInvalidPatch, err.Error())
	}
	ch, err := c.parseBookProps(v.Owner, next, copyJSON(cur))
	if err != nil {
		return err
	}
	if !v.Own {
		if ch.description != nil {
			return failSet(SetForbidden, "only the owner can change the description", "description")
		}
		return c.h.store.UpdateSharePrefs(c.User, col.Key(), func(p *store.SharePrefs) {
			if ch.name != nil {
				p.DisplayName = *ch.name
			}
			if ch.sortOrder != nil {
				p.Order = strconv.Itoa(*ch.sortOrder)
			}
			if ch.isSubscribed != nil {
				p.Hidden = !*ch.isSubscribed
			}
		})
	}
	if col.ReadOnly() && (ch.name != nil || ch.description != nil || ch.shares != nil) {
		return failSet(SetForbidden, "generated address book")
	}
	if ch.shares != nil {
		if err := col.SetShares(*ch.shares); err != nil {
			return err
		}
	}
	return col.UpdateMeta(func(m *store.Meta) {
		if ch.name != nil {
			m.DisplayName = *ch.name
		}
		if ch.description != nil {
			m.Description = *ch.description
		}
		if ch.sortOrder != nil {
			m.Order = strconv.Itoa(*ch.sortOrder)
		}
		if ch.isSubscribed != nil {
			m.Unsubscribed = !*ch.isSubscribed
		}
	})
}

func (c *Call) destroyBook(v *View, id string, removeContents bool) error {
	real, _ := c.ResolveID(id)
	col := v.Colls[real]
	if col == nil {
		return failSet(SetNotFound, "no such address book")
	}
	if !v.Own {
		return failSet(SetForbidden, "shared with you; set isSubscribed to false to hide it")
	}
	if col.ReadOnly() {
		return failSet(SetForbidden, "generated address book")
	}
	if len(col.List()) > 0 && !removeContents {
		return failSet(SetAddressBookHasContents, "not empty; set onDestroyRemoveContents")
	}
	return c.h.store.DeleteCollection(col)
}
