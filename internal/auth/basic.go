package auth

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Basic authenticates HTTP Basic requests (DAV, JMAP) against app
// passwords and, as a fallback, the users' config.yaml passwords. Failed
// logins are rate-limited.
type Basic struct {
	password func(user string) (hash string, ok bool)
	appPass  *AppPasswords
	limit    *Limiter
	cache    *Cache
	log      *slog.Logger
	label    string
}

// NewBasic returns a Basic authenticator. password looks up a user's
// config.yaml password hash ("" if none) and reports whether the user
// exists. label names the protocol in log messages ("dav", "jmap").
func NewBasic(password func(user string) (string, bool), ap *AppPasswords, lim *Limiter, log *slog.Logger, label string) *Basic {
	return &Basic{password: password, appPass: ap, limit: lim, cache: NewCache(15 * time.Minute), log: log, label: label}
}

// Authenticate returns the user r authenticates as. If it returns false,
// it has already answered with 401 or 429.
func (b *Basic) Authenticate(w http.ResponseWriter, r *http.Request, ip string) (string, bool) {
	name, _, hasAuth := r.BasicAuth()
	if hasAuth {
		if ok, wait := b.limit.Allow(ip, name); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
			http.Error(w, "too many failed logins, try again later", http.StatusTooManyRequests)
			return "", false
		}
	}
	user, ok := b.check(r)
	if !ok {
		// Clients first ask without credentials; only wrong ones count.
		if hasAuth {
			b.limit.Fail(ip, name)
			b.log.Warn(b.label+" login failed", "user", ClipName(name), "ip", ip)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="ccdav", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	b.limit.Success(ip, user)
	return user, true
}

func (b *Basic) check(r *http.Request) (string, bool) {
	name, pw, ok := r.BasicAuth()
	if !ok {
		return "", false
	}
	hash, ok := b.password(name)
	if ok && b.appPass.Verify(name, pw) {
		return name, true
	}
	if !ok || hash == "" {
		// Burn comparable time so user enumeration via timing is harder.
		VerifyNone(pw)
		return "", false
	}
	// The config.yaml password stays as a fallback, unless guessed at
	// from too many places.
	if b.limit.AccountPaused(name) {
		VerifyNone(pw)
		b.log.Warn(b.label+" password logins paused after too many failures", "user", name)
		return "", false
	}
	if !b.cache.Verify(name, hash, pw) {
		b.limit.FailAccount(name)
		return "", false
	}
	return name, true
}
