package server

import (
	"errors"
	"net/http"
	"strings"

	"davd/internal/store"
)

type kind int

const (
	kRoot       kind = iota // /dav/ (and / for discovery)
	kPrincipals             // /dav/principals/
	kPrincipal              // /dav/principals/<u>/
	kHomes                  // /dav/<calendars|addressbooks>/
	kHome                   // /dav/<calendars|addressbooks>/<u>/
	kCollection             // .../<u>/<collection>/
	kObject                 // .../<u>/<collection>/<name>
)

// kindInfo describes what differs between calendars and address books at
// the URL and resource level.
type kindInfo struct {
	seg         string // URL segment below /dav/
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

// resource is a resolved request target. col is nil when the collection
// does not exist (yet); exists reports whether the object exists.
type resource struct {
	kind    kind
	sk      store.Kind // for kHomes and below
	owner   string
	colName string
	col     *store.Collection
	name    string
	info    store.ObjectInfo
	exists  bool
}

func (r *resource) isCollection() bool { return r.kind != kObject }
func (r *resource) isCalendar() bool   { return r.sk == store.Calendars }

func (r *resource) href() string {
	switch r.kind {
	case kRoot:
		return "/dav/"
	case kPrincipals:
		return "/dav/principals/"
	case kPrincipal:
		return principalHref(r.owner)
	case kHomes:
		return "/dav/" + kinds[r.sk].seg + "/"
	case kHome:
		return homeHref(r.sk, r.owner)
	case kCollection:
		return homeHref(r.sk, r.owner) + r.colName + "/"
	default:
		return homeHref(r.sk, r.owner) + r.colName + "/" + r.name
	}
}

func principalHref(u string) string { return "/dav/principals/" + u + "/" }

func homeHref(sk store.Kind, u string) string { return "/dav/" + kinds[sk].seg + "/" + u + "/" }

var errForbidden = errStatus(http.StatusForbidden, "forbidden")

// resolve maps a request path to a resource and enforces access: users can
// see other principals but only their own homes. (Sharing will replace
// the owner check with a can(user, collection) lookup.)
func (s *Server) resolve(path, user string) (*resource, error) {
	if path == "/" || path == "/dav" || path == "/dav/" {
		return &resource{kind: kRoot, owner: user}, nil
	}
	rest := strings.TrimPrefix(path, "/dav/")
	trailing := strings.HasSuffix(rest, "/")
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	for _, sg := range segs {
		if sg == "" || sg == "." || sg == ".." {
			return nil, errStatus(http.StatusNotFound, "not found")
		}
	}
	if segs[0] == "principals" {
		switch len(segs) {
		case 1:
			return &resource{kind: kPrincipals, owner: user}, nil
		case 2:
			if _, ok := s.cfg.Get().Users[segs[1]]; !ok {
				return nil, store.ErrNotFound
			}
			return &resource{kind: kPrincipal, owner: segs[1]}, nil
		}
		return nil, store.ErrNotFound
	}
	sk, ok := kindOfSeg(segs[0])
	if !ok {
		return nil, store.ErrNotFound
	}
	if len(segs) == 1 {
		return &resource{kind: kHomes, sk: sk, owner: user}, nil
	}
	owner := segs[1]
	if _, ok := s.cfg.Get().Users[owner]; !ok {
		return nil, store.ErrNotFound
	}
	if owner != user {
		return nil, errForbidden
	}
	if err := s.store.EnsureHome(owner, sk, kinds[sk].defaults); err != nil {
		return nil, err
	}
	res := &resource{kind: kHome, sk: sk, owner: owner}
	if len(segs) == 2 {
		return res, nil
	}
	res.kind, res.colName = kCollection, segs[2]
	col, err := s.store.Collection(owner, sk, segs[2])
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	res.col = col
	if len(segs) == 3 {
		return res, nil
	}
	if len(segs) > 4 || trailing {
		return nil, store.ErrNotFound
	}
	res.kind, res.name = kObject, segs[3]
	if col != nil {
		if info, err := col.Stat(res.name); err == nil {
			res.info, res.exists = info, true
		}
	}
	return res, nil
}

// object returns the resource for an object in collection res.
func (res *resource) object(info store.ObjectInfo, exists bool) *resource {
	return &resource{kind: kObject, sk: res.sk, owner: res.owner, colName: res.colName,
		col: res.col, name: info.Name, info: info, exists: exists}
}

// children lists the members of a collection resource for Depth: 1.
func (s *Server) children(res *resource) ([]*resource, error) {
	switch res.kind {
	case kRoot:
		out := []*resource{{kind: kPrincipals, owner: res.owner}}
		for _, sk := range storeKinds {
			out = append(out, &resource{kind: kHomes, sk: sk, owner: res.owner})
		}
		return out, nil
	case kPrincipals:
		return []*resource{{kind: kPrincipal, owner: res.owner}}, nil
	case kHomes:
		if err := s.store.EnsureHome(res.owner, res.sk, kinds[res.sk].defaults); err != nil {
			return nil, err
		}
		return []*resource{{kind: kHome, sk: res.sk, owner: res.owner}}, nil
	case kHome:
		cols, err := s.store.Collections(res.owner, res.sk)
		if err != nil {
			return nil, err
		}
		out := make([]*resource, 0, len(cols))
		for _, c := range cols {
			out = append(out, &resource{kind: kCollection, sk: res.sk, owner: res.owner, colName: c.Name, col: c})
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
