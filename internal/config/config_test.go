package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExampleConfig(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Users["alice"].Email != "alice@example.com" || c.Groups["family"][0] != "alice" {
		t.Fatalf("%+v", c)
	}
}

func TestLoadJSONStillWorks(t *testing.T) {
	p := write(t, t.TempDir(), "config.json", `{"users": {"bob": {"password": "x"}}, "groups": {"g": ["bob"]}}`)
	c, err := Load(p)
	if err != nil || c.Users["bob"].Password != "x" {
		t.Fatal(c, err)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ body, want string }{
		{"users:\n  alice:\n    pasword: x\n", "pasword"},
		{"users:\n  Alice:\n    password: x\n", "invalid user name"},
		{"users:\n  a:\n    password: x\ngroups:\n  g: [nobody]\n", "unknown user"},
		{"users: [\n", "yaml"},
	} {
		_, err := Load(write(t, dir, "config.yaml", tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want error containing %q", tc.body, err, tc.want)
		}
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	if _, err := Find(dir); err == nil {
		t.Fatal("expected error")
	}
	write(t, dir, "config.json", "{}")
	write(t, dir, "config.yaml", "")
	if p, err := Find(dir); err != nil || filepath.Base(p) != "config.yaml" {
		t.Fatal(p, err)
	}
}

func TestPrefixes(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(write(t, dir, "config.yaml", "users: {}\n"))
	if err != nil || c.DAVPrefix != "/dav/" || c.UIPrefix != "/ui/" {
		t.Fatal(c, err)
	}
	c, err = Load(write(t, dir, "config.yaml", "dav_prefix: caldav\nui_prefix: /admin/x\n"))
	if err != nil || c.DAVPrefix != "/caldav/" || c.UIPrefix != "/admin/x/" {
		t.Fatal(c, err)
	}
	for _, bad := range []string{
		"dav_prefix: /\n",
		"dav_prefix: /a b/\n",
		"dav_prefix: /x/\nui_prefix: /x/\n",
		"dav_prefix: /dav/\nui_prefix: /dav/ui/\n",
		"jmap: true\njmap_prefix: /ui/\n",
		"jmap: true\njmap_prefix: /dav/jmap/\n",
		"users:\n  groups:\n    password: x\n",
		"users: {}\ngroups:\n  Bad: []\n",
	} {
		if _, err := Load(write(t, dir, "config.yaml", bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	p := write(t, dir, "config.yaml", "users: {}\n")
	st, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "config.yaml", "dav_prefix: /other/\n")
	if err := st.Reload(); err == nil || st.Get().DAVPrefix != "/dav/" {
		t.Fatal("prefix change on reload must be rejected", err)
	}
	write(t, dir, "config.yaml", "jmap: true\n")
	if err := st.Reload(); err == nil || st.Get().JMAP {
		t.Fatal("jmap change on reload must be rejected", err)
	}
	c, err = Load(write(t, dir, "config.yaml", "jmap: true\n"))
	if err != nil || c.JMAPPrefix != "/jmap/" {
		t.Fatal(c, err)
	}
}

func TestDataDir(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ body, want string }{
		{"", dir},
		{"data_dir: store\n", filepath.Join(dir, "store")},
		{"data_dir: ../store\n", filepath.Join(filepath.Dir(dir), "store")},
		{"data_dir: /var/lib/ccdav\n", "/var/lib/ccdav"},
	} {
		c, err := Load(write(t, dir, "config.yaml", tc.body))
		if err != nil {
			t.Fatal(err)
		}
		if c.DataDir != tc.want {
			t.Errorf("%q: DataDir = %q, want %q", tc.body, c.DataDir, tc.want)
		}
	}
	st, err := NewStore(write(t, dir, "config.yaml", ""))
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "config.yaml", "data_dir: elsewhere\n")
	if err := st.Reload(); err == nil || st.Get().DataDir != dir {
		t.Fatal("data_dir change on reload must be rejected", err)
	}
}

func TestGroupsOf(t *testing.T) {
	c := &Config{Groups: map[string][]string{"b": {"alice"}, "a": {"alice", "bob"}, "c": {"bob"}}}
	if got := c.GroupsOf("alice"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
}

func TestOIDCConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(write(t, dir, "config.yaml", "base_url: https://x.example/\noidc:\n  issuer: https://idp.example\n  client_id: ccdav\nusers:\n  alice:\n    email: Alice@Example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.OIDC.UsernameClaim != "preferred_username" || len(c.OIDC.Scopes) != 3 || c.BaseURL != "https://x.example" {
		t.Fatalf("%+v %q", c.OIDC, c.BaseURL)
	}
	if u, ok := c.UserByEmail("alice@example.com"); !ok || u != "alice" {
		t.Fatal(u, ok)
	}
	if _, ok := c.UserByEmail("mallory"); ok {
		t.Fatal("unknown login accepted")
	}
	for _, bad := range []string{
		"oidc:\n  issuer: https://idp\n  client_id: x\n", // no base_url
		"base_url: https://x\noidc:\n  client_id: x\n",   // no issuer
	} {
		if _, err := Load(write(t, dir, "config.yaml", bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestQuotaParsing(t *testing.T) {
	dir := t.TempDir()
	for in, want := range map[string]int64{"": 0, "1048576": 1 << 20, "500MB": 500 << 20, "2GB": 2 << 30, "1.5GB": 3 << 29, "64 kb": 64 << 10} {
		c, err := Load(write(t, dir, "config.yaml", "quota: \""+in+"\"\nusers: {}\n"))
		if err != nil || c.QuotaBytes != want {
			t.Errorf("%q: %d, %v; want %d", in, c.QuotaBytes, err, want)
		}
	}
	for _, bad := range []string{"lots", "-1GB", "5XB", "GB", "NaN", "inf"} {
		if _, err := Load(write(t, dir, "config.yaml", "quota: \""+bad+"\"\nusers: {}\n")); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
