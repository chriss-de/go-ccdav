// Package server is the HTTP front: routing, authentication and the
// WebDAV / CalDAV / CardDAV methods on top of store, dav, caldav and carddav.
//
// URL layout, below the configurable DAV prefix (default /dav/):
//
//	/.well-known/caldav, carddav     -> 301 <prefix>
//	<prefix>                         root (current-user-principal discovery)
//	<prefix>principals/<user>/       principal
//	<prefix>principals/groups/<g>/   group principal (config.yaml groups)
//	<prefix>calendars/<user>/        calendar home
//	<prefix>calendars/<user>/<cal>/  calendar (own)
//	<prefix>calendars/<user>/<owner>~<cal>/  calendar shared with <user>
//	<prefix>calendars/.../<x>        calendar object (.ics)
//	<prefix>addressbooks/...         the same for address books (.vcf)
//
// Nothing else is served by this handler; "/" is 404.
package server

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/auth"
	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/carddav"
	"github.com/chriss-de/go-ccdav/internal/clientip"
	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/store"
)

const maxResourceSize = 10 << 20

type Server struct {
	prefix string // DAV prefix, fixed for the server's lifetime
	cfg    *config.Store
	store  *store.Store
	basic  *auth.Basic
	log    *slog.Logger
}

// New creates the DAV handler. ap and lim are shared with the web UI.
func New(cfg *config.Store, st *store.Store, ap *auth.AppPasswords, lim *auth.Limiter, log *slog.Logger) *Server {
	return &Server{prefix: cfg.Get().DAVPrefix, cfg: cfg, store: st,
		basic: auth.NewBasic(cfg.Password, ap, lim, log, "dav"), log: log}
}

// DefaultCalendars are created in a user's calendar home on first access:
// one "Personal" calendar for events, tasks and journal entries. (Apple
// Reminders prefers tasks in a calendar of their own; users can create
// one in the web UI.)
var DefaultCalendars = map[string]store.Meta{
	"personal": {DisplayName: "Personal", Color: "#3a87ad", Components: []string{"VEVENT", "VTODO", "VJOURNAL"}},
}

// DefaultAddressBooks are created in a user's address book home on first
// access.
var DefaultAddressBooks = map[string]store.Meta{
	"contacts": {DisplayName: "Contacts"},
}

// UIDOf is the store.UIDFunc for this server.
func UIDOf(k store.Kind, data []byte) (uid, comp string) {
	if k == store.AddressBooks {
		return carddav.UIDOf(data), ""
	}
	return caldav.UIDOf(data)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	ip := clientip.From(r, s.cfg.Get().Trusted)
	user := s.serve(sw, r, ip)
	s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
		"user", user, "ip", ip, "dur_ms", float64(time.Since(start).Microseconds())/1000)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, ip string) (user string) {
	w.Header().Set("Server", "ccdav")
	p := r.URL.Path
	switch {
	case p == "/.well-known/caldav" || p == "/.well-known/carddav":
		http.Redirect(w, r, s.prefix, http.StatusMovedPermanently)
		return ""
	case p+"/" == s.prefix || strings.HasPrefix(p, s.prefix):
	default:
		http.NotFound(w, r)
		return ""
	}
	if r.Method == http.MethodOptions {
		s.options(w)
		return ""
	}
	user, ok := s.basic.Authenticate(w, r, ip)
	if !ok {
		return ""
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxResourceSize)
	res, err := s.resolve(p, user)
	if err != nil {
		s.httpError(w, err)
		return user
	}
	switch r.Method {
	case "PROPFIND":
		s.propfind(w, r, res)
	case "PROPPATCH":
		s.proppatch(w, r, res)
	case "MKCALENDAR":
		s.mkcollection(w, r, res, false)
	case "MKCOL":
		s.mkcollection(w, r, res, true)
	case http.MethodGet, http.MethodHead:
		s.get(w, r, res)
	case http.MethodPut:
		s.put(w, r, res)
	case http.MethodDelete:
		s.delete(w, r, res)
	case "REPORT":
		s.report(w, r, res)
	default:
		w.Header().Set("Allow", allowed)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	return user
}

const allowed = "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCOL, MKCALENDAR"

func (s *Server) options(w http.ResponseWriter) {
	w.Header().Set("Allow", allowed)
	w.Header().Set("DAV", "1, 3, calendar-access, addressbook, extended-mkcol")
	w.WriteHeader(http.StatusOK)
}

type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

func errStatus(code int, msg string) error { return &httpErr{code, msg} }

func (s *Server) httpError(w http.ResponseWriter, err error) {
	var he *httpErr
	switch {
	case errors.As(err, &he):
		http.Error(w, he.msg, he.code)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrPrecondition):
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
	case errors.Is(err, store.ErrReadOnly):
		needPrivileges(w)
	case errors.Is(err, store.ErrBadName):
		http.Error(w, "invalid name", http.StatusBadRequest)
	default:
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		s.log.Error("internal error", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Recover turns a panic in next into a logged 500 instead of a dropped
// connection, so one bad request shows up with its method, path and stack.
func Recover(log *slog.Logger, cfg *config.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			log.Error("panic", "method", r.Method, "path", r.URL.Path,
				"ip", clientip.From(r, cfg.Get().Trusted), "err", v, "stack", string(debug.Stack()))
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}
