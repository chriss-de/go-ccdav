package auth

import (
	"sync"
	"time"
)

// Limiter throttles failed logins. Failures are counted per client IP and
// user name, and per IP alone (so one address can't try many names). A
// locked-out pair doesn't affect the same user logging in from elsewhere.
type Limiter struct {
	Window     time.Duration
	PerUser    int // failures per IP+user within Window
	PerIP      int // failures per IP within Window
	mu         sync.Mutex
	fails      map[string]*failCount
	lastPruned time.Time
}

type failCount struct {
	n     int
	first time.Time
}

// NewLimiter returns a limiter with the defaults: 10 failures per IP and
// user, 50 per IP, within 15 minutes.
func NewLimiter() *Limiter {
	return &Limiter{Window: 15 * time.Minute, PerUser: 10, PerIP: 50, fails: map[string]*failCount{}}
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
// the client should wait.
func (l *Limiter) Allow(ip, user string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	for _, k := range []struct {
		key   string
		limit int
	}{{ip + "\x00" + user, l.PerUser}, {ip, l.PerIP}} {
		if f := l.count(k.key, now); f.n >= k.limit {
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
	l.count(ip+"\x00"+user, now).n++
	l.count(ip, now).n++
}

// Success clears the IP+user counter.
func (l *Limiter) Success(ip, user string) {
	l.mu.Lock()
	delete(l.fails, ip+"\x00"+user)
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
