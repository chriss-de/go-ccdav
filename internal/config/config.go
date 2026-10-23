// Package config loads the server configuration (users, groups, SMTP and
// RSVP settings) from config.yaml in the data directory. config.json is
// accepted too: YAML is a superset of JSON. The live config is swapped
// atomically on Reload (SIGHUP).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"

	"go.yaml.in/yaml/v3"
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

type Config struct {
	// BaseURL is the public URL behind the reverse proxy, used for links in
	// emails (RSVP). DAV hrefs are host-relative and don't need it.
	BaseURL    string              `yaml:"base_url,omitempty"`
	Users      map[string]User     `yaml:"users"`
	Groups     map[string][]string `yaml:"groups,omitempty"`
	SMTP       SMTP                `yaml:"smtp,omitempty"`
	RSVPSecret string              `yaml:"rsvp_secret,omitempty"`
}

var userRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidUserName reports whether name is usable as a user (and path segment).
func ValidUserName(name string) bool { return userRe.MatchString(name) }

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
	for name := range c.Users {
		if !ValidUserName(name) {
			return nil, fmt.Errorf("%s: invalid user name %q", path, name)
		}
	}
	for g, members := range c.Groups {
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

func (s *Store) Reload() error {
	c, err := Load(s.path)
	if err != nil {
		return err
	}
	s.cur.Store(c)
	return nil
}

func (s *Store) Get() *Config { return s.cur.Load() }
