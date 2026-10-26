package jmap

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chriss-de/go-ccdav/internal/store"
)

// Push over EventSource (RFC 8620 §7.3). The store reports which owner's
// data changed; after pushDelay the hub works out, for every open
// stream, which states of the accounts its user can see moved on, and
// sends one StateChange. Bursts (an import, a DAV sync) become one event.

var (
	pushDelay   = time.Second      // changes are collected this long
	pingMin     = 30 * time.Second // smallest ping interval granted
	maxStreams  = 4                // per user
	pingMaximum = time.Hour
)

// pushTypes are the data types with a state, and where it comes from.
var pushTypes = map[string]struct {
	kind  store.Kind
	level store.Level
}{
	"AddressBook":   {store.AddressBooks, store.LevelCollections},
	"ContactCard":   {store.AddressBooks, store.LevelObjects},
	"Calendar":      {store.Calendars, store.LevelCollections},
	"CalendarEvent": {store.Calendars, store.LevelObjects},
}

type hub struct {
	h       *Handler
	mu      sync.Mutex
	streams map[*stream]bool
	dirty   map[string]bool // owners whose data changed
	all     bool            // everything may have changed (config reload)
	wake    chan struct{}
	done    chan struct{}
	closed  bool
	delay   time.Duration // pushDelay when created
	pingMin time.Duration
}

type stream struct {
	user  string
	types map[string]bool // nil: all

	upd  sync.Mutex                   // one update at a time
	sent map[string]map[string]string // accountId -> type -> state, guarded by upd

	mu      sync.Mutex
	pending map[string]map[string]string // accountId -> type -> state, not yet sent
	signal  chan struct{}
}

func newHub(h *Handler) *hub {
	hb := &hub{h: h, streams: map[*stream]bool{}, dirty: map[string]bool{}, wake: make(chan struct{}, 1), done: make(chan struct{}),
		delay: pushDelay, pingMin: pingMin}
	go hb.loop()
	return hb
}

// mark records that owner's data changed. It never blocks.
func (hb *hub) mark(owner string) {
	hb.mu.Lock()
	hb.dirty[owner] = true
	hb.mu.Unlock()
	select {
	case hb.wake <- struct{}{}:
	default:
	}
}

// markAll is mark for everyone (a config reload changes groups and with
// them who sees which account).
func (hb *hub) markAll() {
	hb.mu.Lock()
	hb.all = true
	hb.mu.Unlock()
	select {
	case hb.wake <- struct{}{}:
	default:
	}
}

func (hb *hub) loop() {
	for {
		select {
		case <-hb.done:
			return
		case <-hb.wake:
		}
		select {
		case <-hb.done:
			return
		case <-time.After(hb.delay):
		}
		hb.mu.Lock()
		dirty, all := hb.dirty, hb.all
		hb.dirty, hb.all = map[string]bool{}, false
		streams := slices.Collect(maps.Keys(hb.streams))
		hb.mu.Unlock()
		for _, s := range streams {
			hb.update(s, func(owner string) bool { return all || dirty[owner] })
		}
	}
}

// update computes the current states of the accounts s's user sees whose
// owner affected says changed, and queues those that differ from what
// was sent.
func (hb *hub) update(s *stream, affected func(owner string) bool) {
	s.upd.Lock()
	defer s.upd.Unlock()
	changed := map[string]map[string]string{}
	for id, a := range hb.h.accounts(s.user) {
		if !affected(a.owner) {
			continue
		}
		for typ, state := range hb.h.states(s.user, id, a) {
			if s.types != nil && !s.types[typ] {
				continue
			}
			if s.sent[id][typ] == state {
				continue
			}
			if changed[id] == nil {
				changed[id] = map[string]string{}
			}
			changed[id][typ] = state
			if s.sent[id] == nil {
				s.sent[id] = map[string]string{}
			}
			s.sent[id][typ] = state
		}
	}
	if len(changed) == 0 {
		return
	}
	s.mu.Lock()
	for id, types := range changed {
		if s.pending[id] == nil {
			s.pending[id] = map[string]string{}
		}
		maps.Copy(s.pending[id], types)
	}
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// states returns the state of every push type in one account.
func (h *Handler) states(user, accountID string, a account) map[string]string {
	out := map[string]string{}
	views := map[store.Kind]*View{}
	for typ, src := range pushTypes {
		v, ok := views[src.kind]
		if !ok {
			var err error
			if v, err = h.view(user, accountID, src.kind); err != nil {
				continue
			}
			views[src.kind] = v
		}
		out[typ] = v.State(src.level)
	}
	if a.personal {
		out["Principal"] = principalState(h.cfg.Get())
	}
	return out
}

func (hb *hub) add(s *stream) bool {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if hb.closed {
		return false
	}
	hb.streams[s] = true
	return true
}

func (hb *hub) remove(s *stream) {
	hb.mu.Lock()
	delete(hb.streams, s)
	hb.mu.Unlock()
}

func (hb *hub) close() {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if !hb.closed {
		hb.closed = true
		close(hb.done)
	}
}

// Close ends all push streams; for server shutdown.
func (h *Handler) Close() { h.hub.close() }

// Changed tells push that anything may have changed (after a config
// reload).
func (h *Handler) Changed() { h.hub.markAll() }

// eventSource serves <prefix>eventsource?types=&closeafter=&ping=.
func (h *Handler) eventSource(w http.ResponseWriter, r *http.Request, user string) {
	q := r.URL.Query()
	s := &stream{user: user, sent: map[string]map[string]string{}, pending: map[string]map[string]string{}, signal: make(chan struct{}, 1)}
	if t := q.Get("types"); t != "*" && t != "" {
		s.types = map[string]bool{}
		for name := range strings.SplitSeq(t, ",") {
			s.types[strings.TrimSpace(name)] = true
		}
	}
	closeAfter := q.Get("closeafter") == "state"
	var ping time.Duration
	if p := q.Get("ping"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			http.Error(w, "bad ping", http.StatusBadRequest)
			return
		}
		if n > 0 {
			ping = min(max(time.Duration(n)*time.Second, h.hub.pingMin), pingMaximum)
		}
	}
	release, ok := slot(&h.streams, user, maxStreams)
	if !ok {
		http.Error(w, "too many push connections", http.StatusTooManyRequests)
		return
	}
	defer release()
	if !h.hub.add(s) {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer h.hub.remove(s)

	// The stream outlives the server's write timeout.
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	// The current states first, so a change between the client's last
	// /get and connecting isn't missed. Long polling waits for news.
	h.hub.update(s, func(string) bool { return true })
	if closeAfter {
		s.mu.Lock()
		s.pending = map[string]map[string]string{}
		s.mu.Unlock()
		select {
		case <-s.signal:
		default:
		}
	}

	var tick <-chan time.Time
	if ping > 0 {
		t := time.NewTicker(ping)
		defer t.Stop()
		tick = t.C
	}
	write := func(event string, data any) bool {
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.hub.done:
			return
		case <-tick:
			if !write("ping", map[string]any{"interval": int(ping / time.Second)}) {
				return
			}
		case <-s.signal:
			s.mu.Lock()
			changed := s.pending
			s.pending = map[string]map[string]string{}
			s.mu.Unlock()
			if len(changed) == 0 {
				continue
			}
			if !write("state", map[string]any{"@type": "StateChange", "changed": changed}) || closeAfter {
				return
			}
		}
	}
}
