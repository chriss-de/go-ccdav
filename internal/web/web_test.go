package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/chriss-de/go-ccdav/internal/auth"
	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/directory"
	"github.com/chriss-de/go-ccdav/internal/server"
	"github.com/chriss-de/go-ccdav/internal/store"
)

var testHash = sync.OnceValue(func() string {
	h, err := auth.Hash("pw")
	if err != nil {
		panic(err)
	}
	return h
})

type env struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
}

func newEnv(t *testing.T, mut func(*config.Config)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Users: map[string]config.User{
			"alice": {Password: testHash(), Email: "alice@example.com", Name: "Alice"},
			"bob":   {Password: testHash()},
		},
		Groups: map[string][]string{"fam": {"alice", "bob"}},
	}
	if mut != nil {
		mut(&cfg)
	}
	b, _ := yaml.Marshal(cfg)
	os.WriteFile(filepath.Join(dir, "config.yaml"), b, 0o600)
	cs, err := config.NewStore(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(dir, server.UIDOf)
	ap := auth.NewAppPasswords(dir)
	defaults := map[store.Kind]map[string]store.Meta{store.Calendars: server.DefaultCalendars, store.AddressBooks: server.DefaultAddressBooks}
	mux := http.NewServeMux()
	lim := auth.NewLimiter()
	mux.Handle(cs.Get().UIPrefix, New(cs, st, ap, lim, []byte("0123456789abcdef0123456789abcdef"), defaults, log))
	mux.Handle("/", server.New(cs, st, ap, lim, log))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{t, srv, st}
}

// browser is a cookie-keeping client that does not follow redirects.
type browser struct {
	e  *env
	c  *http.Client
	ui string
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, ui: "/ui/", c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(method, path string, form url.Values) (*http.Response, string) {
	b.e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func (b *browser) expect(method, path string, form url.Values, status int) string {
	b.e.t.Helper()
	resp, out := b.do(method, path, form)
	if resp.StatusCode != status {
		b.e.t.Fatalf("%s %s: %d, want %d\n%s", method, path, resp.StatusCode, status, out)
	}
	return out
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (b *browser) csrf() string {
	b.e.t.Helper()
	out := b.expect("GET", b.ui, nil, http.StatusOK)
	m := csrfRe.FindStringSubmatch(out)
	if m == nil {
		b.e.t.Fatalf("no csrf token in\n%s", out)
	}
	return m[1]
}

func (b *browser) post(path string, status int, kv ...string) string {
	b.e.t.Helper()
	f := url.Values{"csrf": {b.csrf()}}
	for i := 0; i+1 < len(kv); i += 2 {
		f.Add(kv[i], kv[i+1])
	}
	return b.expect("POST", b.ui+path, f, status)
}

func (b *browser) login(user string) {
	b.e.t.Helper()
	b.expect("POST", b.ui+"login", url.Values{"user": {user}, "password": {"pw"}}, http.StatusSeeOther)
}

func (e *env) dav(method, path, user, pw string) int {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, nil)
	req.SetBasicAuth(user, pw)
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) davBody(path, user string) string {
	e.t.Helper()
	req, _ := http.NewRequest("PROPFIND", e.srv.URL+path, nil)
	req.SetBasicAuth(user, "pw")
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func contains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in:\n%s", sub, s)
		}
	}
}

func TestPasswordLoginAndCSRF(t *testing.T) {
	e := newEnv(t, nil)
	b := e.browser()
	out := b.expect("GET", "/ui/", nil, http.StatusOK)
	contains(t, out, `name="password"`)
	b.expect("GET", "/ui", nil, http.StatusTemporaryRedirect) // ServeMux adds the slash
	b.expect("POST", "/ui/login", url.Values{"user": {"alice"}, "password": {"wrong"}}, http.StatusForbidden)
	b.login("alice")
	resp, out := b.do("GET", "/ui/", nil)
	contains(t, out, "Connect a device", "Calendars", "Address books", "Personal", "VEVENT", "VTODO", "VJOURNAL", "Contacts")
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	// Session cookie flags.
	sc := resp.Request.Header.Get("Cookie")
	if !strings.Contains(sc, "ccdav_session=") {
		t.Fatal("no session cookie sent")
	}
	// POST without / with a wrong CSRF token.
	b.expect("POST", "/ui/collections", url.Values{"kind": {"calendars"}, "displayname": {"x"}}, http.StatusForbidden)
	b.expect("POST", "/ui/collections", url.Values{"csrf": {"00"}, "kind": {"calendars"}, "displayname": {"x"}}, http.StatusForbidden)
	b.expect("GET", "/ui/nope", nil, http.StatusNotFound)
	js, err := http.Get(e.srv.URL + "/ui/static/colorwheel.js")
	if err != nil || js.StatusCode != http.StatusOK || !strings.HasPrefix(js.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("colorwheel.js: %v %v", err, js)
	}
	js.Body.Close()
	// Logout.
	b.post("logout", http.StatusSeeOther)
	contains(t, b.expect("GET", "/ui/", nil, http.StatusOK), `name="password"`)
	// Anonymous POSTs go to the login page.
	e.browser().expect("POST", "/ui/collections", url.Values{}, http.StatusSeeOther)
}

func TestAppPasswordsUI(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		u := c.Users["bob"]
		u.Password = "" // bob only has app passwords
		c.Users["bob"] = u
	})
	a := e.browser()
	a.login("alice")
	out := a.post("app-passwords", http.StatusOK, "name", "Phone")
	m := regexp.MustCompile(`<code class="secret">([a-z2-7-]+)</code>`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no new password shown:\n%s", out)
	}
	pw := m[1]
	if e.dav("PROPFIND", "/dav/calendars/alice/", "alice", pw) != http.StatusMultiStatus {
		t.Fatal("app password not accepted for DAV")
	}
	if e.dav("PROPFIND", "/dav/calendars/alice/", "alice", "pw") != http.StatusMultiStatus {
		t.Fatal("config password fallback broken")
	}
	if e.dav("PROPFIND", "/dav/calendars/bob/", "bob", pw) != http.StatusUnauthorized {
		t.Fatal("alice's app password accepted for bob")
	}
	out = a.expect("GET", "/ui/", nil, http.StatusOK)
	contains(t, out, "Phone")
	if strings.Contains(out, pw) {
		t.Fatal("app password shown again")
	}
	id := regexp.MustCompile(`name="id" value="([0-9a-f]+)"`).FindStringSubmatch(out)[1]
	a.post("app-passwords/revoke", http.StatusSeeOther, "id", id)
	if e.dav("PROPFIND", "/dav/calendars/alice/", "alice", pw) != http.StatusUnauthorized {
		t.Fatal("revoked app password still accepted")
	}
	a.post("app-passwords", http.StatusBadRequest, "name", "  ")
}

func TestCollectionsAndSharingUI(t *testing.T) {
	e := newEnv(t, nil)
	a := e.browser()
	a.login("alice")
	a.post("collections", http.StatusSeeOther, "kind", "calendars", "displayname", "Work", "color", "#ff8800", "components", "VEVENT", "components", "VTODO")
	a.post("collections", http.StatusBadRequest, "kind", "calendars", "displayname", "No comps")
	a.post("collections", http.StatusBadRequest, "kind", "calendars", "displayname", "x", "color", "red", "components", "VEVENT")
	a.post("collections", http.StatusSeeOther, "kind", "addressbooks", "displayname", "Clients")
	out := a.expect("GET", "/ui/?msg=created", nil, http.StatusOK)
	contains(t, out, "Collection created.", "Work", "Clients", "VTODO", "background:#ff8800", `data-cw`, `value="#ff8800"`, "static/colorwheel.js")

	// Find the new calendar's key and check it over DAV.
	var work *store.Collection
	for _, c := range e.st.All() {
		if c.Meta().DisplayName == "Work" {
			work = c
		}
	}
	if work == nil || work.Owner != "alice" {
		t.Fatal("calendar not created")
	}
	dav := e.davBody("/dav/calendars/alice/", "alice")
	contains(t, dav, "/dav/calendars/alice/"+work.Name+"/", "<d:displayname>Work</d:displayname>")

	// Per-component counts on the chips, and the action panels script.
	for _, o := range []struct{ name, comp string }{{"a", "VEVENT"}, {"b", "VEVENT"}, {"c", "VTODO"}} {
		ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:" + o.comp + "\r\nUID:" + o.name + "\r\nDTSTAMP:20260101T000000Z\r\nEND:" + o.comp + "\r\nEND:VCALENDAR\r\n"
		if _, _, err := work.Put(o.name+".ics", []byte(ics), store.Cond{}); err != nil {
			t.Fatal(err)
		}
	}
	out = a.expect("GET", "/ui/", nil, http.StatusOK)
	contains(t, out, "VEVENT <b>2</b>", "VTODO <b>1</b>", "<b>0</b> contacts", "data-actions", "static/actions.js")
	if js, err := http.Get(e.srv.URL + "/ui/static/actions.js"); err != nil || js.StatusCode != http.StatusOK {
		t.Fatalf("actions.js: %v %v", err, js)
	}
	if js, err := http.Get(e.srv.URL + "/ui/static/nope.js"); err != nil || js.StatusCode != http.StatusNotFound {
		t.Fatalf("nope.js: %v %v", err, js)
	}

	// Component types: add one, can't drop one that's in use, can drop an
	// unused one, need at least one.
	edit := func(code int, comps ...string) string {
		kv := []string{"key", work.Key(), "displayname", "Work"}
		for _, c := range comps {
			kv = append(kv, "components", c)
		}
		return a.post("collections/edit", code, kv...)
	}
	edit(http.StatusSeeOther, "VEVENT", "VTODO", "VJOURNAL")
	if c := work.Meta().Components; !slices.Equal(c, []string{"VEVENT", "VTODO", "VJOURNAL"}) {
		t.Fatal(c)
	}
	contains(t, edit(http.StatusBadRequest, "VEVENT", "VJOURNAL"), "Work still has 1 task.")
	edit(http.StatusSeeOther, "VEVENT", "VTODO")
	edit(http.StatusBadRequest)
	edit(http.StatusBadRequest, "VEVENT", "VALARM")
	if c := work.Meta().Components; !slices.Equal(c, []string{"VEVENT", "VTODO"}) {
		t.Fatal(c)
	}

	// Edit: name + color; token moves so clients notice.
	tok := work.SyncToken()
	a.post("collections/edit", http.StatusSeeOther, "key", work.Key(), "displayname", "Job", "color", "#00ff00", "description", "office", "components", "VEVENT", "components", "VTODO")
	if m := work.Meta(); m.DisplayName != "Job" || m.Color != "#00ff00" || m.Description != "office" || work.SyncToken() == tok {
		t.Fatalf("%+v token %d", m, work.SyncToken())
	}
	// A calendar without a color shows as such; saving the picker's
	// default stores it.
	work.UpdateMeta(func(m *store.Meta) { m.Color = "" })
	contains(t, a.expect("GET", "/ui/", nil, http.StatusOK), `class="dot nocolor"`)
	a.post("collections/edit", http.StatusSeeOther, "key", work.Key(), "displayname", "Job", "color", "#3a87ad", "components", "VEVENT", "components", "VTODO")
	if work.Meta().Color != "#3a87ad" {
		t.Fatalf("default color not stored: %q", work.Meta().Color)
	}
	// Alpha suffix (Apple) survives an unchanged color.
	work.UpdateMeta(func(m *store.Meta) { m.Color = "#00FF00CC" })
	a.post("collections/edit", http.StatusSeeOther, "key", work.Key(), "displayname", "Job", "color", "#00ff00", "components", "VEVENT", "components", "VTODO")
	if work.Meta().Color != "#00FF00CC" {
		t.Fatalf("alpha lost: %s", work.Meta().Color)
	}

	// Share with bob (read), then he sees it over DAV and in his UI.
	a.post("shares", http.StatusSeeOther, "key", work.Key(), "target", "user:bob", "access", "read")
	a.post("shares", http.StatusBadRequest, "key", work.Key(), "target", "user:alice", "access", "read")
	a.post("shares", http.StatusBadRequest, "key", work.Key(), "target", "user:bob", "access", "admin")
	contains(t, a.expect("GET", "/ui/", nil, http.StatusOK), "user:bob · read")
	contains(t, e.davBody("/dav/calendars/bob/", "bob"), "alice~"+work.Name+"/")

	b := e.browser()
	b.login("bob")
	out = b.expect("GET", "/ui/", nil, http.StatusOK)
	contains(t, out, "Shared with you", "Job", "from alice", "read-only")
	// Bob cannot edit alice's collection itself or its shares.
	b.post("shares", http.StatusBadRequest, "key", work.Key(), "target", "group:fam", "access", "write")
	// His own view: name and color go to his prefs only.
	b.post("collections/edit", http.StatusSeeOther, "key", work.Key(), "displayname", "Alice job", "color", "#123456")
	if p := e.st.SharePrefs("bob", work.Key()); p.DisplayName != "Alice job" || p.Color != "#123456" {
		t.Fatalf("%+v", p)
	}
	if work.Meta().DisplayName != "Job" {
		t.Fatal("sharee edit changed the owner's name")
	}
	b.post("collections/hide", http.StatusSeeOther, "key", work.Key(), "hidden", "1")
	if strings.Contains(e.davBody("/dav/calendars/bob/", "bob"), "alice~"+work.Name) {
		t.Fatal("hidden collection still listed")
	}
	b.post("collections/hide", http.StatusSeeOther, "key", work.Key(), "hidden", "0")
	contains(t, e.davBody("/dav/calendars/bob/", "bob"), "alice~"+work.Name)

	a.post("shares/remove", http.StatusSeeOther, "key", work.Key(), "target", "user:bob")
	if strings.Contains(e.davBody("/dav/calendars/bob/", "bob"), "alice~"+work.Name) {
		t.Fatal("unshared collection still listed")
	}
	b.post("collections/edit", http.StatusBadRequest, "key", work.Key(), "displayname", "x")
}

// fakeIdP implements discovery, token and userinfo endpoints.
func fakeIdP(t *testing.T, claim string) *httptest.Server {
	var idp *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 idp.URL,
			"authorization_endpoint": idp.URL + "/authorize",
			"token_endpoint":         idp.URL + "/token",
			"userinfo_endpoint":      idp.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"at-123","token_type":"Bearer","expires_in":300}`)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-123" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"sub": "1", "preferred_username": claim, "email": claim + "@idp"})
	})
	idp = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	return idp
}

func TestOIDCLogin(t *testing.T) {
	for _, tc := range []struct {
		claim string
		ok    bool
	}{{"alice", true}, {"mallory", false}} {
		idp := fakeIdP(t, tc.claim)
		e := newEnv(t, func(c *config.Config) {
			c.BaseURL = "http://ccdav.test"
			c.OIDC = &config.OIDC{Issuer: idp.URL, ClientID: "ccdav", ClientSecret: "s", Name: "TestIdP"}
		})
		b := e.browser()
		out := b.expect("GET", "/ui/", nil, http.StatusOK)
		contains(t, out, "Sign in with TestIdP")
		// Password login is off when OIDC is configured.
		b.expect("POST", "/ui/login", url.Values{"user": {"alice"}, "password": {"pw"}}, http.StatusForbidden)

		resp, _ := b.do("GET", "/ui/login", nil)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("login start: %d", resp.StatusCode)
		}
		loc, _ := url.Parse(resp.Header.Get("Location"))
		q := loc.Query()
		if !strings.HasPrefix(loc.String(), idp.URL+"/authorize") || q.Get("code_challenge_method") != "S256" ||
			q.Get("redirect_uri") != "http://ccdav.test/ui/callback" || q.Get("state") == "" {
			t.Fatalf("bad authorize redirect: %s", loc)
		}
		// Wrong state and wrong code are rejected.
		b.expect("GET", "/ui/callback?code=good-code&state=forged", nil, http.StatusForbidden)
		resp, _ = b.do("GET", "/ui/login", nil)
		state := mustState(t, resp)
		b.expect("GET", "/ui/callback?code=bad-code&state="+state, nil, http.StatusForbidden)

		resp, _ = b.do("GET", "/ui/login", nil)
		state = mustState(t, resp)
		if !tc.ok {
			out := b.expect("GET", "/ui/callback?code=good-code&state="+state, nil, http.StatusForbidden)
			contains(t, out, "not allowed")
			continue
		}
		b.expect("GET", "/ui/callback?code=good-code&state="+state, nil, http.StatusSeeOther)
		contains(t, b.expect("GET", "/ui/", nil, http.StatusOK), "Connect a device", "Alice")
		// The state cookie is single-use.
		b.expect("GET", "/ui/callback?code=good-code&state="+state, nil, http.StatusForbidden)
		// Cancelled at the provider.
		b.expect("GET", "/ui/callback?error=access_denied", nil, http.StatusForbidden)
	}
}

func mustState(t *testing.T, resp *http.Response) string {
	t.Helper()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("state") == "" {
		t.Fatalf("no state in %q", resp.Header.Get("Location"))
	}
	return loc.Query().Get("state")
}

func TestSessionSigning(t *testing.T) {
	u := &UI{key: []byte("k1k1k1k1k1k1k1k1k1k1k1k1k1k1k1k1")}
	v := u.signTimed("session", sessionTTL, "alice")
	if f, ok := u.verifyTimed("session", v); !ok || f[0] != "alice" {
		t.Fatal("roundtrip")
	}
	if _, ok := u.verifyTimed("oidc", v); ok {
		t.Fatal("purpose not bound")
	}
	// Flip one character in the middle of the signature.
	i := len(v) - 10
	flipped := byte('A')
	if v[i] == 'A' {
		flipped = 'B'
	}
	if _, ok := u.verifyTimed("session", v[:i]+string(flipped)+v[i+1:]); ok {
		t.Fatal("tampered value accepted")
	}
	payload, sig, _ := strings.Cut(v, ".")
	forged := b64.EncodeToString([]byte("9999999999|bob")) + "." + sig
	if _, ok := u.verifyTimed("session", forged); ok || payload == "" {
		t.Fatal("forged payload accepted")
	}
	if _, ok := u.verifyTimed("session", u.signTimed("session", -time.Minute, "alice")); ok {
		t.Fatal("expired value accepted")
	}
	other := &UI{key: []byte("k2k2k2k2k2k2k2k2k2k2k2k2k2k2k2k2")}
	if _, ok := other.verifyTimed("session", v); ok {
		t.Fatal("other key accepted")
	}
}

func TestDirectoryUI(t *testing.T) {
	var cfg *config.Config
	e := newEnv(t, func(c *config.Config) { cfg = c })
	if err := directory.Sync(cfg, e.st, server.DefaultAddressBooks); err != nil {
		t.Fatal(err)
	}
	key := "alice/addressbooks/" + directory.Name
	a := e.browser()
	a.login("alice")
	out := a.expect("GET", "/ui/", nil, http.StatusOK)
	contains(t, out, "Directory", "read-only", "<b>3</b> contacts", "export?key=alice%2faddressbooks%2f"+directory.Name)
	if strings.Contains(out, `name="key" value="`+key+`"`) {
		t.Fatal("directory has change forms")
	}
	for _, p := range [][]string{
		{"collections/edit", "key", key, "displayname", "Mine"},
		{"collections/delete", "key", key, "confirm", "yes"},
		{"shares", "key", key, "target", "user:bob", "access", "read"},
	} {
		contains(t, a.post(p[0], http.StatusBadRequest, p[1:]...), "generated by the server")
	}
	contains(t, a.upload(key, "x.vcf", "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:x\r\nFN:x\r\nEND:VCARD\r\n", http.StatusBadRequest), "generated by the server")
	contains(t, a.expect("GET", "/ui/export?key="+url.QueryEscape(key), nil, http.StatusOK), "FN:Alice")
}
