package jmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/chriss-de/go-ccdav/internal/auth"
	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/store"
)

var testHash = sync.OnceValue(func() string {
	h, err := auth.Hash("pw")
	if err != nil {
		panic(err)
	}
	return h
})

func uidOf(_ store.Kind, b []byte) (uid, comp string) {
	for l := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "UID:"); ok {
			return strings.TrimSpace(v), ""
		}
	}
	return "", ""
}

type env struct {
	t     testing.TB
	srv   *httptest.Server
	h     *Handler
	st    *store.Store
	cfg   *config.Store
	dir   string
	alice string // account ids
	bob   string
}

const capTest = "urn:example:test"

func newEnv(t testing.TB) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{JMAP: true, Users: map[string]config.User{
		"alice": {Password: testHash()},
		"bob":   {Password: testHash()},
		"carol": {Password: testHash()},
	}, Groups: map[string][]string{"fam": {"alice"}}}
	b, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := config.NewStore(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(dir, uidOf)
	st.TrackAccounts()
	if err := st.Reconcile(); err != nil {
		t.Fatal(err)
	}
	defaults := map[store.Kind]map[string]store.Meta{
		store.Calendars:    {"personal": {DisplayName: "Personal"}},
		store.AddressBooks: {"contacts": {DisplayName: "Contacts"}},
	}
	h := New(cs, st, dir, auth.NewAppPasswords(dir), auth.NewLimiter(), defaults, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.AddCapability(capTest, struct{}{}, func(account) any { return struct{}{} })
	h.Register("Test/fixed", capTest, func(*Call, json.RawMessage) (any, error) {
		return json.RawMessage(`{"list":[{"id":"1","tags":["a","b"]},{"id":"2","tags":["c"]}],"ids":["x","y"],"n":3}`), nil
	})
	h.Register("Test/panic", capTest, func(*Call, json.RawMessage) (any, error) { panic("boom") })
	h.Register("Test/fail", capTest, func(*Call, json.RawMessage) (any, error) { return nil, errors.New("disk on fire") })
	h.Register("Test/create", capTest, func(c *Call, args json.RawMessage) (any, error) {
		var a struct{ CID string }
		json.Unmarshal(args, &a)
		c.Created(a.CID, "real-"+a.CID)
		id, ok := c.ResolveID("#" + a.CID)
		return map[string]any{"id": id, "ok": ok}, nil
	})
	h.Register("Test/state", capTest, func(c *Call, args json.RawMessage) (any, error) {
		var a struct{ AccountID string }
		json.Unmarshal(args, &a)
		v, err := c.View(a.AccountID, store.Calendars)
		if err != nil {
			return nil, err
		}
		return map[string]string{"objects": v.State(store.LevelObjects), "collections": v.State(store.LevelCollections)}, nil
	})
	h.Register("Test/changes", capTest, func(c *Call, args json.RawMessage) (any, error) {
		return c.Changes(args, store.Calendars, store.LevelObjects)
	})
	h.Register("TestCollection/changes", capTest, func(c *Call, args json.RawMessage) (any, error) {
		return c.Changes(args, store.Calendars, store.LevelCollections)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(h.Close) // runs first: ends push streams, so srv.Close doesn't wait
	return &env{t: t, srv: srv, h: h, st: st, cfg: cs, dir: dir, alice: AccountID("alice"), bob: AccountID("bob")}
}

func (e *env) do(user, method, path, ctype string, body []byte) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, "pw")
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultTransport.RoundTrip(req) // no redirects followed
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t testing.TB, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// api sends method calls with using [core, test] and returns the
// responses as [name, args, id] with decoded args.
func (e *env) api(user string, calls ...any) []apiResp {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"using": []string{CapCore, capTest, CapContacts, CapPrincipals}, "methodCalls": calls})
	resp := e.do(user, "POST", "/jmap/api", "application/json", body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("api: %d %s", resp.StatusCode, b)
	}
	r := decode[struct {
		MethodResponses []invocation `json:"methodResponses"`
		SessionState    string       `json:"sessionState"`
	}](e.t, resp)
	if r.SessionState == "" {
		e.t.Fatal("no sessionState")
	}
	var out []apiResp
	for _, iv := range r.MethodResponses {
		var args map[string]any
		json.Unmarshal(iv.Args, &args)
		out = append(out, apiResp{iv.Name, args, iv.ID})
	}
	return out
}

type apiResp struct {
	Name string
	Args map[string]any
	ID   string
}

func call(name string, args any, id string) []any { return []any{name, args, id} }

func errType(r apiResp) string {
	if r.Name != "error" {
		return ""
	}
	return r.Args["type"].(string)
}

func TestSessionAndAccounts(t *testing.T) {
	e := newEnv(t)
	if resp := e.do("", "GET", "/.well-known/jmap", "", nil); resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/jmap/session" {
		t.Fatal("well-known", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := e.do("", "GET", "/jmap/session", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("session without auth", resp.StatusCode)
	}
	sess := func(user string) session {
		resp := e.do(user, "GET", "/jmap/session", "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatal(resp.StatusCode)
		}
		return decode[session](t, resp)
	}
	s := sess("alice")
	if s.Username != "alice" || len(s.Accounts) != 1 || !s.Accounts[e.alice].IsPersonal || s.PrimaryAccounts[capTest] != e.alice {
		t.Fatalf("session %+v", s)
	}
	if s.APIURL != e.srv.URL+"/jmap/api" || !strings.Contains(s.DownloadURL, "{blobId}") || !strings.HasSuffix(s.UploadURL, "/upload/{accountId}/") {
		t.Fatalf("urls %+v", s)
	}
	if _, ok := s.Capabilities[CapCore]; !ok {
		t.Fatal("no core capability")
	}

	// bob shares a calendar with alice's group, read-only.
	cal, err := e.st.CreateCollection("bob", store.Calendars, "cal", store.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	cal.SetShares([]store.Share{{Group: "fam", Access: store.AccessRead}})
	s2 := sess("alice")
	b, ok := s2.Accounts[e.bob]
	if !ok || b.IsPersonal || !b.IsReadOnly || b.Name != "bob" {
		t.Fatalf("shared account %+v", s2.Accounts)
	}
	if s2.State == s.State {
		t.Fatal("session state did not change")
	}
	cal.SetShares([]store.Share{{User: "alice", Access: store.AccessWrite}})
	if sess("alice").Accounts[e.bob].IsReadOnly {
		t.Fatal("write share still read-only")
	}
	if _, ok := sess("carol").Accounts[e.bob]; ok {
		t.Fatal("carol sees bob's account")
	}
}

func TestRequestErrors(t *testing.T) {
	e := newEnv(t)
	prob := func(ctype, body, want string) {
		t.Helper()
		resp := e.do("alice", "POST", "/jmap/api", ctype, []byte(body))
		p := decode[problem](t, resp)
		if resp.StatusCode != http.StatusBadRequest || p.Type != want || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("%s: %d %+v", body, resp.StatusCode, p)
		}
	}
	prob("text/plain", `{}`, probNotJSON)
	prob("application/json", `{"using":`, probNotJSON)
	prob("application/json", `[1,2]`, probNotRequest)
	prob("application/json", `{"methodCalls":[]}`, probNotRequest)
	prob("application/json", `{"using":[],"methodCalls":[["a",[],"c"]]}`, probNotRequest)
	prob("application/json", `{"using":["urn:nope"],"methodCalls":[]}`, probUnknownCapability)
	many := `{"using":[],"methodCalls":[` + strings.Repeat(`["Core/echo",{},"c"],`, maxCallsInRequest) + `["Core/echo",{},"c"]]}`
	prob("application/json", many, probLimit)
	if resp := e.do("alice", "GET", "/jmap/api", "", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal("GET api", resp.StatusCode)
	}

	r := e.api("alice",
		call("Core/echo", map[string]any{"hello": true}, "0"),
		call("Nope/get", map[string]any{}, "1"),
		call("Test/panic", map[string]any{}, "2"),
		call("Test/fail", map[string]any{}, "3"),
	)
	if r[0].Name != "Core/echo" || r[0].Args["hello"] != true || r[0].ID != "0" {
		t.Fatalf("echo %+v", r[0])
	}
	if errType(r[1]) != ErrUnknownMethod || errType(r[2]) != ErrServerFail || errType(r[3]) != ErrServerFail {
		t.Fatalf("errors %+v", r[1:])
	}

	// A method whose capability is not in "using" is unknown.
	body := `{"using":["` + CapCore + `"],"methodCalls":[["Test/fixed",{},"0"]]}`
	resp := e.do("alice", "POST", "/jmap/api", "application/json", []byte(body))
	var out response
	json.NewDecoder(resp.Body).Decode(&out)
	if out.MethodResponses[0].Name != "error" || !strings.Contains(string(out.MethodResponses[0].Args), ErrUnknownMethod) {
		t.Fatalf("%s", out.MethodResponses[0].Args)
	}
}

func TestResultReferences(t *testing.T) {
	e := newEnv(t)
	ref := func(of, name, path string) map[string]any {
		return map[string]any{"resultOf": of, "name": name, "path": path}
	}
	r := e.api("alice",
		call("Test/fixed", map[string]any{}, "f"),
		call("Core/echo", map[string]any{"#ids": ref("f", "Test/fixed", "/ids")}, "a"),
		call("Core/echo", map[string]any{"#tags": ref("f", "Test/fixed", "/list/*/tags")}, "b"),
		call("Core/echo", map[string]any{"#first": ref("f", "Test/fixed", "/list/0/id"), "#n": ref("f", "Test/fixed", "/n")}, "c"),
		call("Core/echo", map[string]any{"#x": ref("nope", "Test/fixed", "/ids")}, "d"),
		call("Core/echo", map[string]any{"#x": ref("f", "Other/get", "/ids")}, "e"),
		call("Core/echo", map[string]any{"#x": ref("f", "Test/fixed", "/list/5")}, "g"),
		call("Core/echo", map[string]any{"x": 1, "#x": ref("f", "Test/fixed", "/ids")}, "h"),
		call("Core/echo", map[string]any{"#x": "not a reference"}, "i"),
		call("Core/echo", map[string]any{"#x": ref("f", "Test/fixed", "/n/*")}, "j"),
	)
	asJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if got := asJSON(r[1].Args["ids"]); got != `["x","y"]` {
		t.Errorf("ids %s", got)
	}
	if got := asJSON(r[2].Args["tags"]); got != `["a","b","c"]` {
		t.Errorf("flattened tags %s", got)
	}
	if r[3].Args["first"] != "1" || r[3].Args["n"] != float64(3) {
		t.Errorf("scalars %+v", r[3].Args)
	}
	for _, i := range []int{4, 5, 6, 8, 9} {
		if errType(r[i]) != ErrInvalidResultReference {
			t.Errorf("call %s: %+v", r[i].ID, r[i])
		}
	}
	if errType(r[7]) != ErrInvalidArguments {
		t.Errorf("both x and #x: %+v", r[7])
	}
}

func TestCreatedIDs(t *testing.T) {
	e := newEnv(t)
	body := `{"using":["` + CapCore + `","` + capTest + `"],"createdIds":{"old":"o1"},"methodCalls":[["Test/create",{"cid":"k1"},"0"]]}`
	resp := e.do("alice", "POST", "/jmap/api", "application/json", []byte(body))
	var out struct {
		MethodResponses []invocation      `json:"methodResponses"`
		CreatedIDs      map[string]string `json:"createdIds"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.CreatedIDs["old"] != "o1" || out.CreatedIDs["k1"] != "real-k1" {
		t.Fatalf("createdIds %+v", out.CreatedIDs)
	}
	if !strings.Contains(string(out.MethodResponses[0].Args), `"id":"real-k1"`) {
		t.Fatalf("%s", out.MethodResponses[0].Args)
	}
	// Without createdIds in the request there are none in the response.
	r := e.do("alice", "POST", "/jmap/api", "application/json",
		[]byte(`{"using":["`+CapCore+`"],"methodCalls":[["Core/echo",{},"0"]]}`))
	b, _ := io.ReadAll(r.Body)
	if strings.Contains(string(b), "createdIds") {
		t.Fatalf("%s", b)
	}
}

func (e *env) state(user, acct string) map[string]any {
	e.t.Helper()
	r := e.api(user, call("Test/state", map[string]any{"accountId": acct}, "s"))
	if r[0].Name == "error" {
		e.t.Fatalf("state: %+v", r[0].Args)
	}
	return r[0].Args
}

func (e *env) changes(user, method, acct, since string, maxChanges int) apiResp {
	e.t.Helper()
	args := map[string]any{"accountId": acct, "sinceState": since}
	if maxChanges > 0 {
		args["maxChanges"] = maxChanges
	}
	return e.api(user, call(method, args, "c"))[0]
}

func ids(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	slices.Sort(out)
	return out
}

func TestChanges(t *testing.T) {
	e := newEnv(t)
	cal, _ := e.st.CreateCollection("alice", store.Calendars, "cal", store.Meta{})
	log, _ := e.st.Account("alice", store.Calendars)
	put := func(c *store.Collection, name string) string {
		t.Helper()
		if _, _, err := c.Put(name, []byte("UID:"+name+"\nX"+name), store.Cond{}); err != nil {
			t.Fatal(err)
		}
		id, _ := log.ObjectID(c, name)
		return id
	}
	ia := put(cal, "a")
	s0 := e.state("alice", e.alice)["objects"].(string)
	ib := put(cal, "b")
	cal.Put("a", []byte("UID:a\nchanged"), store.Cond{})

	r := e.changes("alice", "Test/changes", e.alice, s0, 0)
	if r.Name != "Test/changes" || !slices.Equal(ids(r.Args["created"]), []string{ib}) ||
		!slices.Equal(ids(r.Args["updated"]), []string{ia}) || len(ids(r.Args["destroyed"])) != 0 ||
		r.Args["hasMoreChanges"] != false || r.Args["oldState"] != s0 {
		t.Fatalf("changes %+v", r.Args)
	}
	if r.Args["newState"] != e.state("alice", e.alice)["objects"] {
		t.Fatal("newState is not the current state")
	}

	// maxChanges: two pages.
	r = e.changes("alice", "Test/changes", e.alice, s0, 1)
	if r.Args["hasMoreChanges"] != true || len(ids(r.Args["created"]))+len(ids(r.Args["updated"])) != 1 {
		t.Fatalf("page 1 %+v", r.Args)
	}
	r = e.changes("alice", "Test/changes", e.alice, r.Args["newState"].(string), 1)
	if r.Args["hasMoreChanges"] != false {
		t.Fatalf("page 2 %+v", r.Args)
	}

	for _, bad := range []string{"", "x", "ffff-1-o", s0 + "x"} {
		if r := e.changes("alice", "Test/changes", e.alice, bad, 0); errType(r) != ErrCannotCalculateChanges {
			t.Errorf("state %q: %+v", bad, r)
		}
	}
	r = e.api("alice", call("Test/changes", map[string]any{"accountId": e.alice, "sinceState": s0, "maxChanges": 0}, "c"))[0]
	if errType(r) != ErrInvalidArguments {
		t.Errorf("maxChanges 0: %+v", r)
	}
	if r := e.changes("carol", "Test/changes", e.alice, s0, 0); errType(r) != ErrAccountNotFound {
		t.Errorf("foreign account: %+v", r)
	}
	if r := e.changes("alice", "Test/changes", "nonsense", s0, 0); errType(r) != ErrAccountNotFound {
		t.Errorf("bad account id: %+v", r)
	}
}

func TestChangesSharedAccount(t *testing.T) {
	e := newEnv(t)
	shared, _ := e.st.CreateCollection("bob", store.Calendars, "shared", store.Meta{})
	private, _ := e.st.CreateCollection("bob", store.Calendars, "private", store.Meta{})
	log, _ := e.st.Account("bob", store.Calendars)
	put := func(c *store.Collection, name string) string {
		t.Helper()
		if _, _, err := c.Put(name, []byte("UID:"+c.Name+name+"\n"), store.Cond{}); err != nil {
			t.Fatal(err)
		}
		id, _ := log.ObjectID(c, name)
		return id
	}
	shared.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})
	s0 := e.state("alice", e.bob)
	is := put(shared, "s")
	put(private, "p")

	r := e.changes("alice", "Test/changes", e.bob, s0["objects"].(string), 0)
	if !slices.Equal(ids(r.Args["created"]), []string{is}) {
		t.Fatalf("alice sees %+v", r.Args)
	}
	rc := e.changes("alice", "TestCollection/changes", e.bob, s0["collections"].(string), 0)
	if errType(rc) != "" || len(ids(rc.Args["created"]))+len(ids(rc.Args["updated"])) != 0 {
		t.Fatalf("private collection leaked: %+v", rc.Args)
	}
	// bob sees both, and his state strings differ from alice's.
	if bs := e.state("bob", e.bob)["objects"]; bs == e.state("alice", e.bob)["objects"] {
		t.Fatal("owner and sharee share a state string")
	}

	// A move out of the shared collection needs a resync.
	s1 := e.state("alice", e.bob)["objects"].(string)
	if _, err := shared.Move("s", private, "s", store.Cond{}); err != nil {
		t.Fatal(err)
	}
	if r := e.changes("alice", "Test/changes", e.bob, s1, 0); errType(r) != ErrCannotCalculateChanges {
		t.Fatalf("move: %+v", r)
	}
	// So does a change of what is shared.
	s2 := e.state("alice", e.bob)["objects"].(string)
	private.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})
	if r := e.changes("alice", "Test/changes", e.bob, s2, 0); errType(r) != ErrCannotCalculateChanges {
		t.Fatalf("share change: %+v", r)
	}
}

func TestBlobs(t *testing.T) {
	e := newEnv(t)
	resp := e.do("alice", "POST", "/jmap/upload/"+e.alice+"/", "image/png", []byte("PNGDATA"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatal(resp.StatusCode)
	}
	up := decode[uploadResponse](t, resp)
	if up.AccountID != e.alice || up.Size != 7 || up.Type != "image/png" || !strings.HasPrefix(up.BlobID, "G") {
		t.Fatalf("%+v", up)
	}
	resp = e.do("alice", "GET", "/jmap/download/"+e.alice+"/"+up.BlobID+"/pic.png?type=image/png", "", nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "PNGDATA" || resp.Header.Get("Content-Type") != "image/png" ||
		resp.Header.Get("Content-Security-Policy") != "sandbox" || !strings.Contains(resp.Header.Get("Content-Disposition"), "pic.png") {
		t.Fatal(resp.StatusCode, string(b), resp.Header)
	}
	if resp := e.do("bob", "GET", "/jmap/download/"+e.alice+"/"+up.BlobID+"/pic.png", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("bob downloaded alice's blob", resp.StatusCode)
	}
	if resp := e.do("alice", "GET", "/jmap/download/"+e.alice+"/G00/x", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("bad blob id", resp.StatusCode)
	}
	if resp := e.do("alice", "GET", "/jmap/download/"+e.alice+"/../../config.yaml/x", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("path traversal", resp.StatusCode)
	}
	// Uploading into a read-only shared account is refused.
	cal, _ := e.st.CreateCollection("bob", store.Calendars, "cal", store.Meta{})
	cal.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})
	if resp := e.do("alice", "POST", "/jmap/upload/"+e.bob+"/", "text/plain", []byte("x")); resp.StatusCode != http.StatusForbidden {
		t.Fatal("upload to read-only account", resp.StatusCode)
	}
	// An expired blob is gone.
	p := filepath.Join(e.dir, "users", "alice", ".jmap-blobs", up.BlobID[1:])
	old := time.Now().Add(-blobTTL - time.Minute)
	os.Chtimes(p, old, old)
	if resp := e.do("alice", "GET", "/jmap/download/"+e.alice+"/"+up.BlobID+"/pic.png", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("expired blob served", resp.StatusCode)
	}
}

func TestPage(t *testing.T) {
	all := []string{"a", "b", "c", "d", "e"}
	str := func(n int) *string { s := all[n]; return &s }
	num := func(n int) *int { return &n }
	for _, tc := range []struct {
		args QueryArgs
		pos  int
		want string
	}{
		{QueryArgs{}, 0, "abc"}, // capped at maxLimit 3
		{QueryArgs{Position: 3}, 3, "de"},
		{QueryArgs{Position: -2}, 3, "de"},
		{QueryArgs{Position: -9}, 0, "abc"},
		{QueryArgs{Position: 9}, 9, ""},
		{QueryArgs{Limit: num(2)}, 0, "ab"},
		{QueryArgs{Anchor: str(2), AnchorOffset: -1, Limit: num(2)}, 1, "bc"},
		{QueryArgs{Anchor: str(1), AnchorOffset: -5}, 0, "abc"},
	} {
		r, err := Page(all, tc.args, 3)
		if err != nil || r.Position != tc.pos || strings.Join(r.IDs, "") != tc.want {
			t.Errorf("%+v: %+v %v", tc.args, r, err)
		}
	}
	missing := "zz"
	if _, err := Page(all, QueryArgs{Anchor: &missing}, 10); err == nil || err.(*MethodError).Type != ErrAnchorNotFound {
		t.Error("missing anchor", err)
	}
	if r, _ := Page(all, QueryArgs{Limit: num(50), CalculateTotal: true}, 10); r.Limit == nil || *r.Limit != 10 || *r.Total != 5 {
		t.Errorf("capped limit %+v", r)
	}
}

func TestApplyPatch(t *testing.T) {
	obj := map[string]any{"name": "x", "keywords": map[string]any{"a": true, "b": true}, "gone": 1}
	err := ApplyPatch(obj, map[string]any{"name": "y", "keywords/c": true, "keywords/a": nil, "gone": nil, "a~1b": 2})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(obj)
	if string(b) != `{"a/b":2,"keywords":{"b":true,"c":true},"name":"y"}` {
		t.Fatalf("%s", b)
	}
	if err := ApplyPatch(obj, map[string]any{"missing/x": 1}); err == nil {
		t.Fatal("patched through a missing parent")
	}
}

func TestSelect(t *testing.T) {
	obj := map[string]any{"id": "1", "name": "x", "color": "red"}
	got := Select(obj, &[]string{"name", "nope"})
	if len(got) != 2 || got["id"] != "1" || got["name"] != "x" {
		t.Fatal(got)
	}
	if len(Select(obj, nil)) != 3 {
		t.Fatal("nil properties must return everything")
	}
}
