package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/store"
)

// View is what the calling user sees of one account and kind: the
// owner's collections of that kind, or for a shared account only those
// shared with the user.
type View struct {
	AccountID string
	User      string // the caller
	Owner     string
	Own       bool
	Kind      store.Kind
	Log       *store.Account
	Colls     map[string]*store.Collection // collection id -> collection
	Access    map[string]string            // collection id -> store access

	st *store.Store
}

// View resolves an accountId argument. accountNotFound if the user can't
// see the account.
func (c *Call) View(accountID string, kind store.Kind) (*View, error) {
	a, ok := c.h.account(c.User, accountID)
	if !ok {
		return nil, Errorf(ErrAccountNotFound, "no account %q", accountID)
	}
	if a.personal {
		// As in DAV, a home starts with its default collections.
		if err := c.h.store.EnsureHome(a.owner, kind, c.h.defaults[kind]); err != nil {
			return nil, err
		}
	}
	log, err := c.h.store.Account(a.owner, kind)
	if err != nil {
		return nil, err
	}
	v := &View{st: c.h.store, AccountID: accountID, User: c.User, Owner: a.owner, Own: a.personal, Kind: kind, Log: log,
		Colls: map[string]*store.Collection{}, Access: map[string]string{}}
	groups := c.h.cfg.Get().GroupsOf(c.User)
	for _, col := range c.h.store.All() {
		if col.Owner != a.owner || col.Kind != kind {
			continue
		}
		if acc := col.AccessOf(c.User, groups); acc != "" {
			id := col.ID()
			v.Colls[id], v.Access[id] = col, acc
		}
	}
	return v, nil
}

// tag is part of every state string of a shared account, for what the
// log can't express: it changes whenever the set of shared collections
// or the user's access to them changes, and for collections also when
// the user's own settings for them (name, order, hidden) change. In the
// owner's account it is constant.
func (v *View) tag(l store.Level) string {
	if v.Own {
		return "o"
	}
	sum := sha256.New()
	for _, id := range slices.Sorted(maps.Keys(v.Access)) {
		fmt.Fprintf(sum, "%s:%s", id, v.Access[id])
		if l == store.LevelCollections {
			fmt.Fprintf(sum, ":%d", v.st.SharePrefs(v.User, v.Colls[id].Key()).Version)
		}
		sum.Write([]byte{'\n'})
	}
	return hex.EncodeToString(sum.Sum(nil)[:4])
}

// State is the JMAP state string of a level: "<home>-<token>-<tag>".
func (v *View) State(l store.Level) string {
	home, tok := v.Log.State(l)
	return v.state(l, home, tok)
}

func (v *View) state(l store.Level, home string, tok uint64) string {
	return home + "-" + strconv.FormatUint(tok, 10) + "-" + v.tag(l)
}

// token parses a state string of this view; ok is false if it belongs to
// another log or the tag changed.
func (v *View) token(l store.Level, state string) (uint64, bool) {
	parts := strings.Split(state, "-")
	if len(parts) != 3 {
		return 0, false
	}
	home, _ := v.Log.State(l)
	tok, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || parts[0] != home || parts[2] != v.tag(l) {
		return 0, false
	}
	return tok, true
}

// ChangesArgs are the arguments of every Foo/changes method.
type ChangesArgs struct {
	AccountID  string `json:"accountId"`
	SinceState string `json:"sinceState"`
	MaxChanges *int   `json:"maxChanges"`
}

// ChangesResponse is the response of every Foo/changes method.
type ChangesResponse struct {
	AccountID      string   `json:"accountId"`
	OldState       string   `json:"oldState"`
	NewState       string   `json:"newState"`
	HasMoreChanges bool     `json:"hasMoreChanges"`
	Created        []string `json:"created"`
	Updated        []string `json:"updated"`
	Destroyed      []string `json:"destroyed"`
}

// Changes implements Foo/changes (RFC 8620 §5.2) for collections or
// objects of a kind. In a shared account it only reports what the user
// can see; moves there, which may cross that boundary, need a resync.
func (c *Call) Changes(raw json.RawMessage, kind store.Kind, l store.Level) (*ChangesResponse, error) {
	var args ChangesArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.MaxChanges != nil && *args.MaxChanges <= 0 {
		return nil, Errorf(ErrInvalidArguments, "maxChanges must be positive")
	}
	v, err := c.View(args.AccountID, kind)
	if err != nil {
		return nil, err
	}
	since, ok := v.token(l, args.SinceState)
	if !ok {
		return nil, Errorf(ErrCannotCalculateChanges, "unknown state")
	}
	maxIDs := 0
	if args.MaxChanges != nil {
		maxIDs = *args.MaxChanges
	}
	changes, next, more, err := v.Log.Changes(l, since, maxIDs)
	if errors.Is(err, store.ErrInvalidToken) {
		return nil, Errorf(ErrCannotCalculateChanges, "state too old")
	}
	if err != nil {
		return nil, err
	}
	home, _ := v.Log.State(l)
	resp := &ChangesResponse{AccountID: args.AccountID, OldState: args.SinceState, NewState: v.state(l, home, next),
		HasMoreChanges: more, Created: []string{}, Updated: []string{}, Destroyed: []string{}}
	for _, ch := range changes {
		if !v.Own {
			if ch.Moved {
				return nil, Errorf(ErrCannotCalculateChanges, "objects moved between collections")
			}
			coll := ch.Coll
			if l == store.LevelCollections {
				coll = ch.ID
			}
			if v.Colls[coll] == nil {
				continue
			}
		}
		switch {
		case ch.Deleted:
			resp.Destroyed = append(resp.Destroyed, ch.ID)
		case ch.Created:
			resp.Created = append(resp.Created, ch.ID)
		default:
			resp.Updated = append(resp.Updated, ch.ID)
		}
	}
	return resp, nil
}

// GetArgs are the arguments of every Foo/get method.
type GetArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`        // nil: all
	Properties *[]string `json:"properties"` // nil: all
}

// GetResponse is the response of every Foo/get method.
type GetResponse struct {
	AccountID string   `json:"accountId"`
	State     string   `json:"state"`
	List      []any    `json:"list"`
	NotFound  []string `json:"notFound"`
}

// CheckGet validates GetArgs against maxObjectsInGet.
func CheckGet(args GetArgs) error {
	if args.IDs != nil && len(*args.IDs) > MaxObjectsInGet {
		return Errorf(ErrRequestTooLarge, "more than %d ids", MaxObjectsInGet)
	}
	return nil
}

// Select reduces obj to the requested properties; "id" is always kept.
func Select(obj map[string]any, props *[]string) map[string]any {
	if props == nil {
		return obj
	}
	out := map[string]any{"id": obj["id"]}
	for _, p := range *props {
		if v, ok := obj[p]; ok {
			out[p] = v
		}
	}
	return out
}

// SetArgs are the arguments of every Foo/set method.
type SetArgs struct {
	AccountID string                     `json:"accountId"`
	IfInState *string                    `json:"ifInState"`
	Create    map[string]json.RawMessage `json:"create"`
	Update    map[string]json.RawMessage `json:"update"` // id -> PatchObject
	Destroy   []string                   `json:"destroy"`
}

// SetError is one failed create, update or destroy (RFC 8620 §5.3).
type SetError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
}

// SetResponse is the response of every Foo/set method. Empty maps and
// lists are sent as null, as the RFC asks.
type SetResponse struct {
	AccountID    string              `json:"accountId"`
	OldState     *string             `json:"oldState"`
	NewState     string              `json:"newState"`
	Created      map[string]any      `json:"created"`
	Updated      map[string]any      `json:"updated"`
	Destroyed    []string            `json:"destroyed"`
	NotCreated   map[string]SetError `json:"notCreated"`
	NotUpdated   map[string]SetError `json:"notUpdated"`
	NotDestroyed map[string]SetError `json:"notDestroyed"`
}

// CheckSet validates SetArgs: the number of operations against
// maxObjectsInSet, and ifInState against the current state.
func CheckSet(args SetArgs, current string) error {
	if len(args.Create)+len(args.Update)+len(args.Destroy) > MaxObjectsInSet {
		return Errorf(ErrRequestTooLarge, "more than %d operations", MaxObjectsInSet)
	}
	if args.IfInState != nil && *args.IfInState != current {
		return Errorf(ErrStateMismatch, "state is %s", current)
	}
	return nil
}

// ApplyPatch applies a PatchObject (RFC 8620 §5.3) to obj: every key is
// a JSON Pointer path (without the leading "/") whose value replaces the
// one at that path; null removes it. All parents must exist.
func ApplyPatch(obj map[string]any, patch map[string]any) error {
	for _, path := range slices.Sorted(maps.Keys(patch)) {
		toks := strings.Split(path, "/")
		parent := obj
		for _, t := range toks[:len(toks)-1] {
			next, ok := parent[pointerUnescape.Replace(t)].(map[string]any)
			if !ok {
				return fmt.Errorf("patch path %q: no object at %q", path, t)
			}
			parent = next
		}
		last := pointerUnescape.Replace(toks[len(toks)-1])
		if patch[path] == nil {
			delete(parent, last)
		} else {
			parent[last] = patch[path]
		}
	}
	return nil
}

// QueryArgs are the arguments of every Foo/query method.
type QueryArgs struct {
	AccountID      string          `json:"accountId"`
	Filter         json.RawMessage `json:"filter"`
	Sort           json.RawMessage `json:"sort"`
	Position       int             `json:"position"`
	Anchor         *string         `json:"anchor"`
	AnchorOffset   int             `json:"anchorOffset"`
	Limit          *int            `json:"limit"`
	CalculateTotal bool            `json:"calculateTotal"`
}

// QueryResponse is the response of every Foo/query method.
type QueryResponse struct {
	AccountID           string   `json:"accountId"`
	QueryState          string   `json:"queryState"`
	CanCalculateChanges bool     `json:"canCalculateChanges"`
	Position            int      `json:"position"`
	IDs                 []string `json:"ids"`
	Total               *int     `json:"total,omitempty"`
	Limit               *int     `json:"limit,omitempty"`
}

// Page cuts the window that position or anchor, anchorOffset and limit
// select out of the sorted ids (RFC 8620 §5.5). A limit above maxLimit
// is lowered, and the response then states the limit used.
func Page(ids []string, args QueryArgs, maxLimit int) (*QueryResponse, error) {
	resp := &QueryResponse{AccountID: args.AccountID}
	limit := maxLimit
	if args.Limit != nil {
		if *args.Limit < 0 {
			return nil, Errorf(ErrInvalidArguments, "negative limit")
		}
		if *args.Limit > maxLimit {
			resp.Limit = &maxLimit
		} else {
			limit = *args.Limit
		}
	}
	pos := args.Position
	if args.Anchor != nil {
		i := slices.Index(ids, *args.Anchor)
		if i < 0 {
			return nil, Errorf(ErrAnchorNotFound, "anchor %q not in results", *args.Anchor)
		}
		pos = max(i+args.AnchorOffset, 0)
	} else if pos < 0 {
		pos = max(len(ids)+pos, 0)
	}
	end := min(pos+limit, len(ids))
	resp.Position, resp.IDs = pos, []string{}
	if pos < len(ids) {
		resp.IDs = slices.Clone(ids[pos:end])
	}
	if args.CalculateTotal {
		n := len(ids)
		resp.Total = &n
	}
	return resp, nil
}
