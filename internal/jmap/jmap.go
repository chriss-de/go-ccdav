// Package jmap serves JMAP (RFC 8620): the session resource, the API
// endpoint and blob upload and download. Data types (contacts, calendars)
// register their methods with Register; on its own the handler offers
// Core/echo.
//
// URL layout, below the configurable JMAP prefix (default /jmap/):
//
//	/.well-known/jmap                                  -> 301 <prefix>session
//	<prefix>session                                    session object
//	<prefix>api                                        API requests (POST)
//	<prefix>upload/<accountId>/                        blob upload (POST)
//	<prefix>download/<accountId>/<blobId>/<name>?type= blob download
//	<prefix>eventsource                                push (EventSource)
//
// Every user is one account, with the id "u" + hex(user name). A user
// sees their own account and the account of everyone who shared a
// collection with them.
package jmap

import (
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chriss-de/go-ccdav/internal/auth"
	"github.com/chriss-de/go-ccdav/internal/clientip"
	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// CapCore is the core capability every request uses.
const CapCore = "urn:ietf:params:jmap:core"

// Limits announced in the session and enforced.
const (
	maxSizeUpload         = 50 << 20
	maxConcurrentUpload   = 4
	maxSizeRequest        = 10 << 20
	maxConcurrentRequests = 4
	maxCallsInRequest     = 32
	MaxObjectsInGet       = 500
	MaxObjectsInSet       = 500
)

type Handler struct {
	prefix   string // fixed for the server's lifetime
	cfg      *config.Store
	store    *store.Store
	defaults map[store.Kind]map[string]store.Meta // created in a new home
	basic    *auth.Basic
	blobs    blobStore
	conv     convCache
	log      *slog.Logger
	caps     map[string]capability
	methods  map[string]method
	busy     sync.Map // user -> chan struct{}: API request slots
	uploads  sync.Map // user -> chan struct{}: upload slots
	streams  sync.Map // user -> chan struct{}: push stream slots
	hub      *hub
}

type capability struct {
	session any               // in the session's capabilities
	account func(account) any // in each account's accountCapabilities (omitted where it returns nil); nil: not an account capability
}

// New creates the JMAP handler. ap and lim are shared with DAV and the
// web UI; defaults are the collections a new home starts with, as for
// DAV. The store must track accounts.
func New(cfg *config.Store, st *store.Store, dataDir string, ap *auth.AppPasswords, lim *auth.Limiter,
	defaults map[store.Kind]map[string]store.Meta, log *slog.Logger) *Handler {
	h := &Handler{
		prefix: cfg.Get().JMAPPrefix, cfg: cfg, store: st, defaults: defaults,
		basic: auth.NewBasic(cfg.Password, ap, lim, log, "jmap"),
		blobs: blobStore{root: dataDir}, log: log,
		caps: map[string]capability{CapCore: {session: coreCapability{
			MaxSizeUpload:         maxSizeUpload,
			MaxConcurrentUpload:   maxConcurrentUpload,
			MaxSizeRequest:        maxSizeRequest,
			MaxConcurrentRequests: maxConcurrentRequests,
			MaxCallsInRequest:     maxCallsInRequest,
			MaxObjectsInGet:       MaxObjectsInGet,
			MaxObjectsInSet:       MaxObjectsInSet,
			CollationAlgorithms:   collations,
		}}},
		methods: map[string]method{},
	}
	h.Register("Core/echo", CapCore, echo)
	h.registerContacts()
	h.registerPrincipals()
	h.hub = newHub(h)
	st.OnChange(func(owner string, _ store.Kind) { h.hub.mark(owner) })
	return h
}

type coreCapability struct {
	MaxSizeUpload         int      `json:"maxSizeUpload"`
	MaxConcurrentUpload   int      `json:"maxConcurrentUpload"`
	MaxSizeRequest        int      `json:"maxSizeRequest"`
	MaxConcurrentRequests int      `json:"maxConcurrentRequests"`
	MaxCallsInRequest     int      `json:"maxCallsInRequest"`
	MaxObjectsInGet       int      `json:"maxObjectsInGet"`
	MaxObjectsInSet       int      `json:"maxObjectsInSet"`
	CollationAlgorithms   []string `json:"collationAlgorithms"`
}

// MethodFunc implements one method. It returns the response arguments,
// or an error: a *MethodError is sent to the client, anything else is
// logged and sent as serverFail.
type MethodFunc func(c *Call, args json.RawMessage) (any, error)

type method struct {
	capability string
	fn         MethodFunc
}

// Register adds a method. The capability must be announced with
// AddCapability (except CapCore) and be in a request's "using".
func (h *Handler) Register(name, capability string, fn MethodFunc) {
	h.methods[name] = method{capability, fn}
}

// AddCapability announces a capability in the session object. With
// perAccount set, it is also an account capability, with the value
// perAccount returns for each account.
func (h *Handler) AddCapability(uri string, session any, perAccount func(account) any) {
	h.caps[uri] = capability{session, perAccount}
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

// Unwrap lets http.ResponseController reach Flush and the deadlines
// (push streams).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	ip := clientip.From(r, h.cfg.Get().Trusted)
	user := h.serve(sw, r, ip)
	h.log.Info("jmap request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
		"user", user, "ip", ip, "dur", time.Since(start).Round(time.Microsecond))
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, ip string) (user string) {
	w.Header().Set("Server", "ccdav")
	if r.URL.Path == "/.well-known/jmap" {
		http.Redirect(w, r, h.prefix+"session", http.StatusMovedPermanently)
		return ""
	}
	rest, ok := strings.CutPrefix(r.URL.Path, h.prefix)
	if !ok {
		http.NotFound(w, r)
		return ""
	}
	user, ok = h.basic.Authenticate(w, r, ip)
	if !ok {
		return ""
	}
	switch {
	case rest == "session":
		if allow(w, r, http.MethodGet, http.MethodHead) {
			h.session(w, r, user)
		}
	case rest == "api":
		if allow(w, r, http.MethodPost) {
			h.api(w, r, user)
		}
	case rest == "eventsource":
		if allow(w, r, http.MethodGet) {
			h.eventSource(w, r, user)
		}
	case strings.HasPrefix(rest, "upload/"):
		acct, ok := strings.CutSuffix(strings.TrimPrefix(rest, "upload/"), "/")
		if !ok || acct == "" || strings.Contains(acct, "/") {
			http.NotFound(w, r)
		} else if allow(w, r, http.MethodPost) {
			h.upload(w, r, user, acct)
		}
	case strings.HasPrefix(rest, "download/"):
		parts := strings.SplitN(strings.TrimPrefix(rest, "download/"), "/", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			http.NotFound(w, r)
		} else if allow(w, r, http.MethodGet, http.MethodHead) {
			h.download(w, r, user, parts[0], parts[1], parts[2])
		}
	default:
		http.NotFound(w, r)
	}
	return user
}

// allow answers 405 unless r uses one of methods.
func allow(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// slot takes one of n concurrent slots of user; release frees it.
func slot(m *sync.Map, user string, n int) (release func(), ok bool) {
	v, _ := m.LoadOrStore(user, make(chan struct{}, n))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}

// AccountID is the JMAP account id of a user.
func AccountID(user string) string { return "u" + hex.EncodeToString([]byte(user)) }

// ownerOf decodes an account id.
func ownerOf(id string) (string, bool) {
	h, ok := strings.CutPrefix(id, "u")
	if !ok {
		return "", false
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

// account is what a user can do in one account.
type account struct {
	owner    string
	viewer   string // the user looking at it
	personal bool
	readOnly bool // nothing in it is writable for the user
}

// accounts returns the accounts user can see, by id: their own and
// those of users who shared at least one collection with them.
func (h *Handler) accounts(user string) map[string]account {
	cfg := h.cfg.Get()
	out := map[string]account{AccountID(user): {owner: user, viewer: user, personal: true}}
	groups := cfg.GroupsOf(user)
	for _, c := range h.store.All() {
		if c.Owner == user {
			continue
		}
		if _, ok := cfg.Users[c.Owner]; !ok {
			continue
		}
		acc := c.AccessOf(user, groups)
		if acc == "" {
			continue
		}
		id := AccountID(c.Owner)
		a, seen := out[id]
		if !seen {
			a = account{owner: c.Owner, viewer: user, readOnly: true}
		}
		if acc == store.AccessWrite {
			a.readOnly = false
		}
		out[id] = a
	}
	return out
}

// account returns the account with the given id if user can see it.
func (h *Handler) account(user, id string) (account, bool) {
	owner, ok := ownerOf(id)
	if !ok {
		return account{}, false
	}
	a, ok := h.accounts(user)[AccountID(owner)]
	return a, ok
}

// baseURL is the server's public origin.
func (h *Handler) baseURL(r *http.Request) string {
	if b := h.cfg.Get().BaseURL; b != "" {
		return b
	}
	scheme, host := clientip.Origin(r, h.cfg.Get().Trusted)
	return scheme + "://" + host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b)
}
