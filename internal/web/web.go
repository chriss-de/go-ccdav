// Package web is the browser UI under the configurable UI prefix: login
// (OIDC, or the config.yaml password when no OIDC is configured), app
// passwords for DAV clients, and management of calendars, address books
// and shares.
package web

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/auth"
	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/clientip"
	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*.js
var staticFS embed.FS

type UI struct {
	prefix    string // UI prefix, fixed for the server's lifetime
	davPrefix string
	cfg       *config.Store
	st        *store.Store
	appPass   *auth.AppPasswords
	sessions  *auth.Sessions
	limit     *auth.Limiter
	pwCache   *auth.Cache
	key       []byte
	oidc      *oidcClient
	tmpl      *template.Template
	defaults  map[store.Kind]map[string]store.Meta
	log       *slog.Logger
}

// New creates the UI. ap and lim must be the instances the DAV server
// uses; defaults are the collections a new user gets (as on a first DAV
// request).
func New(cfg *config.Store, st *store.Store, ap *auth.AppPasswords, sess *auth.Sessions, lim *auth.Limiter, key []byte,
	defaults map[store.Kind]map[string]store.Meta, log *slog.Logger) *UI {
	c := cfg.Get()
	return &UI{
		prefix: c.UIPrefix, davPrefix: c.DAVPrefix,
		cfg: cfg, st: st, appPass: ap, sessions: sess, limit: lim, pwCache: auth.NewCache(15 * time.Minute), key: key,
		oidc: &oidcClient{http: &http.Client{Timeout: 15 * time.Second}},
		tmpl: template.Must(template.New("").Funcs(template.FuncMap{
			"inputColor": inputColor,
			"has":        slices.Contains[[]string],
			"pair":       func(p *pageData, c colView) map[string]any { return map[string]any{"P": p, "C": c} },
		}).ParseFS(templateFS, "templates/*.html")),
		defaults: defaults,
		log:      log,
	}
}

func securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
}

// Landing serves the static page at "/": what this is, a link to the UI
// and to the project. Register it for exactly "/" ("GET /{$}").
func (u *UI) Landing(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w.Header())
	w.Header().Set("Cache-Control", "public, max-age=300")
	host := r.Host
	if b := u.cfg.Get().BaseURL; b != "" {
		host = strings.TrimPrefix(strings.TrimPrefix(b, "https://"), "http://")
	}
	u.render(w, http.StatusOK, "landing.html", map[string]any{"UIPrefix": u.prefix, "Host": host})
}

func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	securityHeaders(h)
	h.Set("Cache-Control", "no-store")

	path := strings.TrimPrefix(r.URL.Path, u.prefix)
	if r.URL.Path+"/" == u.prefix {
		path = ""
	}
	switch {
	case path == "" && r.Method == http.MethodGet:
		u.index(w, r)
	case strings.HasPrefix(path, "static/") && r.Method == http.MethodGet:
		b, err := staticFS.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=3600")
		w.Write(b)
	case path == "login" && r.Method == http.MethodGet:
		if u.cfg.Get().OIDC == nil {
			http.Redirect(w, r, u.prefix, http.StatusSeeOther)
			return
		}
		u.oidcStart(w, r)
	case path == "login" && r.Method == http.MethodPost:
		u.passwordLogin(w, r)
	case path == "callback" && r.Method == http.MethodGet:
		u.oidcCallback(w, r)
	case path == "export" && r.Method == http.MethodGet:
		u.export(w, r)
	case path == "import" && r.Method == http.MethodPost:
		u.importFile(w, r) // multipart, larger limit: not via post()
	case r.Method == http.MethodPost:
		u.post(w, r, path)
	default:
		http.NotFound(w, r)
	}
}

// --- login ---

func (u *UI) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string, status int) {
	c := u.cfg.Get()
	data := map[string]any{"Prefix": u.prefix, "Error": errMsg, "OIDC": c.OIDC != nil}
	if c.OIDC != nil {
		data["Provider"] = c.OIDC.Name
	} else {
		v := randomString(16)
		if ck, err := r.Cookie(loginCookie); err == nil && ck.Value != "" && len(ck.Value) <= 64 {
			v = ck.Value
		}
		u.setCookie(w, r, loginCookie, v, loginTTL)
		data["Token"] = u.loginToken(v)
	}
	u.render(w, status, "login.html", data)
}

func (u *UI) passwordLogin(w http.ResponseWriter, r *http.Request) {
	c := u.cfg.Get()
	if c.OIDC != nil {
		http.Error(w, "password login is disabled; use single sign-on", http.StatusForbidden)
		return
	}
	// Anyone can post here: a small urlencoded body only (ParseForm leaves
	// multipart alone, which would otherwise be spooled to disk).
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !u.loginFormOK(r) {
		u.renderLogin(w, r, "The login page had expired; please sign in again.", http.StatusForbidden)
		return
	}
	name, pw := strings.TrimSpace(r.PostForm.Get("user")), r.PostForm.Get("password")
	ip := clientip.From(r, c.Trusted)
	if ok, wait := u.limit.Allow(ip, name); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
		u.renderLogin(w, r, "Too many failed attempts. Try again in "+wait.Round(time.Minute).String()+".", http.StatusTooManyRequests)
		return
	}
	usr, ok := c.Users[name]
	paused := ok && u.limit.AccountPaused(name)
	if !ok || usr.Password == "" || paused {
		auth.VerifyNone(pw) // as slow as a wrong password: don't tell who exists
	}
	if paused {
		u.log.Warn("ui password logins paused after too many failures", "user", name, "ip", ip)
	}
	if !ok || usr.Password == "" || paused || !u.pwCache.Verify(name, usr.Password, pw) {
		if ok && !paused {
			u.limit.FailAccount(name)
		}
		u.limit.Fail(ip, name)
		u.log.Warn("ui login failed", "user", auth.ClipName(name), "ip", ip)
		u.renderLogin(w, r, "Wrong user name or password.", http.StatusForbidden)
		return
	}
	u.limit.Success(ip, name)
	if err := u.startSession(w, r, name); err != nil {
		u.log.Error("ui login", "user", name, "err", err)
		u.renderLogin(w, r, "Login failed.", http.StatusInternalServerError)
		return
	}
	u.log.Info("login", "user", name, "via", "password")
	http.Redirect(w, r, u.prefix, http.StatusSeeOther)
}

// --- dashboard ---

type shareView struct {
	Target, Access string // "user:bob" / "group:family"
}

type compCount struct {
	Name  string
	Count int
}

type colView struct {
	Key, Kind, DisplayName, Color, Description, Owner, Access string
	Components                                                []string
	Comps                                                     []compCount // objects per component type, calendars only
	Count                                                     int
	Shares                                                    []shareView
	Hidden, ReadOnly                                          bool
}

type pageData struct {
	Prefix, User, Name, CSRF, DAVURL    string
	Msg, Error                          string
	NewPassword, NewPasswordName        string
	AppPasswords                        []auth.AppPassword
	Calendars, AddressBooks             []colView
	SharedCalendars, SharedAddressBooks []colView
	ShareTargets                        []string
	OIDC                                bool
}

var messages = map[string]string{
	"created": "Collection created.", "saved": "Saved.", "shared": "Share added.",
	"unshared": "Share removed.", "revoked": "App password revoked.",
	"hidden": "Hidden from your devices.", "shown": "Shown on your devices again.",
	"deleted": "Collection deleted.",
}

func (u *UI) index(w http.ResponseWriter, r *http.Request) {
	user, raw := u.session(r)
	if user == "" {
		u.renderLogin(w, r, "", http.StatusOK)
		return
	}
	d := u.page(r, user, raw)
	d.Msg = messages[r.URL.Query().Get("msg")]
	u.render(w, http.StatusOK, "dashboard.html", d)
}

func (u *UI) page(r *http.Request, user, raw string) *pageData {
	c := u.cfg.Get()
	d := &pageData{Prefix: u.prefix, User: user, Name: c.Users[user].Name, CSRF: u.csrfToken(raw),
		DAVURL: u.davURL(r), OIDC: c.OIDC != nil}
	if d.Name == "" {
		d.Name = user
	}
	d.AppPasswords, _ = u.appPass.List(user)
	groups := c.GroupsOf(user)
	for _, k := range []store.Kind{store.Calendars, store.AddressBooks} {
		// Make sure default collections exist before the user's first sync.
		u.st.EnsureHome(user, k, u.defaults[k])
	}
	for _, col := range u.st.All() {
		acc := col.AccessOf(user, groups)
		if acc == "" {
			continue
		}
		v := u.view(col, user, acc)
		switch {
		case acc == store.AccessOwner && col.Kind == store.Calendars:
			d.Calendars = append(d.Calendars, v)
		case acc == store.AccessOwner:
			d.AddressBooks = append(d.AddressBooks, v)
		case col.Kind == store.Calendars:
			d.SharedCalendars = append(d.SharedCalendars, v)
		default:
			d.SharedAddressBooks = append(d.SharedAddressBooks, v)
		}
	}
	for name := range c.Users {
		if name != user {
			d.ShareTargets = append(d.ShareTargets, "user:"+name)
		}
	}
	for g := range c.Groups {
		d.ShareTargets = append(d.ShareTargets, "group:"+g)
	}
	slices.Sort(d.ShareTargets)
	return d
}

func (u *UI) view(col *store.Collection, user, acc string) colView {
	m := col.Meta()
	objs := col.List()
	v := colView{Key: col.Key(), Kind: string(col.Kind), DisplayName: m.DisplayName, Color: m.Color,
		Description: m.Description, Owner: col.Owner, Access: acc, Components: m.Components,
		Count: len(objs), ReadOnly: col.ReadOnly()}
	if col.Kind == store.Calendars {
		if len(v.Components) == 0 {
			v.Components = caldav.CalendarComponents // unset means all
		}
		v.Comps = countComps(v.Components, objs)
	}
	if v.DisplayName == "" {
		v.DisplayName = col.Name
	}
	if acc == store.AccessOwner {
		for _, sh := range m.Shares {
			t := "user:" + sh.User
			if sh.Group != "" {
				t = "group:" + sh.Group
			}
			v.Shares = append(v.Shares, shareView{Target: t, Access: sh.Access})
		}
		return v
	}
	p := u.st.SharePrefs(user, col.Key())
	if p.DisplayName != "" {
		v.DisplayName = p.DisplayName
	}
	if p.Color != "" {
		v.Color = p.Color
	}
	v.Hidden = p.Hidden
	return v
}

// countComps counts objects per component type: the supported components
// first, in their configured order (also when empty), then any others an
// import brought in.
func countComps(supported []string, objs []store.ObjectInfo) []compCount {
	n := make(map[string]int)
	for _, o := range objs {
		n[o.Comp]++
	}
	var out []compCount
	for _, c := range supported {
		out = append(out, compCount{c, n[c]})
		delete(n, c)
	}
	extra := slices.Sorted(maps.Keys(n))
	for _, c := range extra {
		name := c
		if name == "" {
			name = "other"
		}
		out = append(out, compCount{name, n[c]})
	}
	return out
}

func (u *UI) davURL(r *http.Request) string {
	if b := u.cfg.Get().BaseURL; b != "" {
		return b + u.davPrefix
	}
	scheme, host := clientip.Origin(r, u.cfg.Get().Trusted)
	return scheme + "://" + host + u.davPrefix
}

// --- actions ---

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)

// errUser is shown to the user as is.
type errUser string

func (e errUser) Error() string { return string(e) }

func (u *UI) post(w http.ResponseWriter, r *http.Request, path string) {
	user, raw := u.session(r)
	if user == "" {
		http.Redirect(w, r, u.prefix, http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("csrf") != u.csrfToken(raw) {
		http.Error(w, "invalid form (expired page?) - reload and try again", http.StatusForbidden)
		return
	}
	f := r.PostForm
	var (
		msg string
		err error
	)
	switch path {
	case "logout":
		// Ends the user's sessions everywhere: a copy of the cookie must
		// not outlive the logout.
		if err := u.sessions.End(user); err != nil {
			u.renderError(w, r, user, raw, err)
			return
		}
		u.clearCookie(w, r, sessionCookie)
		http.Redirect(w, r, u.prefix, http.StatusSeeOther)
		return
	case "app-passwords":
		pw, ap, err := u.appPass.Create(user, f.Get("name"))
		if err != nil {
			u.renderError(w, r, user, raw, err)
			return
		}
		d := u.page(r, user, raw)
		d.NewPassword, d.NewPasswordName = pw, ap.Name
		u.render(w, http.StatusOK, "dashboard.html", d)
		return
	case "app-passwords/revoke":
		err, msg = u.appPass.Revoke(user, f.Get("id")), "revoked"
	case "collections":
		err, msg = u.createCollection(user, f.Get("kind"), f.Get("displayname"), f.Get("color"), f["components"]), "created"
	case "collections/edit":
		err, msg = u.editCollection(user, f.Get("key"), f.Get("displayname"), f.Get("color"), f.Get("description"), f["components"]), "saved"
	case "collections/delete":
		err, msg = u.deleteCollection(user, f.Get("key"), f.Get("confirm")), "deleted"
	case "collections/hide":
		hide := f.Get("hidden") == "1"
		msg = map[bool]string{true: "hidden", false: "shown"}[hide]
		err = u.withShared(user, f.Get("key"), func(col *store.Collection) error {
			return u.st.UpdateSharePrefs(user, col.Key(), func(p *store.SharePrefs) { p.Hidden = hide })
		})
	case "shares":
		err, msg = u.changeShare(user, f.Get("key"), f.Get("target"), f.Get("access")), "shared"
	case "shares/remove":
		err, msg = u.changeShare(user, f.Get("key"), f.Get("target"), ""), "unshared"
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		u.renderError(w, r, user, raw, err)
		return
	}
	http.Redirect(w, r, u.prefix+"?msg="+msg, http.StatusSeeOther)
}

func (u *UI) renderError(w http.ResponseWriter, r *http.Request, user, raw string, err error) {
	d := u.page(r, user, raw)
	var ue errUser
	if errors.As(err, &ue) {
		d.Error = string(ue)
	} else {
		u.log.Error("ui action", "user", user, "err", err)
		d.Error = "Something went wrong; the server log has the details." // not err: paths and such
	}
	u.render(w, http.StatusBadRequest, "dashboard.html", d)
}

// own returns a collection the user owns, by key.
const errReadOnly = errUser("This collection is generated by the server and can't be changed.")

// own returns the user's own collection for a change to it.
func (u *UI) own(user, key string) (*store.Collection, error) {
	col, err := u.byKey(key)
	if err != nil || col.Owner != user {
		return nil, errUser("No such collection.")
	}
	if col.ReadOnly() {
		return nil, errReadOnly
	}
	return col, nil
}

func (u *UI) byKey(key string) (*store.Collection, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || !config.ValidUserName(parts[0]) ||
		store.Kind(parts[1]) != store.Calendars && store.Kind(parts[1]) != store.AddressBooks {
		return nil, errUser("No such collection.")
	}
	return u.st.Collection(parts[0], store.Kind(parts[1]), parts[2])
}

func (u *UI) withShared(user, key string, fn func(*store.Collection) error) error {
	col, err := u.byKey(key)
	if err != nil {
		return errUser("No such collection.")
	}
	if acc := col.AccessOf(user, u.cfg.Get().GroupsOf(user)); acc == "" || acc == store.AccessOwner {
		return errUser("No such shared collection.")
	}
	return fn(col)
}

func newCollectionName() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (u *UI) createCollection(user, kind, name, color string, comps []string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return errUser("Please give the collection a name (up to 100 characters).")
	}
	if color != "" && !colorRe.MatchString(color) {
		return errUser("Invalid color.")
	}
	m := store.Meta{DisplayName: name, Color: color}
	k := store.Kind(kind)
	switch k {
	case store.Calendars:
		if err := checkComps(comps); err != nil {
			return err
		}
		m.Components = comps
	case store.AddressBooks:
		m.Color = ""
	default:
		return errUser("Unknown collection type.")
	}
	_, err := u.st.CreateCollection(user, k, newCollectionName(), m)
	return err
}

func checkComps(comps []string) error {
	for _, c := range comps {
		if !slices.Contains(caldav.CalendarComponents, c) {
			return errUser("Invalid component type.")
		}
	}
	if len(comps) == 0 {
		return errUser("Choose at least one of events, tasks or journal.")
	}
	return nil
}

var compNouns = map[string][2]string{"VEVENT": {"event", "events"}, "VTODO": {"task", "tasks"}, "VJOURNAL": {"journal entry", "journal entries"}}

func (u *UI) editCollection(user, key, name, color, desc string, comps []string) error {
	name = strings.TrimSpace(name)
	if len(name) > 100 || color != "" && !colorRe.MatchString(color) {
		return errUser("Invalid name or color.")
	}
	col, err := u.own(user, key)
	if err == errReadOnly {
		return err
	}
	if err == nil {
		if col.Kind == store.Calendars {
			if err := checkComps(comps); err != nil {
				return err
			}
			// A type can only go once the calendar holds no such objects.
			for _, cc := range countComps(nil, col.List()) {
				if cc.Name == "other" || slices.Contains(comps, cc.Name) {
					continue
				}
				noun := compNouns[cc.Name][1]
				if cc.Count == 1 {
					noun = compNouns[cc.Name][0]
				}
				m := col.Meta()
				return errUser(fmt.Sprintf("%s still has %d %s. Delete them first or keep that type.", m.DisplayName, cc.Count, noun))
			}
		}
		return col.UpdateMeta(func(m *store.Meta) {
			if col.Kind == store.Calendars {
				m.Components = comps
			}
			if name != "" {
				m.DisplayName = name
			}
			if color != "" && col.Kind == store.Calendars {
				m.Color = keepAlpha(m.Color, color)
			}
			m.Description = strings.TrimSpace(desc)
		})
	}
	// Shared with the user: only their own name and color.
	return u.withShared(user, key, func(col *store.Collection) error {
		return u.st.UpdateSharePrefs(user, col.Key(), func(p *store.SharePrefs) {
			p.DisplayName = name
			if color != "" {
				p.Color = keepAlpha(p.Color, color)
			}
		})
	})
}

// keepAlpha applies a #rrggbb from the color picker but keeps an existing
// alpha suffix (Apple stores #RRGGBBAA), and leaves a set value untouched
// if the color didn't change. An unset color is always set: the picker
// shows a default, and saving it must store it.
func keepAlpha(old, picked string) string {
	if old != "" && strings.EqualFold(inputColor(old), picked) {
		return old
	}
	if len(old) == 9 {
		return picked + old[7:]
	}
	return picked
}

// inputColor converts a stored color to the #rrggbb an <input type=color>
// needs.
func inputColor(c string) string {
	if len(c) >= 7 && colorRe.MatchString(c) {
		return strings.ToLower(c[:7])
	}
	return "#3a87ad"
}

func (u *UI) changeShare(user, key, target, acc string) error {
	col, err := u.own(user, key)
	if err != nil {
		return err
	}
	c := u.cfg.Get()
	typ, name, _ := strings.Cut(target, ":")
	var sh store.Share
	switch typ {
	case "user":
		if _, ok := c.Users[name]; !ok || name == user {
			return errUser("Unknown user.")
		}
		sh.User = name
	case "group":
		if _, ok := c.Groups[name]; !ok {
			return errUser("Unknown group.")
		}
		sh.Group = name
	default:
		return errUser("Choose a user or group.")
	}
	shares := slices.DeleteFunc(col.Shares(), func(s store.Share) bool { return s.User == sh.User && s.Group == sh.Group })
	if acc != "" {
		if acc != store.AccessRead && acc != store.AccessWrite {
			return errUser("Invalid access level.")
		}
		sh.Access = acc
		shares = append(shares, sh)
	}
	return col.SetShares(shares)
}

func (u *UI) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		u.log.Error("template", "name", name, "err", err)
	}
}
