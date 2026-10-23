package config

import (
	"os"
	"path/filepath"
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
