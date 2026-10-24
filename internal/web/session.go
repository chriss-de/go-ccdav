package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sessions and the OIDC login state live in signed cookies; nothing is
// stored on the server. The key is generated once per data directory.

const (
	sessionCookie = "ccdav_session"
	oidcCookie    = "ccdav_oidc"
	sessionTTL    = 12 * time.Hour
	oidcTTL       = 10 * time.Minute
)

// LoadOrCreateKey reads the HMAC key from path, creating it if missing.
func LoadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) < 32 {
			return nil, fmt.Errorf("%s: key too short", path)
		}
		return b, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	b = make([]byte, 32)
	rand.Read(b)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

var b64 = base64.RawURLEncoding.Strict() // one valid encoding per value

func (u *UI) sign(purpose, payload string) string {
	m := hmac.New(sha256.New, u.key)
	m.Write([]byte(purpose + "\x00" + payload))
	return b64.EncodeToString([]byte(payload)) + "." + b64.EncodeToString(m.Sum(nil))
}

// verify returns the payload of a value made by sign, or false.
func (u *UI) verify(purpose, value string) (string, bool) {
	p, sig, ok := strings.Cut(value, ".")
	if !ok {
		return "", false
	}
	payload, err1 := b64.DecodeString(p)
	got, err2 := b64.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return "", false
	}
	m := hmac.New(sha256.New, u.key)
	m.Write([]byte(purpose + "\x00" + string(payload)))
	if !hmac.Equal(got, m.Sum(nil)) {
		return "", false
	}
	return string(payload), true
}

// timed payloads are "<unix expiry>|<fields...>".
func (u *UI) signTimed(purpose string, ttl time.Duration, fields ...string) string {
	exp := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	return u.sign(purpose, strings.Join(append([]string{exp}, fields...), "|"))
}

func (u *UI) verifyTimed(purpose, value string) ([]string, bool) {
	p, ok := u.verify(purpose, value)
	if !ok {
		return nil, false
	}
	fields := strings.Split(p, "|")
	exp, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return nil, false
	}
	return fields[1:], true
}

func (u *UI) secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") ||
		strings.HasPrefix(u.cfg.Get().BaseURL, "https://")
}

func (u *UI) setCookie(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: u.prefix, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: u.secure(r), SameSite: http.SameSiteLaxMode,
	})
}

func (u *UI) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: u.prefix, MaxAge: -1,
		HttpOnly: true, Secure: u.secure(r), SameSite: http.SameSiteLaxMode})
}

// session returns the logged-in user, or "". The user must still exist in
// the config.
func (u *UI) session(r *http.Request) (user, raw string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", ""
	}
	f, ok := u.verifyTimed("session", c.Value)
	if !ok || len(f) != 1 {
		return "", ""
	}
	if _, exists := u.cfg.Get().Users[f[0]]; !exists {
		return "", ""
	}
	return f[0], c.Value
}

func (u *UI) startSession(w http.ResponseWriter, r *http.Request, user string) {
	u.setCookie(w, r, sessionCookie, u.signTimed("session", sessionTTL, user), sessionTTL)
}

// csrfToken is bound to the session cookie value.
func (u *UI) csrfToken(rawSession string) string {
	m := hmac.New(sha256.New, u.key)
	m.Write([]byte("csrf\x00" + rawSession))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return b64.EncodeToString(b)
}
