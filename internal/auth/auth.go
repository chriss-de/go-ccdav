// Package auth implements PBKDF2-SHA256 password hashing and HTTP Basic auth.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	iterations = 600_000
	saltLen    = 16
	keyLen     = 32
	prefix     = "pbkdf2-sha256"
)

var b64 = base64.RawStdEncoding

// Hash returns "pbkdf2-sha256$<iter>$<salt>$<key>".
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", prefix, iterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

var errFormat = errors.New("auth: malformed hash")

// Verify checks password against a Hash result.
func Verify(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != prefix {
		return false, errFormat
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false, errFormat
	}
	salt, err1 := b64.DecodeString(parts[2])
	want, err2 := b64.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false, errFormat
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash has the cost of a real hash; no password matches it.
const dummyHash = "pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// VerifyNone spends the time of a Verify without a hash to check against,
// so that unknown users take as long to refuse as known ones.
func VerifyNone(password string) { Verify(dummyHash, password) }

// Cache remembers successful verifications for a while: DAV clients send
// Basic credentials on every request and PBKDF2 is deliberately slow.
type Cache struct {
	ttl time.Duration
	mu  sync.Mutex
	ok  map[[32]byte]time.Time
}

func NewCache(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl, ok: make(map[[32]byte]time.Time)}
}

// Verify is Verify with caching. The key covers the stored hash, so a
// password change in the config invalidates cached entries.
func (c *Cache) Verify(user, hash, password string) bool {
	k := sha256.Sum256([]byte(user + "\x00" + hash + "\x00" + password))
	now := time.Now()
	c.mu.Lock()
	exp, hit := c.ok[k]
	c.mu.Unlock()
	if hit && now.Before(exp) {
		return true
	}
	ok, err := Verify(hash, password)
	if err != nil || !ok {
		return false
	}
	c.mu.Lock()
	if len(c.ok) > 10_000 {
		for kk, e := range c.ok {
			if now.After(e) {
				delete(c.ok, kk)
			}
		}
	}
	c.ok[k] = now.Add(c.ttl)
	c.mu.Unlock()
	return true
}
