package server

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"davd/internal/caldav"
	"davd/internal/dav"
	"davd/internal/ical"
	"davd/internal/store"
)

func badRequest(w http.ResponseWriter, msg string) { http.Error(w, msg, http.StatusBadRequest) }

func (s *Server) propfind(w http.ResponseWriter, r *http.Request, res *resource) {
	if res.kind == kCalendar && res.col == nil || res.kind == kCalObject && !res.exists {
		http.NotFound(w, r)
		return
	}
	depth, err := dav.Depth(r, -1)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if depth < 0 {
		depth = 1 // treat infinity as 1 rather than walking everything
	}
	body, err := dav.ParseXML(r.Body)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	var (
		reqProps []*dav.Element
		names    bool
	)
	if body != nil {
		if body.Name != dav.N(dav.NSDAV, "propfind") {
			badRequest(w, "expected DAV:propfind")
			return
		}
		switch {
		case body.Child(dav.NSDAV, "prop") != nil:
			reqProps = body.Child(dav.NSDAV, "prop").Children
			if reqProps == nil {
				reqProps = []*dav.Element{}
			}
		case body.Child(dav.NSDAV, "propname") != nil:
			names = true
		}
	}
	user := userOf(r)
	ms := &dav.Multistatus{}
	ms.Responses = append(ms.Responses, s.responseFor(user, res, reqProps, names))
	if depth == 1 && res.isCollection() {
		kids, err := s.children(res)
		if err != nil {
			s.httpError(w, err)
			return
		}
		for _, k := range kids {
			ms.Responses = append(ms.Responses, s.responseFor(user, k, reqProps, names))
		}
	}
	ms.Write(w)
}

func userOf(r *http.Request) string {
	u, _, _ := r.BasicAuth()
	return u
}

// parseSetRemove extracts (element, remove) pairs from a propertyupdate /
// mkcalendar / mkcol body in document order.
func parseSetRemove(body *dav.Element) (sets []*dav.Element, removes []bool) {
	for _, op := range body.Children {
		remove := op.Name == dav.N(dav.NSDAV, "remove")
		if !remove && op.Name != dav.N(dav.NSDAV, "set") {
			continue
		}
		for _, p := range op.ChildrenNamed(dav.NSDAV, "prop") {
			for _, el := range p.Children {
				sets = append(sets, el)
				removes = append(removes, remove)
			}
		}
	}
	return
}

func (s *Server) proppatch(w http.ResponseWriter, r *http.Request, res *resource) {
	body, err := dav.ParseXML(r.Body)
	if err != nil || body == nil || body.Name != dav.N(dav.NSDAV, "propertyupdate") {
		badRequest(w, "expected DAV:propertyupdate")
		return
	}
	els, removes := parseSetRemove(body)
	resp := &dav.Response{Href: res.href()}
	if res.kind != kCalendar || res.col == nil {
		if res.kind == kCalendar || res.kind == kCalObject && !res.exists {
			http.NotFound(w, r)
			return
		}
		for _, el := range els {
			resp.AddProp(http.StatusForbidden, dav.Prop{Name: el.Name})
		}
		(&dav.Multistatus{Responses: []*dav.Response{resp}}).Write(w)
		return
	}
	// All-or-nothing (RFC 4918 §9.2): apply to a copy, commit only if
	// every property succeeded.
	statuses := make([]int, len(els))
	failed := false
	err = res.col.UpdateMeta(func(m *store.Meta) {
		trial := *m
		trial.DeadProps = maps.Clone(m.DeadProps)
		for i, el := range els {
			statuses[i] = applyProp(&trial, el.Name, el, removes[i])
			failed = failed || statuses[i] != http.StatusOK
		}
		if !failed {
			*m = trial
		}
	})
	if err != nil {
		s.httpError(w, err)
		return
	}
	for i, el := range els {
		st := statuses[i]
		if failed && st == http.StatusOK {
			st = http.StatusFailedDependency
		}
		resp.AddProp(st, dav.Prop{Name: el.Name})
	}
	(&dav.Multistatus{Responses: []*dav.Response{resp}}).Write(w)
}

// mkcalendar handles MKCALENDAR (RFC 4791 §5.3.1) and extended MKCOL
// (RFC 5689) with a calendar resourcetype.
func (s *Server) mkcalendar(w http.ResponseWriter, r *http.Request, res *resource, extended bool) {
	if res.kind != kCalendar {
		http.Error(w, "calendars can only be created in the calendar home", http.StatusForbidden)
		return
	}
	if res.col != nil {
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, DELETE, PROPFIND, PROPPATCH, REPORT")
		http.Error(w, "already exists", http.StatusMethodNotAllowed)
		return
	}
	body, err := dav.ParseXML(r.Body)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	var els []*dav.Element
	if body != nil {
		want := dav.N(dav.NSCalDAV, "mkcalendar")
		if extended {
			want = dav.N(dav.NSDAV, "mkcol")
		}
		if body.Name != want {
			badRequest(w, "unexpected body "+body.Name.Local)
			return
		}
		var removes []bool
		els, removes = parseSetRemove(body)
		for _, rm := range removes {
			if rm {
				badRequest(w, "remove not allowed here")
				return
			}
		}
	}
	m := store.Meta{}
	isCalendar := !extended
	resp := &dav.Response{Href: res.href()}
	failed := false
	for _, el := range els {
		st := http.StatusOK
		switch el.Name {
		case pResourceType:
			isCalendar = el.Child(dav.NSCalDAV, "calendar") != nil
			if !isCalendar {
				st = http.StatusForbidden
			}
		case pSupportedCompSet:
			for _, c := range el.ChildrenNamed(dav.NSCalDAV, "comp") {
				name := strings.ToUpper(c.Attr("name"))
				if !isKnownComp(name) {
					st = http.StatusForbidden
				}
				m.Components = append(m.Components, name)
			}
		default:
			st = applyProp(&m, el.Name, el, false)
		}
		failed = failed || st != http.StatusOK
		resp.AddProp(st, dav.Prop{Name: el.Name})
	}
	if extended && !isCalendar {
		http.Error(w, "only calendar collections can be created", http.StatusForbidden)
		return
	}
	if failed {
		for st, props := range resp.Props {
			if st == http.StatusOK {
				delete(resp.Props, st)
				for _, p := range props {
					resp.AddProp(http.StatusFailedDependency, p)
				}
			}
		}
		(&dav.Multistatus{Responses: []*dav.Response{resp}}).Write(w)
		return
	}
	if _, err := s.store.CreateCollection(res.owner, store.Calendars, res.colName, m); err != nil {
		if errors.Is(err, store.ErrExists) {
			http.Error(w, "already exists", http.StatusMethodNotAllowed)
			return
		}
		s.httpError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusCreated)
}

func isKnownComp(name string) bool {
	switch name {
	case "VEVENT", "VTODO", "VJOURNAL", "VFREEBUSY":
		return true
	}
	return false
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, res *resource) {
	if res.kind != kCalObject {
		w.Header().Set("Allow", "OPTIONS, PROPFIND, PROPPATCH, REPORT, DELETE")
		http.Error(w, "GET is only supported on calendar objects", http.StatusMethodNotAllowed)
		return
	}
	if !res.exists {
		http.NotFound(w, r)
		return
	}
	data, info, err := res.col.Get(res.name)
	if err != nil {
		s.httpError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/calendar; charset=utf-8")
	h.Set("ETag", info.ETag)
	h.Set("Last-Modified", info.ModTime.Format(http.TimeFormat))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(info.ETag, inm) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(data)
	}
}

func etagMatch(etag, list string) bool {
	for t := range strings.SplitSeq(list, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}

func conds(r *http.Request) store.Cond {
	return store.Cond{IfMatch: r.Header.Get("If-Match"), IfNoneMatch: r.Header.Get("If-None-Match")}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, res *resource) {
	if res.kind != kCalObject {
		http.Error(w, "PUT is only supported on calendar objects", http.StatusMethodNotAllowed)
		return
	}
	if res.col == nil {
		http.Error(w, "calendar does not exist", http.StatusConflict)
		return
	}
	if !store.ValidName(res.name) {
		badRequest(w, "invalid resource name")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "text/calendar") {
		dav.WriteError(w, http.StatusUnsupportedMediaType, dav.N(dav.NSCalDAV, "supported-calendar-data"), "expected text/calendar")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		s.httpError(w, err)
		return
	}
	cal, err := ical.ParseBytes(data)
	if err != nil {
		dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSCalDAV, "valid-calendar-data"), err.Error())
		return
	}
	if _, _, err := caldav.Validate(cal, res.col.Meta().Components); err != nil {
		pre := "valid-calendar-object-resource"
		switch {
		case errors.Is(err, caldav.ErrInvalidData):
			pre = "valid-calendar-data"
		case errors.Is(err, caldav.ErrUnsupportedComponent):
			pre = "supported-calendar-component"
		}
		dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSCalDAV, pre), err.Error())
		return
	}
	info, created, err := res.col.Put(res.name, data, conds(r))
	var uidErr *store.UIDConflictError
	if errors.As(err, &uidErr) {
		href := calHomeHref(res.owner) + res.colName + "/" + uidErr.Existing
		w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+`<d:error xmlns:d="DAV:" xmlns:cal="%s"><cal:no-uid-conflict>%s</cal:no-uid-conflict></d:error>`+"\n",
			dav.NSCalDAV, dav.Href(href))
		return
	}
	if err != nil {
		s.httpError(w, err)
		return
	}
	w.Header().Set("ETag", info.ETag)
	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request, res *resource) {
	switch res.kind {
	case kCalObject:
		if !res.exists {
			http.NotFound(w, r)
			return
		}
		if err := res.col.Delete(res.name, conds(r)); err != nil {
			s.httpError(w, err)
			return
		}
	case kCalendar:
		if res.col == nil {
			http.NotFound(w, r)
			return
		}
		if err := s.store.DeleteCollection(res.col); err != nil {
			s.httpError(w, err)
			return
		}
	default:
		http.Error(w, "cannot delete this resource", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// report dispatches REPORT bodies.
func (s *Server) report(w http.ResponseWriter, r *http.Request, res *resource) {
	body, err := dav.ParseXML(r.Body)
	if err != nil || body == nil {
		badRequest(w, "missing or malformed REPORT body")
		return
	}
	user := userOf(r)
	switch body.Name {
	case dav.N(dav.NSCalDAV, "calendar-multiget"):
		s.multiget(w, user, res, body)
	case dav.N(dav.NSCalDAV, "calendar-query"):
		s.calendarQuery(w, r, user, res, body)
	case dav.N(dav.NSDAV, "principal-search-property-set"):
		w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
		io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+`<d:principal-search-property-set xmlns:d="DAV:"/>`+"\n")
	case dav.N(dav.NSDAV, "principal-property-search"), dav.N(dav.NSDAV, "expand-property"):
		(&dav.Multistatus{}).Write(w)
	default:
		dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSDAV, "supported-report"), "unsupported report "+body.Name.Local)
	}
}

func propElements(body *dav.Element) []*dav.Element {
	if p := body.Child(dav.NSDAV, "prop"); p != nil {
		return p.Children
	}
	return nil // allprop
}

func (s *Server) multiget(w http.ResponseWriter, user string, res *resource, body *dav.Element) {
	props := propElements(body)
	ms := &dav.Multistatus{}
	for _, h := range body.ChildrenNamed(dav.NSDAV, "href") {
		raw := strings.TrimSpace(h.Text)
		p := raw
		if u, err := url.Parse(raw); err == nil {
			p = u.Path
		}
		target, err := s.resolve(p, user)
		if err != nil || target.kind != kCalObject || !target.exists {
			st := http.StatusNotFound
			if errors.Is(err, errForbidden) {
				st = http.StatusForbidden
			}
			ms.Responses = append(ms.Responses, &dav.Response{Href: p, Status: st})
			continue
		}
		resp := s.responseFor(user, target, props, false)
		resp.Href = p // echo what the client asked for
		ms.Responses = append(ms.Responses, resp)
	}
	ms.Write(w)
}

func (s *Server) calendarQuery(w http.ResponseWriter, r *http.Request, user string, res *resource, body *dav.Element) {
	var objs []*resource
	switch {
	case res.kind == kCalendar && res.col != nil:
		depth, err := dav.Depth(r, 0)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		if depth != 0 {
			objs, _ = s.children(res)
		}
	case res.kind == kCalObject && res.exists:
		objs = []*resource{res}
	default:
		http.NotFound(w, r)
		return
	}
	filter := body.Child(dav.NSCalDAV, "filter")
	if filter == nil {
		dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSCalDAV, "valid-filter"), "missing filter")
		return
	}
	var floating = caldav.FloatingZone(res.col.Meta().Timezone)
	if tz := body.Child(dav.NSCalDAV, "timezone"); tz != nil {
		if loc := caldav.FloatingZone(tz.Text); loc != nil {
			floating = loc
		}
	}
	q := &caldav.Query{Filter: filter, Floating: floating}
	props := propElements(body)
	ms := &dav.Multistatus{}
	for _, o := range objs {
		data, _, err := o.col.Get(o.name)
		if err != nil {
			continue // deleted concurrently
		}
		cal, err := ical.ParseBytes(data)
		if err != nil {
			s.log.Warn("unparsable object", "href", o.href(), "err", err)
			continue
		}
		ok, err := q.Match(cal)
		if err != nil {
			dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSCalDAV, "valid-filter"), err.Error())
			return
		}
		if ok {
			ms.Responses = append(ms.Responses, s.responseFor(user, o, props, false))
		}
	}
	ms.Write(w)
}
