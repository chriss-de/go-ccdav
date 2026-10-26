package jmap

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chriss-de/go-ccdav/internal/store"
)

// Codec converts stored objects (vCards, later iCalendar files) to JMAP
// objects. The files stay the source of truth: DAV clients get exactly
// what they wrote, and JMAP sees a converted view.
type Codec interface {
	ToJMAP(raw []byte) (map[string]any, error)
}

// typeDef ties a JMAP object type to a store kind.
type typeDef struct {
	name     string // "ContactCard"
	kind     store.Kind
	codec    Codec
	collProp string // "addressBookIds"
	comp     string // only objects of this component ("VEVENT"); "" for all
}

// convCache keeps converted objects by collection, name and ETag, so
// /query doesn't parse every file each time. Cached maps are shared and
// must not be changed.
type convCache struct {
	mu sync.Mutex
	m  map[string]map[string]any
}

const convCacheMax = 20000

func (cc *convCache) get(key string) (map[string]any, bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	v, ok := cc.m[key]
	return v, ok
}

func (cc *convCache) put(key string, v map[string]any) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.m == nil || len(cc.m) >= convCacheMax {
		cc.m = map[string]map[string]any{} // crude, but rare and cheap to refill
	}
	cc.m[key] = v
}

// entry is one object as the caller sees it.
type entry struct {
	ref store.ObjectRef
	obj map[string]any // the full JMAP object; the caller's own copy
}

// load converts one object and adds the JMAP-level properties: id, the
// collection map, created and (if the file has none) updated. false if
// it is gone or can't be converted; the latter is logged.
func (h *Handler) load(t *typeDef, v *View, ref store.ObjectRef) (*entry, bool) {
	e, ok := h.loadRaw(t, v, ref)
	if ok && v.hasOverlay() {
		e.obj = applyOverlay(e.obj, v.overlayOf(ref.ID))
	}
	return e, ok
}

// loadRaw is load without the caller's per-user data: the object as
// stored.
func (h *Handler) loadRaw(t *typeDef, v *View, ref store.ObjectRef) (*entry, bool) {
	col := v.Colls[ref.Coll]
	if col == nil {
		return nil, false
	}
	info, err := col.Stat(ref.Name)
	if err != nil || t.comp != "" && info.Comp != t.comp {
		return nil, false
	}
	key := ref.Coll + "/" + ref.Name + "/" + info.ETag
	conv, ok := h.conv.get(key)
	if !ok {
		data, cur, err := col.Get(ref.Name)
		if err != nil {
			return nil, false
		}
		if conv, err = t.codec.ToJMAP(data); err != nil {
			h.log.Warn("jmap: cannot convert", "type", t.name, "collection", col.Key(), "name", ref.Name, "err", err)
			return nil, false
		}
		info = cur
		h.conv.put(ref.Coll+"/"+ref.Name+"/"+cur.ETag, conv)
	}
	obj := maps.Clone(conv)
	obj["id"] = ref.ID
	obj[t.collProp] = map[string]any{ref.Coll: true}
	if _, ok := obj["created"]; !ok { // the file's own (CREATED) wins
		obj["created"] = ref.Created.Format(time.RFC3339)
	}
	if _, ok := obj["updated"]; !ok {
		obj["updated"] = info.ModTime.UTC().Format(time.RFC3339)
	}
	return &entry{ref: ref, obj: obj}, true
}

// all loads every object the view shows, ordered by collection and name.
func (h *Handler) all(t *typeDef, v *View) []*entry {
	var out []*entry
	for _, id := range slices.Sorted(maps.Keys(v.Colls)) {
		col := v.Colls[id]
		for _, info := range col.List() {
			ref, ok := v.Log.ObjectAt(col, info.Name)
			if !ok {
				continue // not reconciled yet
			}
			if e, ok := h.load(t, v, ref); ok {
				out = append(out, e)
			}
		}
	}
	return out
}

// getObjects implements Foo/get for an object type.
func (c *Call) getObjects(raw json.RawMessage, t *typeDef) (any, error) {
	var args GetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := CheckGet(args); err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, t.kind)
	if err != nil {
		return nil, err
	}
	resp := &GetResponse{AccountID: args.AccountID, State: v.State(store.LevelObjects), List: []any{}, NotFound: []string{}}
	if args.IDs == nil {
		es := c.h.all(t, v)
		if len(es) > MaxObjectsInGet {
			return nil, Errorf(ErrRequestTooLarge, "%d objects; ask for ids", len(es))
		}
		for _, e := range es {
			resp.List = append(resp.List, Select(e.obj, args.Properties))
		}
		return resp, nil
	}
	seen := map[string]bool{}
	for _, id := range *args.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		real, ok := c.ResolveID(id)
		ref, found := v.Log.Lookup(real)
		var e *entry
		if ok && found {
			e, found = c.h.load(t, v, ref)
		}
		if !ok || !found {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		resp.List = append(resp.List, Select(e.obj, args.Properties))
	}
	return resp, nil
}

// Collations for text filters and sorting. Matching is by substring after
// lower-casing, as in CardDAV's addressbook-query.
var collations = []string{"i;ascii-casemap", "i;unicode-casemap"}

// filter is a parsed FilterOperator / FilterCondition tree.
type filter struct {
	op    string // "AND", "OR", "NOT"; "" for a condition
	conds []*filter
	cond  map[string]json.RawMessage
}

// parseFilter parses a filter; null gives nil (match everything).
// Conditions may only use the known properties.
func parseFilter(raw json.RawMessage, known map[string]bool) (*filter, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	return parseFilterNode(raw, known, 0)
}

func parseFilterNode(raw json.RawMessage, known map[string]bool, depth int) (*filter, error) {
	if depth > 32 {
		return nil, Errorf(ErrUnsupportedFilter, "filter nested too deeply")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, Errorf(ErrInvalidArguments, "filter: %v", err)
	}
	if opRaw, ok := m["operator"]; ok {
		f := &filter{}
		if err := json.Unmarshal(opRaw, &f.op); err != nil || f.op != "AND" && f.op != "OR" && f.op != "NOT" {
			return nil, Errorf(ErrInvalidArguments, "filter: bad operator %s", opRaw)
		}
		var conds []json.RawMessage
		if err := json.Unmarshal(m["conditions"], &conds); err != nil || conds == nil {
			return nil, Errorf(ErrInvalidArguments, "filter: operator %s needs conditions", f.op)
		}
		for _, c := range conds {
			sub, err := parseFilterNode(c, known, depth+1)
			if err != nil {
				return nil, err
			}
			f.conds = append(f.conds, sub)
		}
		return f, nil
	}
	for k := range m {
		if !known[k] {
			return nil, Errorf(ErrUnsupportedFilter, "unsupported filter condition %q", k)
		}
	}
	return &filter{cond: m}, nil
}

// eval evaluates the tree, with match deciding single conditions. NOT
// matches when none of its conditions does.
func (f *filter) eval(match func(map[string]json.RawMessage) (bool, error)) (bool, error) {
	if f == nil {
		return true, nil
	}
	if f.op == "" {
		return match(f.cond)
	}
	for _, c := range f.conds {
		ok, err := c.eval(match)
		if err != nil {
			return false, err
		}
		switch {
		case f.op == "AND" && !ok:
			return false, nil
		case f.op == "OR" && ok:
			return true, nil
		case f.op == "NOT" && ok:
			return false, nil
		}
	}
	return f.op != "OR", nil
}

// comparator is one sort criterion.
type comparator struct {
	Property    string `json:"property"`
	IsAscending *bool  `json:"isAscending"`
	Collation   string `json:"collation"`
}

func (c comparator) ascending() bool { return c.IsAscending == nil || *c.IsAscending }

// parseSort parses a sort list; only known properties and our
// collations are supported.
func parseSort(raw json.RawMessage, known map[string]bool) ([]comparator, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out []comparator
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, Errorf(ErrInvalidArguments, "sort: %v", err)
	}
	for _, c := range out {
		if !known[c.Property] {
			return nil, Errorf(ErrUnsupportedSort, "cannot sort by %q", c.Property)
		}
		if c.Collation != "" && !slices.Contains(collations, c.Collation) {
			return nil, Errorf(ErrUnsupportedSort, "unsupported collation %q", c.Collation)
		}
	}
	return out, nil
}

// contains is a case-insensitive substring match.
func contains(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
