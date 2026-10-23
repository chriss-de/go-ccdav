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
	kCalHomes               // /dav/calendars/
	kCalHome                // /dav/calendars/<u>/
	kCalendar               // /dav/calendars/<u>/<cal>/
	kCalObject              // /dav/calendars/<u>/<cal>/<name>
)

// resource is a resolved request target. col is nil when the calendar does
// not exist (yet); exists reports whether the object exists.
type resource struct {
	kind    kind
	owner   string
	colName string
	col     *store.Collection
	name    string
	info    store.ObjectInfo
	exists  bool
}

func (r *resource) isCollection() bool { return r.kind != kCalObject }

func (r *resource) href() string {
	switch r.kind {
	case kRoot:
		return "/dav/"
	case kPrincipals:
		return "/dav/principals/"
	case kPrincipal:
		return principalHref(r.owner)
	case kCalHomes:
		return "/dav/calendars/"
	case kCalHome:
		return calHomeHref(r.owner)
	case kCalendar:
		return calHomeHref(r.owner) + r.colName + "/"
	default:
		return calHomeHref(r.owner) + r.colName + "/" + r.name
	}
}

func principalHref(u string) string { return "/dav/principals/" + u + "/" }
func calHomeHref(u string) string   { return "/dav/calendars/" + u + "/" }

var errForbidden = errStatus(http.StatusForbidden, "forbidden")

// resolve maps a request path to a resource and enforces access: users can
// see other principals but only their own calendars. (Sharing will replace
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
	switch segs[0] {
	case "principals":
		switch len(segs) {
		case 1:
			return &resource{kind: kPrincipals, owner: user}, nil
		case 2:
			if _, ok := s.cfg.Get().Users[segs[1]]; !ok {
				return nil, store.ErrNotFound
			}
			return &resource{kind: kPrincipal, owner: segs[1]}, nil
		}
	case "calendars":
		if len(segs) == 1 {
			return &resource{kind: kCalHomes, owner: user}, nil
		}
		owner := segs[1]
		if _, ok := s.cfg.Get().Users[owner]; !ok {
			return nil, store.ErrNotFound
		}
		if owner != user {
			return nil, errForbidden
		}
		if err := s.store.EnsureHome(owner, store.Calendars, DefaultCalendars); err != nil {
			return nil, err
		}
		res := &resource{kind: kCalHome, owner: owner}
		if len(segs) == 2 {
			return res, nil
		}
		res.kind, res.colName = kCalendar, segs[2]
		col, err := s.store.Collection(owner, store.Calendars, segs[2])
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
		res.kind, res.name = kCalObject, segs[3]
		if col != nil {
			if info, err := col.Stat(res.name); err == nil {
				res.info, res.exists = info, true
			}
		}
		return res, nil
	}
	return nil, store.ErrNotFound
}

// children lists the members of a collection resource for Depth: 1.
func (s *Server) children(res *resource) ([]*resource, error) {
	switch res.kind {
	case kRoot:
		return []*resource{
			{kind: kPrincipals, owner: res.owner},
			{kind: kCalHomes, owner: res.owner},
		}, nil
	case kPrincipals:
		return []*resource{{kind: kPrincipal, owner: res.owner}}, nil
	case kCalHomes:
		if err := s.store.EnsureHome(res.owner, store.Calendars, DefaultCalendars); err != nil {
			return nil, err
		}
		return []*resource{{kind: kCalHome, owner: res.owner}}, nil
	case kCalHome:
		cols, err := s.store.Collections(res.owner, store.Calendars)
		if err != nil {
			return nil, err
		}
		out := make([]*resource, 0, len(cols))
		for _, c := range cols {
			out = append(out, &resource{kind: kCalendar, owner: res.owner, colName: c.Name, col: c})
		}
		return out, nil
	case kCalendar:
		if res.col == nil {
			return nil, nil
		}
		var out []*resource
		for _, o := range res.col.List() {
			out = append(out, &resource{kind: kCalObject, owner: res.owner, colName: res.colName,
				col: res.col, name: o.Name, info: o, exists: true})
		}
		return out, nil
	}
	return nil, nil
}
