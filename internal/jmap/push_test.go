package jmap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chriss-de/go-ccdav/internal/store"
)

type event struct {
	name string
	data map[string]any
}

// openStream connects to the event source and delivers its events.
func (e *env) openStream(user, query string) (<-chan event, func()) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/jmap/eventsource?"+query, nil)
	req.SetBasicAuth(user, "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		e.t.Fatalf("eventsource: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ch := make(chan event, 16)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		var ev event
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data)
			case line == "":
				ch <- ev
				ev = event{}
			}
		}
	}()
	return ch, func() { resp.Body.Close() }
}

func next(t *testing.T, ch <-chan event, within time.Duration) (event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(within):
		return event{}, false
	}
}

func changed(ev event) map[string]any {
	c, _ := ev.data["changed"].(map[string]any)
	return c
}

func fastPush(t *testing.T) {
	d, p := pushDelay, pingMin
	pushDelay, pingMin = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pushDelay, pingMin = d, p })
}

func TestPushStateChanges(t *testing.T) {
	fastPush(t)
	// The burst below must fit in one collection window, even with the
	// fsyncs of 50 writes on a slow CI disk under -race.
	pushDelay = time.Second
	e, book, _ := contactsEnv(t)
	ch, stop := e.openStream("alice", "types=*&closeafter=no&ping=0")
	defer stop()

	first, ok := next(t, ch, time.Second)
	own, _ := changed(first)[e.alice].(map[string]any)
	if !ok || first.name != "state" || first.data["@type"] != "StateChange" || own["ContactCard"] == nil || own["AddressBook"] == nil || own["Principal"] == nil {
		t.Fatalf("initial %+v", first)
	}

	// A DAV-side burst becomes one event, with only the type that changed.
	for i := range 50 {
		book.Put(fmt.Sprintf("burst-%d.vcf", i), []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:b%d\r\nFN:B\r\nEND:VCARD\r\n", i)), store.Cond{})
	}
	ev, ok := next(t, ch, 3*time.Second)
	acct, _ := changed(ev)[e.alice].(map[string]any)
	if !ok || len(acct) != 1 || acct["ContactCard"] == nil || acct["ContactCard"] == own["ContactCard"] {
		t.Fatalf("after puts %+v", ev)
	}
	if extra, ok := next(t, ch, 1500*time.Millisecond); ok {
		t.Fatalf("burst gave a second event %+v", extra)
	}
	// The pushed state is the one /get reports.
	r := e.api("alice", call("ContactCard/get", map[string]any{"accountId": e.alice, "ids": []string{}}, "0"))[0]
	if r.Args["state"] != acct["ContactCard"] {
		t.Fatalf("pushed %v, /get says %v", acct["ContactCard"], r.Args["state"])
	}
}

func TestPushSharedAccounts(t *testing.T) {
	fastPush(t)
	e := newEnv(t)
	shared, _ := e.st.CreateCollection("bob", store.AddressBooks, "team", store.Meta{})
	private, _ := e.st.CreateCollection("bob", store.AddressBooks, "private", store.Meta{})
	shared.SetShares([]store.Share{{User: "alice", Access: store.AccessRead}})

	ch, stop := e.openStream("alice", "types=ContactCard&closeafter=no&ping=0")
	defer stop()
	next(t, ch, time.Second) // initial

	private.Put("p.vcf", []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:p\r\nFN:P\r\nEND:VCARD\r\n"), store.Cond{})
	if ev, ok := next(t, ch, 300*time.Millisecond); ok {
		t.Fatalf("alice told about bob's private book: %+v", ev)
	}
	shared.Put("s.vcf", []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:s\r\nFN:S\r\nEND:VCARD\r\n"), store.Cond{})
	ev, ok := next(t, ch, time.Second)
	if acct, _ := changed(ev)[e.bob].(map[string]any); !ok || acct["ContactCard"] == nil || len(acct) != 1 {
		t.Fatalf("shared change %+v", ev)
	}
	// Types filter: an AddressBook change isn't sent to a ContactCard stream.
	e.st.UpdateSharePrefs("alice", shared, func(p *store.SharePrefs) { p.DisplayName = "Team" })
	if ev, ok := next(t, ch, 300*time.Millisecond); ok {
		t.Fatalf("filtered type sent: %+v", ev)
	}
}

func TestPushCloseAfterAndPing(t *testing.T) {
	fastPush(t)
	e, book, _ := contactsEnv(t)
	// Long polling: no initial event, one event on change, then EOF.
	ch, stop := e.openStream("alice", "types=*&closeafter=state&ping=0")
	defer stop()
	if ev, ok := next(t, ch, 200*time.Millisecond); ok {
		t.Fatalf("long poll sent the current state: %+v", ev)
	}
	book.Put("x.vcf", []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:x\r\nFN:X\r\nEND:VCARD\r\n"), store.Cond{})
	if ev, ok := next(t, ch, time.Second); !ok || ev.name != "state" {
		t.Fatalf("long poll: %+v", ev)
	}
	if _, ok := next(t, ch, time.Second); ok {
		t.Fatal("stream not closed after the state event")
	}

	// Pings are raised to the minimum and report the interval.
	ch2, stop2 := e.openStream("alice", "types=AddressBook&closeafter=no&ping=0")
	defer stop2()
	next(t, ch2, time.Second)
	ch3, stop3 := e.openStream("alice", "types=Nothing&closeafter=no&ping=1")
	defer stop3()
	ev, ok := next(t, ch3, 2*time.Second)
	if !ok || ev.name != "ping" || ev.data["interval"] != float64(1) {
		t.Fatalf("ping %+v", ev)
	}
}

func TestPushLimitsAndClose(t *testing.T) {
	fastPush(t)
	e := newEnv(t)
	var stops []func()
	for range maxStreams {
		_, stop := e.openStream("bob", "types=*&closeafter=no&ping=0")
		stops = append(stops, stop)
	}
	defer func() {
		for _, s := range stops {
			s()
		}
	}()
	req, _ := http.NewRequest("GET", e.srv.URL+"/jmap/eventsource?types=*", nil)
	req.SetBasicAuth("bob", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream %d: %d", maxStreams+1, resp.StatusCode)
	}

	ch, stop := e.openStream("alice", "types=*&closeafter=no&ping=0")
	defer stop()
	next(t, ch, time.Second)
	e.h.Close()
	if _, ok := next(t, ch, time.Second); ok {
		t.Fatal("stream survived Close")
	}
}
