package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/carddav"
	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/store"
)

func badRequest(w http.ResponseWriter, msg string) { http.Error(w, msg, http.StatusBadRequest) }

func (s *Server) propfind(w http.ResponseWriter, r *http.Request, res *resource) {
	if res.kind == kCollection && res.col == nil || res.kind == kObject && !res.exists {
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
	exp := caldav.NewExpandBudget(r.Context())
	ms.Responses = append(ms.Responses, s.responseFor(user, res, reqProps, names, exp))
	if depth == 1 && res.isCollection() {
		kids, err := s.children(res)
		if err != nil {
			s.httpError(w, err)
			return
		}
		for _, k := range kids {
			ms.Responses = append(ms.Responses, s.responseFor(user, k, reqProps, names, exp))
		}
	}
	writeMultistatus(r.Context(), w, ms)
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
	if res.kind != kCollection || res.col == nil || res.col.ReadOnly() {
		if res.kind == kCollection && res.col == nil || res.kind == kObject && !res.exists {
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
	if res.isShared() {
		// A sharee only changes their own view (name, color, order).
		err = s.store.UpdateSharePrefs(res.owner, res.col.Key(), func(p *store.SharePrefs) {
			trial := *p
			for i, el := range els {
				statuses[i] = applySharedProp(&trial, el, removes[i])
				failed = failed || statuses[i] != http.StatusOK
			}
			if !failed {
				*p = trial
			}
		})
	} else {
		err = res.col.UpdateMeta(func(m *store.Meta) {
			trial := *m
			trial.DeadProps = maps.Clone(m.DeadProps)
			for i, el := range els {
				statuses[i] = applyProp(&trial, res.sk, el.Name, el, removes[i])
				failed = failed || statuses[i] != http.StatusOK
			}
			if !failed {
				*m = trial
			}
		})
	}
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

// mkcollection handles MKCALENDAR (RFC 4791 §5.3.1) and extended MKCOL
// (RFC 5689) with a calendar or addressbook resourcetype matching the home.
func (s *Server) mkcollection(w http.ResponseWriter, r *http.Request, res *resource, extended bool) {
	if res.kind != kCollection || !extended && !res.isCalendar() {
		http.Error(w, "collections can only be created directly in a home of the matching type", http.StatusForbidden)
		return
	}
	if res.col != nil {
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, DELETE, PROPFIND, PROPPATCH, REPORT")
		http.Error(w, "already exists", http.StatusMethodNotAllowed)
		return
	}
	if !store.ValidCollectionName(res.colName) {
		http.Error(w, `invalid collection name ("~" is reserved)`, http.StatusForbidden)
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
	typed := !extended // MKCALENDAR implies the type; MKCOL must name it
	resp := &dav.Response{Href: res.href()}
	failed := false
	for _, el := range els {
		st := http.StatusOK
		switch {
		case el.Name == pResourceType:
			if res.isCalendar() {
				typed = el.Child(dav.NSCalDAV, "calendar") != nil
			} else {
				typed = el.Child(dav.NSCardDAV, "addressbook") != nil
			}
			if !typed {
				st = http.StatusForbidden
			}
		case el.Name == pSupportedCompSet && res.isCalendar():
			for _, c := range el.ChildrenNamed(dav.NSCalDAV, "comp") {
				name := strings.ToUpper(c.Attr("name"))
				if !isKnownComp(name) {
					st = http.StatusForbidden
				}
				m.Components = append(m.Components, name)
			}
		default:
			st = applyProp(&m, res.sk, el.Name, el, false)
		}
		failed = failed || st != http.StatusOK
		resp.AddProp(st, dav.Prop{Name: el.Name})
	}
	if !typed {
		http.Error(w, "only calendar or address book collections can be created", http.StatusForbidden)
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
	if _, err := s.store.CreateCollection(res.owner, res.sk, res.colName, m); err != nil {
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

// writeMultistatus sends ms, unless the request ran out of time: then
// expansions may have stopped early and ms may be incomplete.
func writeMultistatus(ctx context.Context, w http.ResponseWriter, ms *dav.Multistatus) {
	if ctx.Err() != nil {
		tooSlow(w)
		return
	}
	ms.Write(w)
}

// tooSlow answers a request that ran past workTimeout.
func tooSlow(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	http.Error(w, "the request took too long", http.StatusServiceUnavailable)
}

// needPrivileges answers a write on a read-only share.
func needPrivileges(w http.ResponseWriter) {
	dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSDAV, "need-privileges"), "read-only share")
}

// applySharedProp applies a PROPPATCH from a sharee to their own view of
// the collection: only name, color and order can be changed.
func applySharedProp(p *store.SharePrefs, el *dav.Element, remove bool) int {
	text := ""
	if !remove {
		text = strings.TrimSpace(el.Text)
	}
	switch el.Name {
	case pDisplayName:
		p.DisplayName = text
	case pCalendarColor:
		p.Color = text
	case pCalendarOrder:
		p.Order = text
	default:
		return http.StatusForbidden
	}
	return http.StatusOK
}

func isKnownComp(name string) bool {
	switch name {
	case "VEVENT", "VTODO", "VJOURNAL", "VFREEBUSY":
		return true
	}
	return false
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, res *resource) {
	if res.kind != kObject {
		w.Header().Set("Allow", "OPTIONS, PROPFIND, PROPPATCH, REPORT, DELETE")
		http.Error(w, "GET is only supported on calendar objects and contacts", http.StatusMethodNotAllowed)
		return
	}
	if !res.exists {
		http.NotFound(w, r)
		return
	}
	data, info, err := s.objectData(res)
	if err != nil {
		s.httpError(w, err)
		return
	}
	etag := s.etagOf(res)
	h := w.Header()
	h.Set("Content-Type", kinds[res.sk].contentType)
	h.Set("ETag", etag)
	h.Set("Last-Modified", info.ModTime.Format(http.TimeFormat))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(etag, inm) {
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
	if res.kind != kObject {
		http.Error(w, "PUT is only supported on calendar objects and contacts", http.StatusMethodNotAllowed)
		return
	}
	if res.col == nil {
		http.Error(w, "collection does not exist", http.StatusConflict)
		return
	}
	if !store.ValidName(res.name) {
		badRequest(w, "invalid resource name")
		return
	}
	// In a calendar someone else owns, per-user changes need only read
	// access (putPerUser checks the rest).
	if res.acc < accWrite && !s.perUserApplies(res) {
		needPrivileges(w)
		return
	}
	ns := dav.NSCalDAV
	if !res.isCalendar() {
		ns = dav.NSCardDAV
	}
	if !contentTypeOK(res.sk, r.Header.Get("Content-Type")) {
		pre := "supported-calendar-data"
		if !res.isCalendar() {
			pre = "supported-address-data"
		}
		dav.WriteError(w, http.StatusUnsupportedMediaType, dav.N(ns, pre), "unexpected content type")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		s.httpError(w, err)
		return
	}
	if pre, err := validate(res, data); err != nil {
		dav.WriteError(w, http.StatusForbidden, dav.N(ns, pre), err.Error())
		return
	}
	if s.perUserApplies(res) {
		s.putPerUser(w, r, res, data)
		return
	}
	info, created, err := res.col.Put(res.name, data, conds(r))
	var uidErr *store.UIDConflictError
	if errors.As(err, &uidErr) {
		href := res.object(store.ObjectInfo{Name: uidErr.Existing}, true).href()
		w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+`<d:error xmlns:d="DAV:" xmlns:x="%s"><x:no-uid-conflict>%s</x:no-uid-conflict></d:error>`+"\n",
			ns, dav.Href(href))
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

func contentTypeOK(sk store.Kind, ct string) bool {
	if ct == "" {
		return true
	}
	ct = strings.ToLower(ct)
	if sk == store.Calendars {
		return strings.HasPrefix(ct, "text/calendar")
	}
	return strings.HasPrefix(ct, "text/vcard") || strings.HasPrefix(ct, "text/x-vcard")
}

// validate checks a PUT body and returns the failed precondition's name.
func validate(res *resource, data []byte) (string, error) {
	if !res.isCalendar() {
		if _, err := carddav.Validate(data); err != nil {
			if errors.Is(err, carddav.ErrUnsupportedData) {
				return "supported-address-data", err
			}
			return "valid-address-data", err
		}
		return "", nil
	}
	cal, err := ical.ParseBytes(data)
	if err != nil {
		return "valid-calendar-data", err
	}
	if _, _, err := caldav.Validate(cal, res.col.Meta().Components); err != nil {
		switch {
		case errors.Is(err, caldav.ErrInvalidData):
			return "valid-calendar-data", err
		case errors.Is(err, caldav.ErrUnsupportedComponent):
			return "supported-calendar-component", err
		}
		return "valid-calendar-object-resource", err
	}
	return "", nil
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request, res *resource) {
	switch res.kind {
	case kObject:
		if !res.exists {
			http.NotFound(w, r)
			return
		}
		if res.acc < accWrite {
			needPrivileges(w)
			return
		}
		if err := res.col.Delete(res.name, conds(r)); err != nil {
			s.httpError(w, err)
			return
		}
	case kCollection:
		if res.col == nil {
			http.NotFound(w, r)
			return
		}
		if res.isShared() {
			// A sharee "deleting" a shared collection hides it for them.
			if err := s.store.UpdateSharePrefs(res.owner, res.col.Key(), func(p *store.SharePrefs) { p.Hidden = true }); err != nil {
				s.httpError(w, err)
				return
			}
			break
		}
		if res.col.ReadOnly() {
			needPrivileges(w)
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
	case dav.N(dav.NSCalDAV, "calendar-multiget"), dav.N(dav.NSCardDAV, "addressbook-multiget"):
		s.multiget(r.Context(), w, user, res, body)
	case dav.N(dav.NSCalDAV, "calendar-query"):
		s.calendarQuery(w, r, user, res, body)
	case dav.N(dav.NSCardDAV, "addressbook-query"):
		s.addressbookQuery(w, r, user, res, body)
	case dav.N(dav.NSCalDAV, "free-busy-query"):
		s.freeBusyQuery(r.Context(), w, res, body)
	case dav.N(dav.NSDAV, "sync-collection"):
		s.syncCollection(r.Context(), w, user, res, body)
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

func (s *Server) multiget(ctx context.Context, w http.ResponseWriter, user string, res *resource, body *dav.Element) {
	props := propElements(body)
	ms := &dav.Multistatus{}
	exp := caldav.NewExpandBudget(ctx)
	for _, h := range body.ChildrenNamed(dav.NSDAV, "href") {
		raw := strings.TrimSpace(h.Text)
		p := raw
		if u, err := url.Parse(raw); err == nil {
			p = u.Path
		}
		target, err := s.resolve(p, user)
		if err != nil || target.kind != kObject || !target.exists || target.sk != res.sk {
			st := http.StatusNotFound
			if errors.Is(err, errForbidden) {
				st = http.StatusForbidden
			}
			ms.Responses = append(ms.Responses, &dav.Response{Href: p, Status: st})
			continue
		}
		resp := s.responseFor(user, target, props, false, exp)
		resp.Href = p // echo what the client asked for
		ms.Responses = append(ms.Responses, resp)
	}
	writeMultistatus(ctx, w, ms)
}

// queryTargets returns the objects a query REPORT runs over: the members
// of a collection (Depth: 1) or a single object.
func (s *Server) queryTargets(w http.ResponseWriter, r *http.Request, res *resource, wantCal bool) ([]*resource, bool) {
	if res.isCalendar() != wantCal {
		dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSDAV, "supported-report"), "report does not match collection type")
		return nil, false
	}
	switch {
	case res.kind == kCollection && res.col != nil:
		depth, err := dav.Depth(r, 0)
		if err != nil {
			badRequest(w, err.Error())
			return nil, false
		}
		if depth == 0 {
			return nil, true
		}
		objs, err := s.children(res)
		if err != nil {
			s.httpError(w, err)
			return nil, false
		}
		return objs, true
	case res.kind == kObject && res.exists:
		return []*resource{res}, true
	}
	http.NotFound(w, r)
	return nil, false
}

func (s *Server) calendarQuery(w http.ResponseWriter, r *http.Request, user string, res *resource, body *dav.Element) {
	objs, ok := s.queryTargets(w, r, res, true)
	if !ok {
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
	q := &caldav.Query{Filter: filter, Floating: floating, Ctx: r.Context()}
	props := propElements(body)
	ms := &dav.Multistatus{}
	exp := caldav.NewExpandBudget(r.Context())
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
			ms.Responses = append(ms.Responses, s.responseFor(user, o, props, false, exp))
		}
	}
	writeMultistatus(r.Context(), w, ms)
}

// addressbookQuery implements CARDDAV:addressbook-query (RFC 6352 §8.6).
func (s *Server) addressbookQuery(w http.ResponseWriter, r *http.Request, user string, res *resource, body *dav.Element) {
	objs, ok := s.queryTargets(w, r, res, false)
	if !ok {
		return
	}
	filter := body.Child(dav.NSCardDAV, "filter")
	limit := 0
	if l := body.Child(dav.NSCardDAV, "limit"); l != nil {
		n, err := strconv.Atoi(strings.TrimSpace(l.Child(dav.NSCardDAV, "nresults").Text))
		if err != nil || n < 1 {
			badRequest(w, "bad limit")
			return
		}
		limit = n
	}
	props := propElements(body)
	ms := &dav.Multistatus{}
	exp := caldav.NewExpandBudget(r.Context())
	matched := 0
	for _, o := range objs {
		data, _, err := o.col.Get(o.name)
		if err != nil {
			continue // deleted concurrently
		}
		card, err := ical.ParseBytes(data)
		if err != nil {
			s.log.Warn("unparsable object", "href", o.href(), "err", err)
			continue
		}
		ok, err := carddav.Match(card, filter)
		if err != nil {
			dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSCardDAV, "valid-filter"), err.Error())
			return
		}
		if !ok {
			continue
		}
		if limit > 0 && matched == limit {
			ms.Responses = append(ms.Responses, &dav.Response{
				Href:   res.href(),
				Status: http.StatusInsufficientStorage,
				Error:  dav.E(dav.N(dav.NSDAV, "number-of-matches-within-limits"), ""),
			})
			break
		}
		matched++
		ms.Responses = append(ms.Responses, s.responseFor(user, o, props, false, exp))
	}
	writeMultistatus(r.Context(), w, ms)
}

// freeBusyQuery implements CALDAV:free-busy-query (RFC 4791 §7.10) on a
// calendar: a VFREEBUSY of the busy time within the requested range.
func (s *Server) freeBusyQuery(ctx context.Context, w http.ResponseWriter, res *resource, body *dav.Element) {
	if res.kind != kCollection || res.col == nil || !res.isCalendar() {
		dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSDAV, "supported-report"), "free-busy-query is only supported on calendars")
		return
	}
	tr := body.Child(dav.NSCalDAV, "time-range")
	if tr == nil || tr.Attr("start") == "" || tr.Attr("end") == "" {
		badRequest(w, "free-busy-query needs a time-range with start and end")
		return
	}
	from, to, err := caldav.ParseTimeRange(tr)
	if err != nil || !to.After(from) {
		badRequest(w, "invalid time-range")
		return
	}
	var cals []*ical.Component
	for _, o := range res.col.List() {
		data, _, err := res.col.Get(o.Name)
		if err != nil {
			continue
		}
		if cal, err := ical.ParseBytes(data); err == nil {
			cals = append(cals, cal)
		}
	}
	floating := caldav.FloatingZone(res.col.Meta().Timezone)
	ps, err := caldav.FreeBusy(ctx, cals, from, to, floating)
	if ctx.Err() != nil {
		tooSlow(w)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
		return
	}
	out := caldav.FreeBusyCalendar(ps, from, to, time.Now()).Bytes()
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.Write(out)
}
