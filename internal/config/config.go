// Package config loads the server configuration (users, groups, SMTP and
// RSVP settings) from config.yaml, by default in the data directory. config.json is
// accepted too: YAML is a superset of JSON. The live config is swapped
// atomically on Reload (SIGHUP).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	"go.yaml.in/yaml/v3"

	"github.com/chriss-de/go-ccdav/internal/clientip"
)

type User struct {
	Password string `yaml:"password"` // auth.Hash format
	Email    string `yaml:"email,omitempty"`
	Name     string `yaml:"name,omitempty"`
}

type SMTP struct {
	Host     string `yaml:"host,omitempty"`
	Port     int    `yaml:"port,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	From     string `yaml:"from,omitempty"`
}

// OIDC configures web UI login. Only users listed in Users may log in:
// the UsernameClaim from the userinfo endpoint must equal a user name or
// a user's email.
type OIDC struct {
	Issuer        string   `yaml:"issuer"` // discovery: <issuer>/.well-known/openid-configuration
	ClientID      string   `yaml:"client_id"`
	ClientSecret  string   `yaml:"client_secret,omitempty"`
	UsernameClaim string   `yaml:"username_claim,omitempty"` // default preferred_username
	Scopes        []string `yaml:"scopes,omitempty"`         // default openid, profile, email
	Name          string   `yaml:"name,omitempty"`           // shown on the login button
}

type Config struct {
	// DAVPrefix and UIPrefix are the URL paths the DAV tree and the web UI
	// live under. "/" itself only has a static landing page. Changing them needs a
	// restart and breaks existing client accounts / bookmarks.
	DAVPrefix string `yaml:"dav_prefix,omitempty"`
	UIPrefix  string `yaml:"ui_prefix,omitempty"`
	// DataDir holds collections, session.key and app passwords. Load makes
	// a relative path relative to the config file and defaults it to the
	// file's directory. Changing it needs a restart.
	DataDir string `yaml:"data_dir,omitempty"`
	// JMAP turns on the JMAP API under JMAPPrefix and /.well-known/jmap.
	// Both need a restart.
	JMAP       bool   `yaml:"jmap,omitempty"`
	JMAPPrefix string `yaml:"jmap_prefix,omitempty"`
	// CalDAVPerUser gives everyone their own alarms, color, categories and
	// free/busy in calendars shared with them, via CalDAV too (the same
	// per-user data JMAP keeps). Needs a restart.
	CalDAVPerUser bool `yaml:"caldav_per_user,omitempty"`
	// BaseURL is the public URL behind the reverse proxy, used for links in
	// emails (RSVP). DAV hrefs are host-relative and don't need it.
	BaseURL string              `yaml:"base_url,omitempty"`
	Users   map[string]User     `yaml:"users"`
	Groups  map[string][]string `yaml:"groups,omitempty"`
	OIDC    *OIDC               `yaml:"oidc,omitempty"`
	// TrustedProxies (CIDRs or addresses) may set X-Forwarded-For; the
	// client IP is used for logging and login rate limiting.
	TrustedProxies []string       `yaml:"trusted_proxies,omitempty"`
	Trusted        []netip.Prefix `yaml:"-"`
	SMTP           SMTP           `yaml:"smtp,omitempty"`
	RSVPSecret     string         `yaml:"rsvp_secret,omitempty"`
	// Directory adds a generated, read-only address book with all users
	// to every user's home (default on).
	Directory *bool `yaml:"directory,omitempty"`
}

var userRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidUserName reports whether name is usable as a user or group name
// (and path segment). "groups" is reserved for /principals/groups/.
func ValidUserName(name string) bool { return userRe.MatchString(name) && name != "groups" }

const (
	DefaultDAVPrefix  = "/dav/"
	DefaultUIPrefix   = "/ui/"
	DefaultJMAPPrefix = "/jmap/"
)

var prefixRe = regexp.MustCompile(`^(/[a-zA-Z0-9._-]+)+/$`)

// normPrefix turns "dav", "/dav" or "/dav/" into "/dav/".
func normPrefix(p, def string) (string, error) {
	if p == "" {
		return def, nil
	}
	p = "/" + strings.Trim(p, "/") + "/"
	if !prefixRe.MatchString(p) {
		return "", fmt.Errorf("invalid path prefix %q", p)
	}
	return p, nil
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true) // catch typos like "pasword"
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.DAVPrefix, err = normPrefix(c.DAVPrefix, DefaultDAVPrefix); err != nil {
		return nil, fmt.Errorf("%s: dav_prefix: %w", path, err)
	}
	if c.UIPrefix, err = normPrefix(c.UIPrefix, DefaultUIPrefix); err != nil {
		return nil, fmt.Errorf("%s: ui_prefix: %w", path, err)
	}
	if c.JMAPPrefix, err = normPrefix(c.JMAPPrefix, DefaultJMAPPrefix); err != nil {
		return nil, fmt.Errorf("%s: jmap_prefix: %w", path, err)
	}
	type prefix struct{ key, val string }
	prefixes := []prefix{{"dav_prefix", c.DAVPrefix}, {"ui_prefix", c.UIPrefix}}
	if c.JMAP {
		prefixes = append(prefixes, prefix{"jmap_prefix", c.JMAPPrefix})
	}
	for i, a := range prefixes {
		for _, b := range prefixes[i+1:] {
			if strings.HasPrefix(a.val, b.val) || strings.HasPrefix(b.val, a.val) {
				return nil, fmt.Errorf("%s: %s %q and %s %q must not overlap", path, a.key, a.val, b.key, b.val)
			}
		}
	}
	if o := c.OIDC; o != nil {
		if o.Issuer == "" || o.ClientID == "" {
			return nil, fmt.Errorf("%s: oidc needs issuer and client_id", path)
		}
		if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
			return nil, fmt.Errorf("%s: oidc needs base_url (for the redirect URI)", path)
		}
		if o.UsernameClaim == "" {
			o.UsernameClaim = "preferred_username"
		}
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "profile", "email"}
		}
		if o.Name == "" {
			o.Name = "single sign-on"
		}
	}
	c.BaseURL = strings.TrimSuffix(c.BaseURL, "/")
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(filepath.Dir(path), c.DataDir)
	}
	if c.Trusted, err = clientip.ParsePrefixes(c.TrustedProxies); err != nil {
		return nil, fmt.Errorf("%s: trusted_proxies: %w", path, err)
	}
	for name := range c.Users {
		if !ValidUserName(name) {
			return nil, fmt.Errorf("%s: invalid user name %q", path, name)
		}
	}
	for g, members := range c.Groups {
		if !ValidUserName(g) {
			return nil, fmt.Errorf("%s: invalid group name %q", path, g)
		}
		for _, m := range members {
			if _, ok := c.Users[m]; !ok {
				return nil, fmt.Errorf("%s: group %q: unknown user %q", path, g, m)
			}
		}
	}
	return &c, nil
}

// FileNames are tried in order by Find.
var FileNames = []string{"config.yaml", "config.yml", "config.json"}

// Find returns the first existing config file in dir.
func Find(dir string) (string, error) {
	for _, n := range FileNames {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no config file in %s (looked for %v)", dir, FileNames)
}

// Store holds the current config and reloads it on demand.
type Store struct {
	path string
	cur  atomic.Pointer[Config]
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	return s, s.Reload()
}

// Reload re-reads the file. A changed dav_prefix, ui_prefix, data_dir,
// jmap or jmap_prefix is rejected (the old config stays active) because
// those need a restart.
func (s *Store) Reload() error {
	c, err := Load(s.path)
	if err != nil {
		return err
	}
	if old := s.cur.Load(); old != nil && (old.DAVPrefix != c.DAVPrefix || old.UIPrefix != c.UIPrefix || old.DataDir != c.DataDir ||
		old.JMAP != c.JMAP || old.JMAPPrefix != c.JMAPPrefix || old.CalDAVPerUser != c.CalDAVPerUser) {
		return fmt.Errorf("%s: changing dav_prefix, ui_prefix, data_dir, jmap, jmap_prefix or caldav_per_user requires a restart", s.path)
	}
	s.cur.Store(c)
	return nil
}

// Password returns a user's config.yaml password hash ("" if none) and
// whether the user exists in the current config.
func (s *Store) Password(user string) (string, bool) {
	u, ok := s.Get().Users[user]
	return u.Password, ok
}

// UserByLogin maps an OIDC claim value to a configured user: by name
// first, then by email (case-insensitive).
func (c *Config) UserByLogin(v string) (string, bool) {
	if _, ok := c.Users[v]; ok {
		return v, true
	}
	for name, u := range c.Users {
		if u.Email != "" && strings.EqualFold(u.Email, v) {
			return name, true
		}
	}
	return "", false
}

// GroupsOf returns the groups user belongs to.
func (c *Config) GroupsOf(user string) []string {
	var out []string
	for g, members := range c.Groups {
		if slices.Contains(members, user) {
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return out
}

// DirectoryEnabled reports whether the directory address book is on.
func (c *Config) DirectoryEnabled() bool { return c.Directory == nil || *c.Directory }

func (s *Store) Get() *Config { return s.cur.Load() }
