package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/store"
)

type kind int

const (
	kRoot           kind = iota // <prefix>
	kPrincipals                 // <prefix>principals/
	kPrincipal                  // <prefix>principals/<u>/
	kGroups                     // <prefix>principals/groups/
	kGroupPrincipal             // <prefix>principals/groups/<g>/
	kHomes                      // <prefix><calendars|addressbooks>/
	kHome                       // <prefix><calendars|addressbooks>/<u>/
	kCollection                 // .../<u>/<collection>/ or .../<u>/<owner>~<collection>/
	kObject                     // .../<collection>/<name>
)

// access is a user's effective right on a collection.
type access int

const (
	accNone access = iota
	accRead
	accWrite
	accOwner
)

// kindInfo describes what differs between calendars and address books at
// the URL and resource level.
type kindInfo struct {
	seg         string // URL segment below the DAV prefix
	contentType string // of objects
	defaults    map[string]store.Meta
}

var kinds = map[store.Kind]kindInfo{
	store.Calendars:    {seg: "calendars", contentType: "text/calendar; charset=utf-8", defaults: DefaultCalendars},
	store.AddressBooks: {seg: "addressbooks", contentType: "text/vcard; charset=utf-8", defaults: DefaultAddressBooks},
}

// storeKinds lists the kinds in a stable order (for listings).
var storeKinds = []store.Kind{store.Calendars, store.AddressBooks}

func kindOfSeg(seg string) (store.Kind, bool) {
	for k, ki := range kinds {
		if ki.seg == seg {
			return k, true
		}
	}
	return "", false
}

// resource is a resolved request target.
//
// For collections in a home, owner is the home's user and colName the path
// segment. A collection shared with that user appears in their home under
// the alias "<real owner>~<name>"; col then belongs to someone else and
// acc says what the user may do with it. col is nil when the collection
// does not exist (yet); exists reports whether the object exists.
type resource struct {
	base    string // DAV prefix, e.g. "/dav/"
	kind    kind
	sk      store.Kind // for kHomes and below
	owner   string     // user (or group, for kGroupPrincipal) in the path
	colName string
	col     *store.Collection
	acc     access
	name    string
	info    store.ObjectInfo
	exists  bool
}

func (r *resource) isCollection() bool { return r.kind != kObject }
func (r *resource) isCalendar() bool   { return r.sk == store.Calendars }

// isShared reports whether the collection belongs to someone else.
func (r *resource) isShared() bool { return r.col != nil && r.col.Owner != r.owner }

func (r *resource) href() string {
	switch r.kind {
	case kRoot:
		return r.base
	case kPrincipals:
		return r.base + "principals/"
	case kPrincipal:
		return principalHref(r.base, r.owner)
	case kGroups:
		return r.base + "principals/groups/"
	case kGroupPrincipal:
		return groupHref(r.base, r.owner)
	case kHomes:
		return r.base + kinds[r.sk].seg + "/"
	case kHome:
		return homeHref(r.base, r.sk, r.owner)
	case kCollection:
		return homeHref(r.base, r.sk, r.owner) + r.colName + "/"
	default:
		return homeHref(r.base, r.sk, r.owner) + r.colName + "/" + r.name
	}
}

func principalHref(base, u string) string { return base + "principals/" + u + "/" }
func groupHref(base, g string) string     { return base + "principals/groups/" + g + "/" }

func homeHref(base string, sk store.Kind, u string) string {
	return base + kinds[sk].seg + "/" + u + "/"
}

// aliasName is how collection c appears in another user's home.
func aliasName(c *store.Collection) string { return c.Owner + "~" + c.Name }

var errForbidden = errStatus(http.StatusForbidden, "forbidden")

// access computes user's right on c.
func (s *Server) access(user string, c *store.Collection) access {
	switch c.AccessOf(user, s.cfg.Get().GroupsOf(user)) {
	case store.AccessOwner:
		return accOwner
	case store.AccessWrite:
		return accWrite
	case store.AccessRead:
		return accRead
	}
	return accNone
}

// ownAccess is the owner's right on their own collection c (nil: not
// created yet); generated collections are read-only even for the owner.
func ownAccess(c *store.Collection) access {
	if c != nil && c.ReadOnly() {
		return accRead
	}
	return accOwner
}

// resolve maps a request path to a resource and enforces access: users can
// see all principals but only their own homes; collections shared with
// them appear in their home under an alias.
func (s *Server) resolve(path, user string) (*resource, error) {
	base := s.prefix
	if path == base || path+"/" == base {
		return &resource{base: base, kind: kRoot, owner: user}, nil
	}
	rest, ok := strings.CutPrefix(path, base)
	if !ok {
		return nil, store.ErrNotFound
	}
	trailing := strings.HasSuffix(rest, "/")
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	for _, sg := range segs {
		if sg == "" || sg == "." || sg == ".." {
			return nil, store.ErrNotFound
		}
	}
	cfg := s.cfg.Get()
	if segs[0] == "principals" {
		switch {
		case len(segs) == 1:
			return &resource{base: base, kind: kPrincipals, owner: user}, nil
		case segs[1] == "groups" && len(segs) == 2:
			return &resource{base: base, kind: kGroups, owner: user}, nil
		case segs[1] == "groups" && len(segs) == 3:
			if _, ok := cfg.Groups[segs[2]]; !ok {
				return nil, store.ErrNotFound
			}
			return &resource{base: base, kind: kGroupPrincipal, owner: segs[2]}, nil
		case len(segs) == 2:
			if _, ok := cfg.Users[segs[1]]; !ok {
				return nil, store.ErrNotFound
			}
			return &resource{base: base, kind: kPrincipal, owner: segs[1]}, nil
		}
		return nil, store.ErrNotFound
	}
	sk, ok := kindOfSeg(segs[0])
	if !ok {
		return nil, store.ErrNotFound
	}
	if len(segs) == 1 {
		return &resource{base: base, kind: kHomes, sk: sk, owner: user}, nil
	}
	owner := segs[1]
	if _, ok := cfg.Users[owner]; !ok {
		return nil, store.ErrNotFound
	}
	if owner != user {
		return nil, errForbidden
	}
	if err := s.store.EnsureHome(owner, sk, kinds[sk].defaults); err != nil {
		return nil, err
	}
	res := &resource{base: base, kind: kHome, sk: sk, owner: owner}
	if len(segs) == 2 {
		return res, nil
	}
	res.kind, res.colName = kCollection, segs[2]
	if realOwner, name, isAlias := strings.Cut(segs[2], "~"); isAlias {
		// A collection shared with this user. Unknown or inaccessible
		// aliases are simply not found (nothing can be created here).
		if _, known := cfg.Users[realOwner]; !known || realOwner == owner {
			return nil, store.ErrNotFound
		}
		col, err := s.store.Collection(realOwner, sk, name)
		if err != nil {
			return nil, err
		}
		if res.acc = s.access(user, col); res.acc == accNone {
			return nil, store.ErrNotFound
		}
		res.col = col
	} else {
		col, err := s.store.Collection(owner, sk, segs[2])
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		res.col, res.acc = col, ownAccess(col)
	}
	if len(segs) == 3 {
		return res, nil
	}
	if len(segs) > 4 || trailing {
		return nil, store.ErrNotFound
	}
	res.kind, res.name = kObject, segs[3]
	if res.col != nil {
		if info, err := res.col.Stat(res.name); err == nil {
			res.info, res.exists = info, true
		}
	}
	return res, nil
}

// object returns the resource for an object in collection res.
func (res *resource) object(info store.ObjectInfo, exists bool) *resource {
	o := *res
	o.kind, o.name, o.info, o.exists = kObject, info.Name, info, exists
	return &o
}

// children lists the members of a collection resource for Depth: 1.
func (s *Server) children(res *resource) ([]*resource, error) {
	base := res.base
	switch res.kind {
	case kRoot:
		out := []*resource{{base: base, kind: kPrincipals, owner: res.owner}}
		for _, sk := range storeKinds {
			out = append(out, &resource{base: base, kind: kHomes, sk: sk, owner: res.owner})
		}
		return out, nil
	case kPrincipals:
		return []*resource{
			{base: base, kind: kPrincipal, owner: res.owner},
			{base: base, kind: kGroups, owner: res.owner},
		}, nil
	case kGroups:
		var out []*resource
		for _, g := range s.cfg.Get().GroupsOf(res.owner) {
			out = append(out, &resource{base: base, kind: kGroupPrincipal, owner: g})
		}
		return out, nil
	case kHomes:
		if err := s.store.EnsureHome(res.owner, res.sk, kinds[res.sk].defaults); err != nil {
			return nil, err
		}
		return []*resource{{base: base, kind: kHome, sk: res.sk, owner: res.owner}}, nil
	case kHome:
		cols, err := s.store.Collections(res.owner, res.sk)
		if err != nil {
			return nil, err
		}
		var out []*resource
		for _, c := range cols {
			out = append(out, &resource{base: base, kind: kCollection, sk: res.sk, owner: res.owner,
				colName: c.Name, col: c, acc: ownAccess(c)})
		}
		for _, c := range s.store.All() {
			if c.Kind != res.sk || c.Owner == res.owner {
				continue
			}
			acc := s.access(res.owner, c)
			if acc == accNone || s.store.SharePrefs(res.owner, c.Key()).Hidden {
				continue
			}
			out = append(out, &resource{base: base, kind: kCollection, sk: res.sk, owner: res.owner,
				colName: aliasName(c), col: c, acc: acc})
		}
		return out, nil
	case kCollection:
		if res.col == nil {
			return nil, nil
		}
		var out []*resource
		for _, o := range res.col.List() {
			out = append(out, res.object(o, true))
		}
		return out, nil
	}
	return nil, nil
}
