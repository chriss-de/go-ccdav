// Package server is the HTTP front: routing, authentication and the WebDAV
// / CalDAV methods on top of store, dav and caldav.
//
// URL layout:
//
//	/.well-known/caldav             -> 301 /dav/
//	/dav/                           root (current-user-principal discovery)
//	/dav/principals/<user>/         principal
//	/dav/calendars/<user>/          calendar home
//	/dav/calendars/<user>/<cal>/    calendar collection
//	/dav/calendars/<user>/<cal>/<x> calendar object
package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"davd/internal/auth"
	"davd/internal/caldav"
	"davd/internal/config"
	"davd/internal/store"
)

const maxResourceSize = 10 << 20

type Server struct {
	cfg   *config.Store
	store *store.Store
	auth  *auth.Cache
	log   *slog.Logger
}

func New(cfg *config.Store, st *store.Store, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: st, auth: auth.NewCache(15 * time.Minute), log: log}
}

// DefaultCalendars are created in a user's calendar home on first access.
// Apple expects tasks in a separate collection, so events and tasks are split.
var DefaultCalendars = map[string]store.Meta{
	"default": {DisplayName: "Calendar", Components: []string{"VEVENT"}},
	"tasks":   {DisplayName: "Tasks", Components: []string{"VTODO"}},
	"journal": {DisplayName: "Journal", Components: []string{"VJOURNAL"}},
}

// UIDOf is the store.UIDFunc for this server.
func UIDOf(_ store.Kind, data []byte) string { return caldav.UIDOf(data) }

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
	user := s.serve(sw, r)
	s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
		"user", user, "dur", time.Since(start).Round(time.Microsecond))
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) (user string) {
	w.Header().Set("Server", "davd")
	p := r.URL.Path
	switch {
	case p == "/.well-known/caldav" || p == "/.well-known/carddav":
		http.Redirect(w, r, "/dav/", http.StatusMovedPermanently)
		return ""
	case p == "/" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("davd: CalDAV server. Point your client at this host.\n"))
		return ""
	case p == "/" || p == "/dav" || strings.HasPrefix(p, "/dav/"):
	default:
		http.NotFound(w, r)
		return ""
	}
	if r.Method == http.MethodOptions {
		s.options(w)
		return ""
	}
	user, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="davd", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
		s.mkcalendar(w, r, res, false)
	case "MKCOL":
		s.mkcalendar(w, r, res, true)
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
	w.Header().Set("DAV", "1, 3, calendar-access, extended-mkcol")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) authenticate(r *http.Request) (string, bool) {
	name, pw, ok := r.BasicAuth()
	if !ok {
		return "", false
	}
	u, ok := s.cfg.Get().Users[name]
	if !ok || u.Password == "" {
		// Burn comparable time so user enumeration via timing is harder.
		auth.Verify("pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", pw)
		return "", false
	}
	if !s.auth.Verify(name, u.Password, pw) {
		return "", false
	}
	return name, true
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
