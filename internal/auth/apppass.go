package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// AppPassword is a named, revocable password for DAV clients. Only a hash
// is stored. App passwords are 120 random bits, so a fast hash (SHA-256)
// is safe here, unlike for user-chosen passwords; it keeps checking
// several of them per request cheap.
type AppPassword struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hash     string    `json:"hash"` // hex SHA-256 of the password
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

// lastUsedEvery limits how often a successful login is written to disk.
const lastUsedEvery = time.Hour

// AppPasswords keeps data/users/<user>/app-passwords.json files.
type AppPasswords struct {
	root string // data directory

	mu    sync.Mutex
	cache map[string][]AppPassword
}

func NewAppPasswords(dataDir string) *AppPasswords {
	return &AppPasswords{root: dataDir, cache: map[string][]AppPassword{}}
}

func (a *AppPasswords) path(user string) string {
	return filepath.Join(a.root, "users", user, "app-passwords.json")
}

func (a *AppPasswords) load(user string) ([]AppPassword, error) {
	if l, ok := a.cache[user]; ok {
		return l, nil
	}
	var l []AppPassword
	b, err := os.ReadFile(a.path(user))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("%s: %w", a.path(user), err)
		}
	}
	a.cache[user] = l
	return l, nil
}

func (a *AppPasswords) save(user string, l []AppPassword) error {
	b, _ := json.MarshalIndent(l, "", "  ")
	p := a.path(user)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	a.cache[user] = l
	return nil
}

// List returns the user's app passwords (without secrets).
func (a *AppPasswords) List(user string) ([]AppPassword, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, err := a.load(user)
	return slices.Clone(l), err
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func hashAppPassword(pw string) string {
	sum := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(sum[:])
}

// Create makes a new app password and returns it in clear text; it can't
// be shown again later.
func (a *AppPasswords) Create(user, name string) (string, AppPassword, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", AppPassword{}, errors.New("auth: app password name must be 1-100 characters")
	}
	raw := make([]byte, 15) // 120 bits
	rand.Read(raw)
	enc := strings.ToLower(b32.EncodeToString(raw)) // 24 chars
	pw := enc[0:6] + "-" + enc[6:12] + "-" + enc[12:18] + "-" + enc[18:24]
	id := make([]byte, 6)
	rand.Read(id)
	ap := AppPassword{ID: hex.EncodeToString(id), Name: name, Hash: hashAppPassword(pw), Created: time.Now().UTC().Truncate(time.Second)}

	a.mu.Lock()
	defer a.mu.Unlock()
	l, err := a.load(user)
	if err != nil {
		return "", AppPassword{}, err
	}
	if len(l) >= 50 {
		return "", AppPassword{}, errors.New("auth: too many app passwords")
	}
	if err := a.save(user, append(slices.Clone(l), ap)); err != nil {
		return "", AppPassword{}, err
	}
	return pw, ap, nil
}

// Revoke deletes an app password by ID.
func (a *AppPasswords) Revoke(user, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, err := a.load(user)
	if err != nil {
		return err
	}
	n := slices.DeleteFunc(slices.Clone(l), func(ap AppPassword) bool { return ap.ID == id })
	if len(n) == len(l) {
		return errors.New("auth: no such app password")
	}
	return a.save(user, n)
}

// Verify reports whether pw is one of the user's app passwords and
// records when it was last used (persisted at most once an hour).
func (a *AppPasswords) Verify(user, pw string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, err := a.load(user)
	if err != nil {
		return false
	}
	h := []byte(hashAppPassword(strings.TrimSpace(pw)))
	match := -1
	for i, ap := range l {
		if subtle.ConstantTimeCompare(h, []byte(ap.Hash)) == 1 {
			match = i
		}
	}
	if match < 0 {
		return false
	}
	if now := time.Now().UTC(); now.Sub(l[match].LastUsed) >= lastUsedEvery {
		n := slices.Clone(l)
		n[match].LastUsed = now.Truncate(time.Second)
		if err := a.save(user, n); err != nil {
			a.cache[user] = n // keep it in memory at least
		}
	}
	return true
}
