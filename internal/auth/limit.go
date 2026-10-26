package auth

import (
	"net/netip"
	"sync"
	"time"
)

// Limiter throttles failed logins. Failures are counted per client IP and
// user name, and per IP alone (so one address can't try many names); IPv6
// addresses count per /64. A locked-out pair doesn't affect the same user
// logging in from elsewhere.
type Limiter struct {
	Window  time.Duration
	PerUser int // failures per IP+user within Window
	PerIP   int // failures per IP within Window
	// PerAccount failures for one user within Window, from anywhere,
	// pause that user's config.yaml password (not app passwords).
	PerAccount int
	mu         sync.Mutex
	fails      map[string]*failCount
	lastPruned time.Time
}

type failCount struct {
	n     int
	first time.Time
}

// NewLimiter returns a limiter with the defaults: 10 failures per IP and
// user, 50 per IP, and 100 per account, within 15 minutes.
func NewLimiter() *Limiter {
	return &Limiter{Window: 15 * time.Minute, PerUser: 10, PerIP: 50, PerAccount: 100, fails: map[string]*failCount{}}
}

func accountKey(user string) string { return "\x00account\x00" + ClipName(user) }

// FailAccount records a failed password for an existing user, from
// wherever it came.
func (l *Limiter) FailAccount(user string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count(accountKey(user), now).n++
}

// AccountPaused reports whether user's password logins are paused: too
// many failures across all addresses within Window.
func (l *Limiter) AccountPaused(user string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[accountKey(user)]
	return f != nil && now.Sub(f.first) <= l.Window && f.n >= l.PerAccount
}

// maxUserKey clips user names in keys: real ones are at most 64 bytes
// (config.ValidUserName), and an attacker's must not take more memory.
const maxUserKey = 64

func userKey(ip, user string) string { return ipKey(ip) + "\x00" + ClipName(user) }

// ipKey is the client part of a key: an IPv6 client usually has a whole
// /64, so its addresses count together.
func ipKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is6() || a.Is4In6() {
		return ip
	}
	p, _ := a.WithZone("").Prefix(64)
	return p.String()
}

// ClipName shortens a user name that a client sent to what a real one can
// be, for keys and log lines.
func ClipName(user string) string {
	if len(user) > maxUserKey {
		return user[:maxUserKey] + "..."
	}
	return user
}

func (l *Limiter) count(key string, now time.Time) *failCount {
	f := l.fails[key]
	if f == nil || now.Sub(f.first) > l.Window {
		f = &failCount{first: now}
		l.fails[key] = f
	}
	return f
}

// Allow reports whether a login attempt may proceed and, if not, how long
// the client should wait. It only looks: entries are made by Fail, so
// requests with ever new user names don't fill the map.
func (l *Limiter) Allow(ip, user string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	for _, k := range []struct {
		key   string
		limit int
	}{{userKey(ip, user), l.PerUser}, {ipKey(ip), l.PerIP}} {
		if f := l.fails[k.key]; f != nil && now.Sub(f.first) <= l.Window && f.n >= k.limit {
			return false, f.first.Add(l.Window).Sub(now).Round(time.Second) + time.Second
		}
	}
	return true, 0
}

// Fail records a failed attempt.
func (l *Limiter) Fail(ip, user string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count(userKey(ip, user), now).n++
	l.count(ipKey(ip), now).n++
}

// Success clears the IP+user counter.
func (l *Limiter) Success(ip, user string) {
	l.mu.Lock()
	delete(l.fails, userKey(ip, user))
	l.mu.Unlock()
}

func (l *Limiter) prune(now time.Time) {
	if now.Sub(l.lastPruned) < time.Minute {
		return
	}
	l.lastPruned = now
	for k, f := range l.fails {
		if now.Sub(f.first) > l.Window {
			delete(l.fails, k)
		}
	}
}
