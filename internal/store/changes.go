package store

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The change log (.changes.log) has one line per change, in token order:
//
//	<token> PUT <name>     object created or modified
//	<token> DEL <name>     object deleted
//	<token> META -         collection properties changed
//
// Compaction keeps only the newest line per name (and the newest META) and
// drops the oldest deletions beyond maxTombstones. The highest dropped
// token is kept as a "FLOOR <token>" first line: sync tokens below it can
// no longer be answered and are rejected, so the client does a full resync.

// ErrInvalidToken means a sync token is unknown or too old.
var ErrInvalidToken = errors.New("store: invalid sync token")

const (
	logName       = ".changes.log"
	maxTombstones = 1000
	compactSlack  = 1000
)

// Change is the newest change of one object since some token.
type Change struct {
	Name    string
	Token   uint64
	Deleted bool
}

type changeLog struct {
	floor   uint64
	meta    uint64   // token of the newest META line
	entries []Change // object changes in token order, names may repeat
}

func readLog(path string) (changeLog, error) {
	var l changeLog
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		fields := strings.SplitN(sc.Text(), " ", 3)
		if len(fields) == 2 && fields[0] == "FLOOR" {
			if n, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				l.floor = n
			}
			continue
		}
		if len(fields) != 3 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch fields[1] {
		case "META":
			l.meta = max(l.meta, n)
		case "PUT", "DEL":
			l.entries = append(l.entries, Change{Name: fields[2], Token: n, Deleted: fields[1] == "DEL"})
		}
	}
	return l, sc.Err()
}

// last is the highest token recorded in the log.
func (l changeLog) last() uint64 {
	tok := max(l.floor, l.meta)
	for _, e := range l.entries {
		tok = max(tok, e.Token)
	}
	return tok
}

// latest collapses entries to the newest change per name with Token >
// since, sorted by token.
func (l changeLog) latest(since uint64) []Change {
	newest := make(map[string]Change)
	for _, e := range l.entries {
		if e.Token > since {
			newest[e.Name] = e
		}
	}
	out := make([]Change, 0, len(newest))
	for _, c := range newest {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Change) int { return cmpUint(a.Token, b.Token) })
	return out
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func newSyncID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SyncID identifies this incarnation of the collection. It is part of the
// sync token, so tokens of a deleted and recreated collection are rejected.
func (c *Collection) SyncID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.meta.SyncID
}

// Changes returns the newest change per object since token since, and the
// current token. since == 0 is the initial sync and returns every object.
func (c *Collection) Changes(since uint64) ([]Change, uint64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if since == 0 {
		out := make([]Change, 0, len(c.objs))
		for _, o := range c.objs {
			out = append(out, Change{Name: o.Name, Token: c.token})
		}
		slices.SortFunc(out, func(a, b Change) int { return strings.Compare(a.Name, b.Name) })
		return out, c.token, nil
	}
	if since > c.token || since < c.floor {
		return nil, c.token, ErrInvalidToken
	}
	l, err := readLog(filepath.Join(c.dir, logName))
	if err != nil {
		return nil, c.token, err
	}
	return l.latest(since), c.token, nil
}

// logChange appends one line to the change log, before the change is
// made; the caller holds the write lock. The token moves only once the
// line is written.
func (c *Collection) logChange(op, name string) error {
	f, err := os.OpenFile(filepath.Join(c.dir, logName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	var size int64
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	_, err = fmt.Fprintf(f, "%d %s %s\n", c.token+1, op, name)
	if err != nil {
		f.Truncate(size) // no torn line for the next append to join
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	c.token++
	c.logLines++
	if c.logLines > c.compactAt() {
		return c.compact()
	}
	return nil
}

// repairLogTail cuts off a last line without "\n", left by a crash while
// appending: the next append would be glued to it and lost. It serves the
// account log too.
func repairLogTail(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || err == nil && (len(b) == 0 || b[len(b)-1] == '\n') {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Truncate(path, int64(bytes.LastIndexByte(b, '\n')+1))
}

// catchUpSlack: objects modified this long after the log's last write
// were changed without the server (a write logs first, then changes the
// file a moment later).
const catchUpSlack = 2 * time.Second

// catchUp logs, at load, what changed in the files without being logged:
// files edited, added or removed while the server was off. Objects the log
// doesn't have (or has as deleted) and objects modified after the log's
// last write become PUTs, logged objects that are gone DELs. Sync then
// reports them instead of missing them.
func (c *Collection) catchUp(l changeLog, mtimes map[string]time.Time, logPath string) error {
	var logged time.Time
	if fi, err := os.Stat(logPath); err == nil {
		logged = fi.ModTime()
	}
	newest := map[string]Change{}
	for _, e := range l.entries {
		newest[e.Name] = e
	}
	var puts, dels []string
	for name := range c.objs {
		if e, ok := newest[name]; !ok || e.Deleted || mtimes[name].After(logged.Add(catchUpSlack)) {
			puts = append(puts, name)
		}
	}
	for name, e := range newest {
		if _, ok := c.objs[name]; !ok && !e.Deleted {
			dels = append(dels, name)
		}
	}
	slices.Sort(puts)
	slices.Sort(dels)
	for _, name := range puts {
		if err := c.logChange("PUT", name); err != nil {
			return err
		}
	}
	for _, name := range dels {
		if err := c.logChange("DEL", name); err != nil {
			return err
		}
	}
	return nil
}

// compactAt is the log length that triggers compaction. Compaction keeps
// up to maxTombstones deletions: they count in, or a collection with few
// objects would compact on every write.
func (c *Collection) compactAt() int { return 2*len(c.objs) + maxTombstones + compactSlack }

// compact rewrites the log; the caller holds the write lock.
func (c *Collection) compact() error {
	path := filepath.Join(c.dir, logName)
	l, err := readLog(path)
	if err != nil {
		return err
	}
	kept := l.latest(0)
	floor := l.floor
	// kept is in token order; drop the oldest tombstones beyond the limit.
	var dels []uint64
	for _, e := range kept {
		if e.Deleted {
			dels = append(dels, e.Token)
		}
	}
	if extra := len(dels) - maxTombstones; extra > 0 {
		cutoff := dels[extra-1]
		floor = max(floor, cutoff)
		kept = slices.DeleteFunc(kept, func(e Change) bool { return e.Deleted && e.Token <= cutoff })
	}
	var sb strings.Builder
	if floor > 0 {
		fmt.Fprintf(&sb, "FLOOR %d\n", floor)
	}
	metaDone := l.meta == 0
	for _, e := range kept {
		if !metaDone && l.meta < e.Token {
			fmt.Fprintf(&sb, "%d META -\n", l.meta)
			metaDone = true
		}
		op := "PUT"
		if e.Deleted {
			op = "DEL"
		}
		fmt.Fprintf(&sb, "%d %s %s\n", e.Token, op, e.Name)
	}
	if !metaDone {
		fmt.Fprintf(&sb, "%d META -\n", l.meta)
	}
	if err := writeFileAtomic(path, []byte(sb.String())); err != nil {
		return err
	}
	c.floor, c.logLines = floor, strings.Count(sb.String(), "\n")
	return nil
}
