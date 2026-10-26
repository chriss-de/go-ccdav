package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/jscalendar"
	"github.com/chriss-de/go-ccdav/internal/peruser"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// Per-user data in CalDAV (caldav_per_user): in a calendar someone else
// owns, each user gets their own alarms, color, categories, free/busy
// and alarm bookkeeping, as JMAP keeps them (package peruser). A user's
// GET shows the owner's object with their values; their PUT is split
// into the per-user part (their overlay) and the rest (the object,
// needing write access). The owner sees no difference.
//
// Read-only shares keep advertising read-only privileges, so clients
// don't offer edits and practically never PUT there; a per-user-only PUT
// that arrives anyway is accepted.

// userView is the requesting user's per-user data for one object.
type userView struct {
	eventID string
	ev      store.OverlayEvent
	data    peruser.Data
}

// perUserApplies reports whether per-user data applies to res: an
// existing object of a calendar the requesting user doesn't own.
func (s *Server) perUserApplies(res *resource) bool {
	return s.perUser && res.kind == kObject && res.isCalendar() && res.isShared() && res.exists && res.acc >= accRead
}

func (s *Server) userViewOf(res *resource) (userView, bool) {
	if !s.perUserApplies(res) {
		return userView{}, false
	}
	log, err := s.store.Account(res.col.Owner, store.Calendars)
	if err != nil {
		return userView{}, false
	}
	id, ok := log.ObjectID(res.col, res.name)
	if !ok {
		return userView{}, false
	}
	o, err := s.store.Overlay(res.owner, res.col.Owner)
	if err != nil {
		return userView{}, false
	}
	ev := o.Events[id]
	return userView{eventID: id, ev: ev, data: peruser.Decode(ev.Data)}, true
}

// etagOf is an object's ETag as the requesting user sees it: with their
// own values, the file's plus their data's version.
func (s *Server) etagOf(res *resource) string {
	if uv, ok := s.userViewOf(res); ok && !uv.data.Empty() {
		return strings.TrimSuffix(res.info.ETag, `"`) + "-u" + strconv.FormatUint(uv.ev.V, 10) + `"`
	}
	return res.info.ETag
}

// objectData reads an object as the requesting user sees it.
func (s *Server) objectData(res *resource) ([]byte, store.ObjectInfo, error) {
	data, info, err := res.col.Get(res.name)
	if err != nil {
		return nil, info, err
	}
	uv, ok := s.userViewOf(res)
	if !ok || uv.data.Empty() {
		return data, info, nil
	}
	cal, err := ical.ParseBytes(data)
	if err != nil {
		return data, info, nil
	}
	e, err := jscalendar.FromICal(cal)
	if err != nil {
		return data, info, nil
	}
	out, err := jscalendar.Render(cal, peruser.Apply(e, uv.data))
	if err != nil {
		s.log.Warn("per-user view", "href", res.href(), "err", err)
		return data, info, nil
	}
	return out.Bytes(), info, nil
}

// overlayVersion is the requesting user's per-user data version for the
// owner of a shared calendar, or -1 if per-user data doesn't apply.
func (s *Server) overlayVersion(res *resource) int64 {
	if !s.perUser || !res.isCalendar() || !res.isShared() {
		return -1
	}
	o, err := s.store.Overlay(res.owner, res.col.Owner)
	if err != nil {
		return -1
	}
	return int64(o.Version)
}

// overlayChanges lists objects of res's collection whose per-user data
// changed after version since.
func (s *Server) overlayChanges(res *resource, since uint64) []string {
	o, err := s.store.Overlay(res.owner, res.col.Owner)
	if err != nil {
		return nil
	}
	log, err := s.store.Account(res.col.Owner, store.Calendars)
	if err != nil {
		return nil
	}
	coll := res.col.ID()
	var names []string
	for id, ev := range o.Events {
		if ev.V <= since {
			continue
		}
		if ref, ok := log.Lookup(id); ok && ref.Coll == coll {
			names = append(names, ref.Name)
		}
	}
	return names
}

// putPerUser handles a PUT to an object in a calendar the user doesn't
// own: their per-user values go into their overlay (allowed read-only),
// anything else into the object (needing write access).
func (s *Server) putPerUser(w http.ResponseWriter, r *http.Request, res *resource, upload []byte) {
	uv, ok := s.userViewOf(res)
	if !ok {
		needPrivileges(w)
		return
	}
	if im := r.Header.Get("If-Match"); im != "" && !etagMatch(s.etagOf(res), im) {
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
		return
	}
	if r.Header.Get("If-None-Match") == "*" {
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
		return
	}
	prevData, info, err := res.col.Get(res.name)
	if err != nil {
		s.httpError(w, err)
		return
	}
	prev, err1 := ical.ParseBytes(prevData)
	up, err2 := ical.ParseBytes(upload)
	if err1 != nil || err2 != nil {
		badRequest(w, "cannot parse the calendar object")
		return
	}
	owner, err1 := jscalendar.FromICal(prev)
	mine, err2 := jscalendar.FromICal(up)
	if err1 != nil || err2 != nil {
		// No event (a task, a journal): no per-user data, as before.
		s.putPlain(w, r, res, upload, info)
		return
	}
	if owner["uid"] != mine["uid"] {
		badRequest(w, "the UID cannot change")
		return
	}
	file, d := peruser.Split(mine, owner)
	if d.Empty() && res.acc >= accWrite {
		// Nothing of the user's own: the upload is the object, as before.
		s.putPlain(w, r, res, upload, info)
		if err := s.store.SetOverlay(res.owner, res.col.Owner, uv.eventID, nil); err != nil {
			s.log.Error("per-user data", "err", err)
		}
		return
	}
	merged, changed, err := jscalendar.Merge(prev, file)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if changed {
		if res.acc < accWrite {
			needPrivileges(w)
			return
		}
		if _, _, err := res.col.Put(res.name, merged.Bytes(), store.Cond{IfMatch: info.ETag}); err != nil {
			s.httpError(w, err)
			return
		}
	}
	if err := s.store.SetOverlay(res.owner, res.col.Owner, uv.eventID, peruser.Encode(d)); err != nil {
		s.httpError(w, err)
		return
	}
	// What is stored differs from the upload: no ETag (RFC 4791 §5.3.4),
	// the client fetches it again.
	w.WriteHeader(http.StatusNoContent)
}

// putPlain stores an upload as it is, with the If-Match the client sent
// translated to the object's own ETag.
func (s *Server) putPlain(w http.ResponseWriter, r *http.Request, res *resource, upload []byte, info store.ObjectInfo) {
	if res.acc < accWrite {
		needPrivileges(w)
		return
	}
	cond := store.Cond{}
	if r.Header.Get("If-Match") != "" {
		cond.IfMatch = info.ETag // checked against the per-user ETag already
	}
	ni, _, err := res.col.Put(res.name, upload, cond)
	if err != nil {
		s.httpError(w, err)
		return
	}
	w.Header().Set("ETag", ni.ETag)
	w.WriteHeader(http.StatusNoContent)
}
